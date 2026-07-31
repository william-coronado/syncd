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

	tmp, tmpPath, err := createTempSibling(dstPath, srcInfo.Mode().Perm())
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

// tempSiblingPathFunc is tempSiblingPath, indirected through a package-level
// variable so tests can force two attempts to collide on the same name
// deterministically (same pattern as execute/conflict.go's timeNow seam),
// without depending on an actual nanosecond-timestamp/PRNG coincidence.
var tempSiblingPathFunc = tempSiblingPath

// maxTempSiblingAttempts bounds createTempSibling's retry loop. A genuine
// collision is astronomically unlikely (the name embeds a nanosecond
// timestamp and a random draw) - this exists only so a persistently broken
// destination directory (e.g. read-only) fails with a clear error instead
// of retrying forever.
const maxTempSiblingAttempts = 5

// createTempSibling opens a fresh temp staging file next to dstPath,
// retrying with a new generated name if the chosen one already exists
// (os.ErrExist) rather than silently truncating whatever is already there -
// O_TRUNC alone would clobber a stale leftover temp file from a crashed
// prior run, or the vanishingly rare genuine name collision, instead of
// failing or picking a different name (review finding, Gemini 3.6).
func createTempSibling(dstPath string, perm os.FileMode) (*os.File, string, error) {
	var lastErr error
	for attempt := 0; attempt < maxTempSiblingAttempts; attempt++ {
		tmpPath := tempSiblingPathFunc(dstPath)
		tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
		if err == nil {
			return tmp, tmpPath, nil
		}
		if !os.IsExist(err) {
			return nil, "", err
		}
		lastErr = err
	}
	return nil, "", fmt.Errorf("no available temp filename after %d attempts: %w", maxTempSiblingAttempts, lastErr)
}
