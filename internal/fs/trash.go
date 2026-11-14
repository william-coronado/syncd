package fs

import (
	"crypto/sha256"
	"encoding/hex"
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
	// Classified by the file's own directory, not the root: a root can
	// contain a mount of a different kind.
	if !info.Mode().IsRegular() || t.onNetwork(filepath.Dir(abs)) {
		return removeIfExists(abs)
	}

	dst, err := t.reserve(root, rel)
	if err != nil {
		return fmt.Errorf("move to trash: %w", err)
	}
	// dst is an empty file only this call owns, so replacing it is safe.
	if err := os.Rename(abs, dst); err == nil {
		return nil
	}
	// Rename fails across filesystems (a tree on a different disk from the
	// state directory): copy, then remove the original.
	if err := AtomicCopyFile(abs, dst); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("move to trash: %w", err)
	}
	return removeIfExists(abs)
}

// Preserve copies root/rel into the trash and leaves the original where it
// is, for a file about to be overwritten rather than deleted. Unlike
// Remove it applies to network filesystems too: the caller has decided
// this one version is worth keeping. A path that does not exist, or is not
// a regular file, is not an error and nothing is kept.
func (t *Trash) Preserve(root, rel string) error {
	if t == nil {
		return nil
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	dst, err := t.reserve(root, rel)
	if err != nil {
		return fmt.Errorf("keep a copy in trash: %w", err)
	}
	if err := AtomicCopyFile(abs, dst); err != nil {
		_ = os.Remove(dst)
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

// reserve creates, exclusively, an empty file at the trash destination for
// root/rel and returns its path. O_EXCL makes the claim atomic across
// processes; an existence check followed by a rename would let two
// concurrent deletes pick the same name and the second replace the first.
// When a file deleted earlier the same day already holds the plain name, a
// time (and, if needed, a counter) suffix is tried.
func (t *Trash) reserve(root, rel string) (string, error) {
	now := trashNow()
	base := filepath.Join(t.Dir, now.Format(trashDayLayout), RootFolder(root), filepath.FromSlash(rel))
	// Owner-only: a file that was private because of its directory must not
	// become readable by sitting in the trash.
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return "", err
	}
	stamped := base + "." + now.Format("150405")
	for attempt := 0; attempt < 1000; attempt++ {
		candidate := base
		switch {
		case attempt == 1:
			candidate = stamped
		case attempt > 1:
			candidate = fmt.Sprintf("%s-%d", stamped, attempt)
		}
		f, err := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return candidate, f.Close()
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("no free trash name for %s", base)
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
