package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// EntryCheck runs against the exact directory entry moved aside by syncd.
type EntryCheck func(tree *Tree, name string) error

const holdingPrefix = ".syncd-hold-"

// claimEntry atomically detaches the current name into a private directory.
// On failure, callers restore it without overwriting a newly created name.
// A crash or failed restoration leaves the original bytes in the holding
// directory, whose name is excluded from reconciliation: scans skip it and
// report it as an issue (see Tree.Scan).
func claimEntry(parent *os.File, leaf string) (*Tree, func(error) error, error) {
	var holding *os.File
	var name string
	for range maxTempSiblingAttempts {
		name = holdingPrefix + tempSiblingPath(leaf)
		err := unix.Mkdirat(fd(parent), name, 0o700)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		holding, err = openAt(parent, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return nil, nil, err
		}
		break
	}
	if holding == nil {
		return nil, nil, fmt.Errorf("cannot reserve holding directory for %s", leaf)
	}
	if err := unix.Renameat(fd(parent), leaf, fd(holding), leaf); err != nil {
		holding.Close()
		_ = unix.Unlinkat(fd(parent), name, unix.AT_REMOVEDIR)
		return nil, nil, err
	}
	finish := func(cause error) error {
		defer holding.Close()
		if cause != nil {
			if err := publishExclusive(holding, leaf, parent, leaf); err != nil {
				return fmt.Errorf("%w; original retained at %s (restore failed: %v)", cause, filepath.Join(holding.Name(), leaf), err)
			}
		}
		_ = unix.Unlinkat(fd(parent), name, unix.AT_REMOVEDIR)
		return cause
	}
	return &Tree{dir: holding}, finish, nil
}

// exclusiveRename and hardLink are seams so tests can simulate a mount
// that supports neither primitive.
var (
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
func publishExclusive(src *os.File, old string, dst *os.File, new string) error {
	err := exclusiveRename(src, old, dst, new)
	if !unsupportedOp(err) {
		return err
	}
	err = hardLink(src, old, dst, new)
	if err == nil {
		// The new name now safely references the same entry. The caller owns
		// and can clean up the private/staging name if this unlink fails.
		_ = unix.Unlinkat(fd(src), old, 0)
		return nil
	}
	// EPERM: no hard links on this filesystem (or old is a directory).
	if !unsupportedOp(err) && !errors.Is(err, unix.EPERM) {
		return err
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
