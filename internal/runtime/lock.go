package runtime

import (
	"fmt"
	"os"
	"syscall"
)

type Lock struct {
	file *os.File
	path string
}

func AcquireLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
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
