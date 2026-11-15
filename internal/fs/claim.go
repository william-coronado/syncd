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
// directory, whose name is excluded from reconciliation.
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
	// Probe before detaching user data. Some network mounts support
	// neither exclusive rename nor hard links, which would prevent rollback.
	if err := checkExclusivePublish(holding); err != nil {
		holding.Close()
		_ = unix.Unlinkat(fd(parent), name, unix.AT_REMOVEDIR)
		return nil, nil, fmt.Errorf("filesystem cannot safely restore %s: %w", leaf, err)
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

// publishExclusive moves a name without clobbering another writer. Some
// mounts lack the rename flag but support an exclusive hard link.
func publishExclusive(src *os.File, old string, dst *os.File, new string) error {
	err := renameExclusive(src, old, dst, new)
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EINVAL) {
		return err
	}
	if err := unix.Linkat(fd(src), old, fd(dst), new, 0); err != nil {
		return err
	}
	// The new name now safely references the same entry. The caller owns
	// and can clean up the private/staging name if this unlink fails.
	_ = unix.Unlinkat(fd(src), old, 0)
	return nil
}

func checkExclusivePublish(dir *os.File) error {
	probe, err := openAt(dir, ".probe", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(fd(dir), ".probe", 0)
	defer unix.Unlinkat(fd(dir), ".probe-renamed", 0)
	if err := probe.Close(); err != nil {
		return err
	}
	return publishExclusive(dir, ".probe", dir, ".probe-renamed")
}
