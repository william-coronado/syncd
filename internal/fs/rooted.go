package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var ErrSymlink = errors.New("symlink not supported")

// openSyncRoot rejects a replaced root itself, not only its descendants.
// Comparing identities after opening closes the lstat/open replacement window.
func openSyncRoot(name string) (*os.Root, error) {
	before, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("sync root is not a real directory: %s", name)
	}
	r, err := os.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := r.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		r.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("sync root changed while opening: %s", name)
	}
	return r, nil
}

// CheckPath rejects symlinks anywhere below root, including the final
// component. Missing components are allowed for new destinations. Operations
// must still use os.Root: this check alone cannot prevent a concurrent swap.
func CheckPath(root, rel string) error {
	r, err := openSyncRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	return checkPath(r, rel)
}

func checkPath(r *os.Root, rel string) error {
	if !filepath.IsLocal(rel) {
		return fmt.Errorf("path escapes sync root: %q", rel)
	}
	p := ""
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		p = filepath.Join(p, part)
		info, err := r.Lstat(p)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlink, p)
		}
	}
	return nil
}

// LstatInRoot checks ancestors as well as the final path. In particular a
// scoped scan must not follow a symlink in a parent it did not walk.
func LstatInRoot(root, rel string) (os.FileInfo, error) {
	r, err := openSyncRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if err := checkPath(r, rel); err != nil {
		return nil, err
	}
	return r.Lstat(rel)
}

func openRegularInRoot(r *os.Root, rel string) (*os.File, error) {
	if err := checkPath(r, rel); err != nil {
		return nil, err
	}
	f, err := r.OpenFile(rel, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("not a regular file: %s", rel)
	}
	return f, nil
}

func RemoveInRoot(root, rel string) error {
	r, err := openSyncRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := checkPath(r, filepath.Dir(rel)); err != nil {
		return err
	}
	return r.Remove(rel)
}

func MkdirInRoot(root, rel string) error {
	r, err := openSyncRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := checkPath(r, rel); err != nil {
		return err
	}
	return r.MkdirAll(rel, 0o755)
}

// RenameInRoots pins both parent directories before renameat, including for
// moves into the trash on a different root. No absolute path is re-resolved
// by the rename, so a symlink swapped into a parent cannot redirect it.
func RenameInRoots(srcRoot, srcRel, dstRoot, dstRel string) error {
	s, err := openSyncRoot(srcRoot)
	if err != nil {
		return err
	}
	defer s.Close()
	d, err := openSyncRoot(dstRoot)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := checkPath(s, srcRel); err != nil {
		return err
	}
	if err := checkPath(d, dstRel); err != nil {
		return err
	}
	sp, err := s.Open(filepath.Dir(srcRel))
	if err != nil {
		return err
	}
	defer sp.Close()
	dp, err := d.Open(filepath.Dir(dstRel))
	if err != nil {
		return err
	}
	defer dp.Close()
	return unix.Renameat(int(sp.Fd()), filepath.Base(srcRel), int(dp.Fd()), filepath.Base(dstRel))
}

func mustRelative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		// Rooted operations reject the absolute path if it cannot be made relative.
		return path
	}
	return rel
}
