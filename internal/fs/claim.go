package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// EntryCheck runs against the exact directory entry moved aside by syncd.
type EntryCheck func(tree *Tree, name string) error

const holdingPrefix = ".syncd-hold-"

// claimEntry atomically detaches the current name by renaming it to a
// private sibling name (holdingPrefix + a temp suffix) in the same
// directory, and returns a Tree over that directory plus the held name.
// On failure, callers restore it without overwriting a newly created name.
// A crash or failed restoration leaves the original bytes under the held
// name, which is excluded from reconciliation: scans skip it and report it
// as an issue (see Tree.Scan). A sibling rename costs one metadata
// operation, where a private holding directory per claim cost three more
// (mkdir, open, rmdir) - round trips on a network share.
func claimEntry(parent *os.File, leaf string) (*Tree, string, func(error) error, error) {
	var hold string
	var err error
	for range maxTempSiblingAttempts {
		hold = holdingPrefix + tempSiblingPath(leaf)
		if err = publishExclusive(parent, leaf, parent, hold); !os.IsExist(err) {
			break
		}
	}
	if err != nil {
		return nil, "", nil, err
	}
	finish := func(cause error) error {
		if cause != nil {
			if err := publishExclusive(parent, hold, parent, leaf); err != nil {
				return fmt.Errorf("%w; original retained at %s (restore failed: %v)", cause, filepath.Join(parent.Name(), hold), err)
			}
		}
		return cause
	}
	return &Tree{dir: parent, borrowed: true}, hold, finish, nil
}

// exclusiveRename and hardLink are seams so tests can simulate a mount
// that supports neither primitive.
var (
	// noExclusive holds the devices (st_dev) where neither primitive works.
	noExclusive     sync.Map
	exclusiveRename = renameExclusive
	hardLink        = func(src *os.File, old string, dst *os.File, new string) error {
		return unix.Linkat(fd(src), old, fd(dst), new, 0)
	}
)

// publishExclusive moves a name without clobbering another writer. Some
// mounts lack the rename flag but support an exclusive hard link. Some SMB
// mounts support neither; there it falls back to check-then-rename, which
// can still replace a name created inside that window - the exposure a
// plain rename always had - rather than refusing every write and delete.
//
// A device found to lack both primitives is remembered for the life of the
// process, so later publications there skip two failing round trips.
func publishExclusive(src *os.File, old string, dst *os.File, new string) error {
	dev, known := deviceOf(dst)
	if _, lacking := noExclusive.Load(dev); !known || !lacking {
		err := exclusiveRename(src, old, dst, new)
		if !unsupportedOp(err) {
			return err
		}
		err = hardLink(src, old, dst, new)
		if err == nil {
			// The new name now safely references the same entry. The caller
			// owns and can clean up the private/staging name if this unlink
			// fails.
			_ = unix.Unlinkat(fd(src), old, 0)
			return nil
		}
		// EPERM: no hard links on this filesystem (or old is a directory).
		if !unsupportedOp(err) && !errors.Is(err, unix.EPERM) {
			return err
		}
		if known && !errors.Is(err, unix.EPERM) {
			noExclusive.Store(dev, struct{}{})
		}
	}
	if _, err := statAt(dst, new); err == nil {
		return &os.PathError{Op: "publish", Path: filepath.Join(dst.Name(), new), Err: unix.EEXIST}
	} else if !os.IsNotExist(err) {
		return err
	}
	return unix.Renameat(fd(src), old, fd(dst), new)
}

func unsupportedOp(err error) bool {
	return errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL)
}

func deviceOf(f *os.File) (uint64, bool) {
	var st unix.Stat_t
	if err := unix.Fstat(fd(f), &st); err != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}
