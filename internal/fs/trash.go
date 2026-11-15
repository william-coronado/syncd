package fs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// TrashDirName is the trash directory's name inside the state directory.
const TrashDirName = "trash"

// trashDayLayout names the per-day directories inside the trash; PruneTrash
// parses it back to decide what has expired.
const trashDayLayout = "2006-01-02"

// Trash turns a sync-driven delete of a file on a local disk into a move
// to Dir/<day>/<root name>-<root id>/<relative path>, so a delete
// propagated from the other side (or from a mirror) can be undone by hand.
// A file on a network filesystem is removed outright instead: moving it to
// a local trash would mean downloading everything that gets deleted, and
// the server's own recycle bin (Synology's #recycle) is the recovery path
// there.
//
// The trash is shared by every syncd process using the same state
// directory, so each root gets its own folder (two roots may share a base
// name) and each destination is reserved atomically before a file is moved
// onto it.
//
// A nil *Trash removes files outright.
type Trash struct {
	Dir string

	network sync.Map // directory -> bool: is it on a network filesystem
}

// NewTrash returns a Trash rooted at dir. The directory is created on first
// use, not here.
func NewTrash(dir string) *Trash { return &Trash{Dir: dir} }

// trashNow and isNetworkFS are indirected for tests.
var (
	trashNow    = time.Now
	isNetworkFS = isNetworkFilesystem
)

// Remove moves a regular file to local trash or deletes it on a network mount.
func (t *Trash) Remove(root, rel string) error {
	source, err := OpenTree(root)
	if err != nil {
		return err
	}
	defer source.Close()
	return t.RemoveFrom(source, rel, nil)
}

// RemoveFrom claims the actual entry before checking or copying it. A writer
// that replaces the original name after the claim keeps its replacement.
func (t *Trash) RemoveFrom(source *Tree, rel string, check EntryCheck) error {
	parent, leaf, err := source.parent(rel, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	info, err := statAt(parent, leaf)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to remove non-regular entry: %s", rel)
	}
	claimed, finish, err := claimEntry(parent, leaf)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	current, err := claimed.Lstat(leaf)
	if err != nil {
		return finish(err)
	}
	if !SameEntry(info, current) || !current.Mode().IsRegular() {
		return finish(fmt.Errorf("entry changed before removal: %s", rel))
	}
	if check != nil {
		if err := check(claimed, leaf); err != nil {
			return finish(err)
		}
	}
	if t == nil || t.onNetwork(parent.Name()) {
		return finish(claimed.Remove(leaf, false))
	}
	dst, name, err := t.reserveEntry(source.Name(), rel)
	if err != nil {
		return finish(fmt.Errorf("reserve trash entry: %w", err))
	}
	defer dst.Close()
	err = moveIntoTrash(claimed, leaf, dst, name, false)
	if errors.Is(err, unix.EXDEV) {
		// The original name is already detached. Confirm that an open writer
		// did not change the held file while we copied it across devices.
		var original string
		original, err = claimed.Hash(leaf)
		if err == nil {
			err = CopyBetween(claimed, leaf, dst, name)
		}
		if err == nil {
			var copied, latest string
			copied, err = dst.Hash(name)
			if err == nil {
				latest, err = claimed.Hash(leaf)
			}
			if err == nil && (latest != original || copied != original) {
				err = fmt.Errorf("source changed during trash copy: %s", rel)
			}
		}
		if err == nil && check != nil {
			err = check(claimed, leaf)
		}
		if err == nil {
			err = claimed.Remove(leaf, false)
		}
	}
	if err != nil {
		_ = dst.Remove(name, false)
		return finish(fmt.Errorf("move to trash: %w", err))
	}
	return finish(nil)
}

// moveIntoTrash permits tests to simulate a separate device.
var moveIntoTrash = RenameBetween

func (t *Trash) Preserve(root, rel string) error {
	if t == nil {
		return nil
	}
	source, err := OpenTree(root)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Lstat(filepath.FromSlash(rel))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	return t.PreserveFrom(source, rel, root, rel)
}

// PreserveFrom copies an already claimed entry under its original trash name.
func (t *Trash) PreserveFrom(source *Tree, name, originalRoot, originalRel string) error {
	if t == nil {
		return nil
	}
	dst, leaf, err := t.reserveEntry(originalRoot, originalRel)
	if err != nil {
		return err
	}
	defer dst.Close()
	if err := CopyBetween(source, filepath.FromSlash(name), dst, leaf); err != nil {
		_ = dst.Remove(leaf, false)
		return fmt.Errorf("keep a copy in trash: %w", err)
	}
	return nil
}

func (t *Trash) onNetwork(dir string) bool {
	if v, ok := t.network.Load(dir); ok {
		return v.(bool)
	}
	v := isNetworkFS(dir)
	t.network.Store(dir, v)
	return v
}

// RootFolder is the per-root folder name inside a day's trash: the root's
// base name for readability, plus a short hash of its full path so two
// roots that share a base name never share (or block) each other's trash.
func RootFolder(root string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return filepath.Base(root) + "-" + hex.EncodeToString(sum[:4])
}

// reserveEntry returns the pinned parent that owns the exclusive reservation.
// Copying and failure cleanup both use that same directory descriptor.
func (t *Trash) reserveEntry(root, rel string) (*Tree, string, error) {
	if !filepath.IsLocal(rel) || filepath.Clean(rel) == "." {
		return nil, "", fmt.Errorf("invalid trash path: %q", rel)
	}
	stateDir := filepath.Dir(t.Dir)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, "", err
	}
	state, err := OpenTree(stateDir)
	if err != nil {
		return nil, "", err
	}
	defer state.Close()
	now := trashNow()
	base := filepath.Join(filepath.Base(t.Dir), now.Format(trashDayLayout), RootFolder(root), filepath.FromSlash(rel))
	parent, leaf, err := state.parent(base, 0o700)
	if err != nil {
		return nil, "", err
	}
	for attempt := 0; attempt < 1000; attempt++ {
		candidate := leaf
		if attempt == 1 {
			candidate += "." + now.Format("150405")
		} else if attempt > 1 {
			candidate += fmt.Sprintf(".%s-%d", now.Format("150405"), attempt)
		}
		f, err := openAt(parent, candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if err := f.Close(); err != nil {
				_ = unix.Unlinkat(fd(parent), candidate, 0)
				parent.Close()
				return nil, "", err
			}
			return &Tree{dir: parent}, candidate, nil
		}
		if !os.IsExist(err) {
			parent.Close()
			return nil, "", err
		}
	}
	parent.Close()
	return nil, "", fmt.Errorf("no free trash name for %s", base)
}

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// PruneTrash deletes the per-day trash directories under dir that are more
// than keepDays days old, returning how many it removed. keepDays <= 0
// keeps everything. Entries that are not day directories are left alone.
func PruneTrash(dir string, keepDays int) (int, error) {
	if keepDays <= 0 {
		return 0, nil
	}
	tree, err := OpenTree(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer tree.Close()
	entries, err := tree.ReadDir(".")
	if err != nil {
		return 0, err
	}
	now := trashNow()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	cutoff := today.AddDate(0, 0, -keepDays)
	var removed int
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		day, err := time.Parse(trashDayLayout, e.Name())
		if err != nil || !day.Before(cutoff) {
			continue
		}
		if err := pruneEntry(tree.dir, e.Name()); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

// pruneEntry descends only through no-follow directory descriptors. A
// symlink inside expired trash is removed as an entry, never traversed.
func pruneEntry(parent *os.File, name string) error {
	info, err := statAt(parent, name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return unix.Unlinkat(fd(parent), name, 0)
	}
	dir, err := openAt(parent, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, child := range names {
		if err := pruneEntry(dir, child); err != nil {
			return err
		}
	}
	return unix.Unlinkat(fd(parent), name, unix.AT_REMOVEDIR)
}
