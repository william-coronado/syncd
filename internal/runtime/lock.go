package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

type Lock struct {
	file *os.File
	path string
}

func AcquireLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	// Before locking: anyone who can open a lock can hold its flock, and
	// one an older syncd created 0644 let other users block every sync.
	// Best-effort, as a lock owned by someone else cannot be changed.
	_ = f.Chmod(stateFileMode)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sync lock already held at %s", path)
	}
	return &Lock{file: f, path: path}, nil
}

func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	defer func() {
		l.file = nil
	}()
	if err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN); err != nil {
		_ = l.file.Close()
		return fmt.Errorf("unlock lock file: %w", err)
	}
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("close lock file: %w", err)
	}
	return nil
}

// acquireLocks releases all partial claims if any lock is already held.
func acquireLocks(paths []string) (func(), error) {
	sort.Strings(paths)
	var locks []*Lock
	var identities []os.FileInfo
	release := func() {
		for i := len(locks) - 1; i >= 0; i-- {
			_ = locks[i].Release()
		}
	}
	seen := make(map[string]bool)
	for _, path := range paths {
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		if info, err := os.Stat(path); err == nil {
			duplicate := false
			for _, held := range identities {
				if os.SameFile(info, held) {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
		}
		lock, err := AcquireLock(path)
		if err != nil {
			release()
			return nil, err
		}
		locks = append(locks, lock)
		if info, err := lock.file.Stat(); err == nil {
			identities = append(identities, info)
		}
	}
	return release, nil
}
