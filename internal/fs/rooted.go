package fs

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

var ErrSymlink = errors.New("symlink not supported")

// Tree pins a sync root for the entire scan and execution cycle.
type Tree struct {
	dir  *os.File
	name string
	// borrowed: dir belongs to someone else (a claim's parent descriptor);
	// Close leaves it open.
	borrowed bool
}

func OpenTree(name string) (*Tree, error) {
	// Allow configured ancestor aliases (for example /var on macOS) at
	// startup. Open the resolved components without following links.
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("sync root is not a real directory: %s", name)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	base, err := os.Open("/")
	if err != nil {
		return nil, err
	}
	defer base.Close()
	dir, err := walkDirectory(base, strings.TrimPrefix(real, "/"), 0)
	if err != nil {
		return nil, err
	}
	opened, err := dir.Stat()
	if err != nil || !os.SameFile(info, opened) {
		dir.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("sync root changed while opening: %s", name)
	}
	return &Tree{dir: dir, name: abs}, nil
}

func (t *Tree) Close() error {
	if t.borrowed {
		return nil
	}
	return t.dir.Close()
}
func (t *Tree) Name() string {
	if t.name != "" {
		return t.name
	}
	return t.dir.Name()
}
func fd(f *os.File) int { return int(f.Fd()) }

func openAt(dir *os.File, name string, flags int, mode os.FileMode) (*os.File, error) {
	n, err := unix.Openat(fd(dir), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode))
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: filepath.Join(dir.Name(), name), Err: err}
	}
	return os.NewFile(uintptr(n), filepath.Join(dir.Name(), name)), nil
}

func walkDirectory(root *os.File, rel string, create os.FileMode) (*os.File, error) {
	dir, err := openAt(root, ".", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	if rel == "" || rel == "." {
		return dir, nil
	}
	for _, component := range strings.Split(rel, "/") {
		next, err := openChildDir(dir, component)
		if os.IsNotExist(err) {
			if actual := normalizationVariant(dir, component); actual != component {
				next, err = openChildDir(dir, actual)
			}
		}
		if os.IsNotExist(err) && create != 0 {
			if mkErr := unix.Mkdirat(fd(dir), component, uint32(create)); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
				dir.Close()
				return nil, mkErr
			}
			next, err = openChildDir(dir, component)
		}
		dir.Close()
		if err != nil {
			return nil, err
		}
		dir = next
	}
	return dir, nil
}

// openChildDir opens one directory component beneath dir without following
// a symlink, reporting a symlink there as ErrSymlink.
func openChildDir(dir *os.File, component string) (*os.File, error) {
	next, err := openAt(dir, component, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		var st unix.Stat_t
		if unix.Fstatat(fd(dir), component, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			err = fmt.Errorf("%w: %s", ErrSymlink, filepath.Join(dir.Name(), component))
		}
	}
	return next, err
}

func (t *Tree) directory(rel string, create os.FileMode) (*os.File, error) {
	if !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("path escapes sync root: %q", rel)
	}
	return walkDirectory(t.dir, filepath.Clean(rel), create)
}

func (t *Tree) parent(rel string, create os.FileMode) (*os.File, string, error) {
	if !filepath.IsLocal(rel) {
		return nil, "", fmt.Errorf("path escapes sync root: %q", rel)
	}
	rel = filepath.Clean(rel)
	d, err := t.directory(filepath.Dir(rel), create)
	if err != nil {
		return nil, filepath.Base(rel), err
	}
	return d, resolveLeaf(d, filepath.Base(rel)), nil
}

// resolveLeaf returns the name under which dir actually holds want: want
// itself when it exists (or cannot have another spelling), else its single
// Unicode-normalization variant, if dir holds exactly one.
//
// Paths reach Tree in the scanner's NFC form, but Linux and Samba store
// names byte for byte, so a file named in NFD (as files from a Mac often
// are) exists only under its own bytes. Looking it up by the NFC key read
// it as deleted: a local edit planned a remote delete, and remote edits and
// deletes never reached it. Resolving here, in the one place every Tree
// operation passes through, keeps the on-disk spelling for reads, replaces
// and deletes alike. A name with no other spelling (see mayHaveVariants)
// costs no extra lookup.
func resolveLeaf(dir *os.File, want string) string {
	if !mayHaveVariants(want) {
		return want
	}
	var st unix.Stat_t
	if err := unix.Fstatat(fd(dir), want, &st, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		return want
	}
	return normalizationVariant(dir, want)
}

// normalizationVariant returns the spelling under which dir stores want:
// want itself if dir holds those exact bytes, else the one entry that is
// the same name up to Unicode normalization - or want if there is none, or
// several (the collision the scanner refuses to sync; picking one would act
// on a file the scan never attributed to this path).
//
// Finding want by lookup is not enough to know its spelling: APFS matches
// names regardless of normalization, so the NFC key opens an NFD-stored
// file there while the directory still lists it as NFD.
func normalizationVariant(dir *os.File, want string) string {
	if !mayHaveVariants(want) {
		return want
	}
	// A private descriptor: reading names moves the directory offset, and
	// dir may be read again by its owner.
	listing, err := openAt(dir, ".", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return want
	}
	defer listing.Close()
	names, err := listing.Readdirnames(-1)
	if err != nil {
		return want
	}
	key := norm.NFC.String(want)
	match := ""
	for _, name := range names {
		if name == want {
			return want
		}
		// An ASCII name is its own NFC form, so it can only equal key by
		// being want itself.
		if isASCII(name) || norm.NFC.String(name) != key {
			continue
		}
		if match != "" {
			return want
		}
		match = name
	}
	if match == "" {
		return want
	}
	return match
}

// storedSpelling returns the spelling under which dir stores name, an entry
// that a lookup of name reaches. Only a filesystem that matches names
// regardless of normalization (APFS) can store it differently; there, both
// canonical spellings reach the same entry. Two stats tell the cases apart,
// so a byte-exact filesystem (Linux, Samba) never pays for a directory
// read - which, once per claimed file, made claiming a whole directory of
// accented names quadratic.
func storedSpelling(dir *os.File, name string) string {
	if isASCII(name) {
		return name
	}
	other := norm.NFD.String(name)
	if other == name {
		other = norm.NFC.String(name)
	}
	if other == name {
		return name // no other canonical spelling
	}
	var a, b unix.Stat_t
	if unix.Fstatat(fd(dir), name, &a, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		unix.Fstatat(fd(dir), other, &b, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		a.Dev != b.Dev || a.Ino != b.Ino {
		return name // byte-exact: the lookup that reached name was exact
	}
	return normalizationVariant(dir, name)
}

// mayHaveVariants reports whether another spelling of name can normalize to
// it. Any non-ASCII byte can; of ASCII, only ';', '`' and 'K', the NFC forms
// of U+037E, U+1FEF and U+212A (KELVIN SIGN) - the only code points whose
// NFC form contains ASCII. Every other ASCII name is the only spelling of
// itself, so the common case pays no lookup.
func mayHaveVariants(name string) bool {
	return !isASCII(name) || strings.ContainsAny(name, ";`K")
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func (t *Tree) Open(name string) (iofs.File, error) {
	d, leaf, err := t.parent(name, 0)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return openAt(d, leaf, unix.O_RDONLY|unix.O_NONBLOCK, 0)
}

// Stat implements fs.StatFS so the walk never stats a symlink's target.
func (t *Tree) Stat(rel string) (os.FileInfo, error) {
	d, leaf, err := t.parent(rel, 0)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return statAt(d, leaf)
}

func (t *Tree) Lstat(rel string) (os.FileInfo, error) {
	info, err := t.Stat(rel)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s", ErrSymlink, rel)
	}
	return info, err
}

// ReadDir builds entries from the pinned directory descriptor, including on
// filesystems whose directory entries do not report their type.
func (t *Tree) ReadDir(rel string) ([]iofs.DirEntry, error) {
	d, err := t.directory(rel, 0)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return readDirAt(d)
}

func readDirAt(d *os.File) ([]iofs.DirEntry, error) {
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	entries := make([]iofs.DirEntry, 0, len(names))
	for _, name := range names {
		info, err := statAt(d, name)
		if err != nil {
			entries = append(entries, unreadableEntry{name, err})
			continue
		}
		entries = append(entries, iofs.FileInfoToDirEntry(info))
	}
	return entries, nil
}

type unreadableEntry struct {
	name string
	err  error
}

func (e unreadableEntry) Name() string               { return e.name }
func (e unreadableEntry) IsDir() bool                { return false }
func (e unreadableEntry) Type() os.FileMode          { return 0 }
func (e unreadableEntry) Info() (os.FileInfo, error) { return nil, e.err }

type atInfo struct {
	name string
	stat unix.Stat_t
}

func (i atInfo) Name() string       { return i.name }
func (i atInfo) Size() int64        { return i.stat.Size }
func (i atInfo) ModTime() time.Time { return time.Unix(i.stat.Mtim.Unix()) }
func (i atInfo) IsDir() bool        { return i.Mode().IsDir() }
func (i atInfo) Sys() any           { return &i.stat }
func (i atInfo) Mode() os.FileMode {
	mode := os.FileMode(i.stat.Mode & 0777)
	switch i.stat.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return mode
	case unix.S_IFDIR:
		return mode | os.ModeDir
	case unix.S_IFLNK:
		return mode | os.ModeSymlink
	default:
		return mode | os.ModeIrregular
	}
}

func SameEntry(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return false
	}
	x, xok := a.Sys().(*unix.Stat_t)
	y, yok := b.Sys().(*unix.Stat_t)
	if xok && yok {
		return x.Dev == y.Dev && x.Ino == y.Ino
	}
	return os.SameFile(a, b)
}

// SameSnapshot reports whether two observations of one entry agree on
// identity, size and full-resolution modification time.
func SameSnapshot(a, b os.FileInfo) bool {
	return SameEntry(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func statAt(d *os.File, leaf string) (os.FileInfo, error) {
	info := atInfo{name: leaf}
	if err := unix.Fstatat(fd(d), leaf, &info.stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, &os.PathError{Op: "fstatat", Path: filepath.Join(d.Name(), leaf), Err: err}
	}
	return info, nil
}

func openRegularAt(d *os.File, leaf string) (*os.File, error) {
	f, err := openAt(d, leaf, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("not a regular file: %s", leaf)
	}
	return f, nil
}

func (t *Tree) openRegular(rel string) (*os.File, error) {
	d, leaf, err := t.parent(rel, 0)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return openRegularAt(d, leaf)
}

func (t *Tree) Remove(rel string, directory bool) error {
	d, leaf, err := t.parent(rel, 0)
	if err != nil {
		return err
	}
	defer d.Close()
	info, err := statAt(d, leaf)
	if err != nil {
		return err
	}
	if directory && info.IsDir() {
		return unix.Unlinkat(fd(d), leaf, unix.AT_REMOVEDIR)
	}
	if !directory && info.Mode().IsRegular() {
		return unix.Unlinkat(fd(d), leaf, 0)
	}
	return fmt.Errorf("unexpected file type at %s", rel)
}

func (t *Tree) MkdirAll(rel string) error {
	d, err := t.directory(rel, 0o755)
	if err != nil {
		return err
	}
	return d.Close()
}

func RenameBetween(s *Tree, src string, d *Tree, dst string, exclusive bool) error {
	sp, sl, err := s.parent(src, 0)
	if err != nil {
		return err
	}
	defer sp.Close()
	dp, dl, err := d.parent(dst, 0)
	if err != nil {
		return err
	}
	defer dp.Close()
	if exclusive {
		return publishExclusive(sp, sl, dp, dl)
	}
	return unix.Renameat(fd(sp), sl, fd(dp), dl)
}

func RemoveInRoot(root, rel string) error {
	t, err := OpenTree(root)
	if err != nil {
		return err
	}
	defer t.Close()
	return t.Remove(rel, false)
}

func MkdirInRoot(root, rel string) error {
	t, err := OpenTree(root)
	if err != nil {
		return err
	}
	defer t.Close()
	return t.MkdirAll(rel)
}

func RenameInRoots(srcRoot, src, dstRoot, dst string) error {
	s, err := OpenTree(srcRoot)
	if err != nil {
		return err
	}
	defer s.Close()
	d, err := OpenTree(dstRoot)
	if err != nil {
		return err
	}
	defer d.Close()
	if _, err := s.Lstat(src); err != nil {
		return err
	}
	if _, err := d.Lstat(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return RenameBetween(s, src, d, dst, false)
}

// verifyParent detects a rename or substitution before publishing. The
// following operation still uses the original directory descriptor.
func (t *Tree) verifyParent(rel string, expected *os.File) error {
	current, _, err := t.parent(rel, 0)
	if err != nil {
		return err
	}
	defer current.Close()
	before, err := expected.Stat()
	if err != nil {
		return err
	}
	after, err := current.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return fmt.Errorf("parent directory changed: %s", rel)
	}
	return nil
}
