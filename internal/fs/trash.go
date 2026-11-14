package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TrashDirName is the trash directory's name inside the state directory.
const TrashDirName = "trash"

// trashDayLayout names the per-day directories inside the trash; PruneTrash
// parses it back to decide what has expired.
const trashDayLayout = "2006-01-02"

// Trash turns a sync-driven delete of a file on a local disk into a move
// to Dir/<day>/<root name>/<relative path>, so a delete propagated from the
// other side (or from a mirror) can be undone by hand. A file on a network
// filesystem is removed outright instead: moving it to a local trash would
// mean downloading everything that gets deleted, and the server's own
// recycle bin (Synology's #recycle) is the recovery path there.
//
// A nil *Trash removes files outright.
type Trash struct {
	Dir string

	network sync.Map // root -> bool: is root on a network filesystem
}

// NewTrash returns a Trash rooted at dir. The directory is created on first
// use, not here.
func NewTrash(dir string) *Trash { return &Trash{Dir: dir} }

// trashNow and isNetworkFS are indirected for tests.
var (
	trashNow    = time.Now
	isNetworkFS = isNetworkFilesystem
)

// Remove deletes root/rel, by moving it to the trash when it is a regular
// file on a local disk. A path that is already gone is not an error. If the
// file cannot be moved to the trash it is left in place and the error
// returned: failing a delete (it is retried next run) is better than making
// an unrecoverable one.
//
// Anything that is not a regular file is handed to os.Remove unchanged, so
// a path that has become a non-empty directory since the scan still fails
// with ENOTEMPTY instead of being moved wholesale.
func (t *Trash) Remove(root, rel string) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if t == nil {
		return removeIfExists(abs)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() || t.onNetwork(root) {
		return removeIfExists(abs)
	}

	now := trashNow()
	dst := filepath.Join(t.Dir, now.Format(trashDayLayout), filepath.Base(root), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("move to trash: %w", err)
	}
	dst, err = freeTrashPath(dst, now)
	if err != nil {
		return fmt.Errorf("move to trash: %w", err)
	}
	if err := os.Rename(abs, dst); err == nil {
		return nil
	}
	// Rename fails across filesystems (a tree on a different disk from the
	// state directory): copy, then remove the original.
	if err := AtomicCopyFile(abs, dst); err != nil {
		return fmt.Errorf("move to trash: %w", err)
	}
	return removeIfExists(abs)
}

func (t *Trash) onNetwork(root string) bool {
	if v, ok := t.network.Load(root); ok {
		return v.(bool)
	}
	v := isNetworkFS(root)
	t.network.Store(root, v)
	return v
}

// freeTrashPath returns dst, or - when a file deleted earlier the same day
// already occupies it - dst with a time (and, if needed, a counter) suffix.
func freeTrashPath(dst string, now time.Time) (string, error) {
	if !lexists(dst) {
		return dst, nil
	}
	stamped := dst + "." + now.Format("150405")
	if !lexists(stamped) {
		return stamped, nil
	}
	for i := 2; i < 1000; i++ {
		if c := fmt.Sprintf("%s-%d", stamped, i); !lexists(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("no free trash name for %s", dst)
}

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PruneTrash deletes the per-day trash directories under dir that are more
// than keepDays days old, returning how many it removed. keepDays <= 0
// keeps everything. Entries that are not day directories are left alone.
func PruneTrash(dir string, keepDays int) (int, error) {
	if keepDays <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
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
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}
