package fs

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

func AtomicCopyFile(srcPath, dstPath string) error {
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat source file: %w", err)
	}
	if srcInfo.IsDir() {
		return fmt.Errorf("source is directory: %s", srcPath)
	}
	if err := EnsureParents(dstPath); err != nil {
		return fmt.Errorf("create destination parent: %w", err)
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()

	tmpPath := tempSiblingPath(dstPath)
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, srcInfo.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpClosed := false
	defer func() {
		if !tmpClosed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()

	if _, err := io.Copy(tmp, src); err != nil {
		return fmt.Errorf("copy bytes: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	tmpClosed = true

	if err := os.Chtimes(tmpPath, srcInfo.ModTime(), srcInfo.ModTime()); err != nil {
		return fmt.Errorf("set temp mtime: %w", err)
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return fmt.Errorf("rename temp into place: %w", err)
	}
	return nil
}

// tempFileInfix marks every atomic-write staging filename tempSiblingPath
// generates: ".<base>" + tempFileInfix + "<nanos>-<rand>". The ignore
// matcher's default staging-file pattern (in ignore.go) is derived from this
// same constant, so the two can never again silently drift apart the way
// "*.tmp" and this naming scheme once did.
const tempFileInfix = ".tmp-"

func tempSiblingPath(dstPath string) string {
	dir := filepath.Dir(dstPath)
	base := filepath.Base(dstPath)
	return filepath.Join(dir, fmt.Sprintf(".%s%s%d-%d", base, tempFileInfix, time.Now().UnixNano(), rand.Intn(100000)))
}
