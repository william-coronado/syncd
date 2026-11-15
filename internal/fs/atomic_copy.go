package fs

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// AtomicCopyFile is the unscoped convenience API. Sync operations use
// AtomicCopyFileInRoots so neither side can escape its configured root.
func AtomicCopyFile(srcPath, dstPath string) error {
	if err := EnsureParents(dstPath); err != nil {
		return err
	}
	return AtomicCopyFileInRoots(filepath.Dir(srcPath), filepath.Base(srcPath), filepath.Dir(dstPath), filepath.Base(dstPath))
}

func AtomicCopyFileInRoots(srcRoot, srcRel, dstRoot, dstRel string) error {
	source, err := OpenTree(srcRoot)
	if err != nil {
		return err
	}
	defer source.Close()
	dest, err := OpenTree(dstRoot)
	if err != nil {
		return err
	}
	defer dest.Close()
	return CopyBetween(source, srcRel, dest, dstRel)
}

func CopyBetween(source *Tree, srcRel string, dest *Tree, dstRel string) error {
	return copyBetween(source, srcRel, dest, dstRel, nil, false, nil)
}

// CopyChecked stages the incoming data before taking the destination out
// of circulation. The check sees the actual displaced entry, and an entry
// created concurrently at the final name is never overwritten. check runs
// up to three times on the displaced entry (after the claim, after
// preserve, after publication); a check that reads content should do so
// only on its first call (see execute.checkObserved).
func CopyChecked(source *Tree, srcRel string, dest *Tree, dstRel string, present bool, check EntryCheck, preserve func(*Tree, string) error) error {
	return copyBetween(source, srcRel, dest, dstRel, check, !present, preserve)
}

func copyBetween(source *Tree, srcRel string, dest *Tree, dstRel string, check EntryCheck, absent bool, preserve func(*Tree, string) error) error {
	src, err := source.openRegular(srcRel)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	srcInfo, err := src.Stat()
	if err != nil {
		return err
	}
	parent, leaf, err := dest.parent(dstRel, 0o755)
	if err != nil {
		return err
	}
	defer parent.Close()
	tmp, tmpName, err := createTempSibling(parent, leaf, srcInfo.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer tmp.Close()
	defer unix.Unlinkat(fd(parent), tmpName, 0)
	if _, err := io.Copy(tmp, src); err != nil {
		return fmt.Errorf("copy bytes: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	// Only after every byte is flushed and the handle closed: on network
	// filesystems a later flush can stamp the server's "now" over it.
	ts := unix.NsecToTimespec(srcInfo.ModTime().UnixNano())
	if err := unix.UtimesNanoAt(fd(parent), tmpName, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("set temp mtime: %w", err)
	}
	if err := dest.verifyParent(dstRel, parent); err != nil {
		return err
	}
	if absent {
		return publishExclusive(parent, tmpName, parent, leaf)
	}
	if check == nil {
		if info, err := statAt(parent, leaf); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular entry: %s", dstRel)
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		return unix.Renameat(fd(parent), tmpName, fd(parent), leaf)
	}
	claimed, held, finish, err := claimEntry(parent, leaf)
	if err != nil {
		return err
	}
	if err := check(claimed, held); err != nil {
		return finish(err)
	}
	if preserve != nil {
		if err := preserve(claimed, held); err != nil {
			return finish(err)
		}
		if err := check(claimed, held); err != nil {
			return finish(err)
		}
	}
	if err := publishExclusive(parent, tmpName, parent, leaf); err != nil {
		return finish(err)
	}
	// Detect edits to an already-open displaced file during publication.
	// Leave it available for recovery if it changed or removal failed.
	checkErr := check(claimed, held)
	if checkErr == nil {
		checkErr = claimed.Remove(held, false)
	}
	if checkErr != nil {
		return fmt.Errorf("replacement published, original retained at %s: %w", filepath.Join(parent.Name(), held), checkErr)
	}
	return finish(nil)
}

// tempFileInfix marks every atomic-write staging filename tempSiblingPath
// generates: ".<base>" + tempFileInfix + "<nanos>-<rand>". The ignore
// matcher's default staging-file pattern (in ignore.go) is derived from this
// same constant, so the two can never again silently drift apart the way
// "*.tmp" and this naming scheme once did.
const tempFileInfix = ".tmp-"

// tempSiblingMaxBase caps how much of dstPath's basename is embedded in the
// staging filename. tempSiblingPath prepends "." (1 byte) and appends
// tempFileInfix+nanos+"-"+rand (at most 5+19+1+5 = 30 bytes) to base; most
// filesystems (ext4, APFS, NTFS/SMB) cap a single path component at 255
// bytes. Without a cap here, a destination basename that is itself valid and
// merely close to that limit produces a staging name that exceeds it, and
// AtomicCopyFile fails with ENAMETOOLONG on a perfectly legal filename
// (review finding, GPT-5.6 Sol). The staging name is transient - deleted or
// renamed away before AtomicCopyFile returns - so truncating it costs
// nothing: 160 leaves 1+160+30 = 191 bytes, comfortably under 255.
const tempSiblingMaxBase = 160

func tempSiblingPath(dstPath string) string {
	dir := filepath.Dir(dstPath)
	base := truncateBytesAtRuneBoundary(filepath.Base(dstPath), tempSiblingMaxBase)
	return filepath.Join(dir, fmt.Sprintf(".%s%s%d-%d", base, tempFileInfix, time.Now().UnixNano(), rand.Intn(100000)))
}

// truncateBytesAtRuneBoundary returns the longest prefix of s that is at
// most maxBytes bytes long and ends on a valid UTF-8 rune boundary, so
// truncating a multi-byte filename never produces invalid UTF-8.
func truncateBytesAtRuneBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for len(s) > maxBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		if size <= 0 {
			break
		}
		s = s[:len(s)-size]
	}
	return s
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
func createTempSibling(parent *os.File, dstName string, perm os.FileMode) (*os.File, string, error) {
	var lastErr error
	for attempt := 0; attempt < maxTempSiblingAttempts; attempt++ {
		tmpPath := filepath.Base(tempSiblingPathFunc(filepath.Join(parent.Name(), dstName)))
		tmp, err := openAt(parent, tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
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
