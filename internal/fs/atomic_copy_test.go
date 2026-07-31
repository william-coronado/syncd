package fs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAtomicCopyFile_CopiesContentAndMtime(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcPath := filepath.Join(srcDir, "source.txt")
	writeFile(t, srcPath, "atomic copy contents")
	mtime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(srcPath, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	dstPath := filepath.Join(dstDir, "nested", "dest.txt")
	if err := AtomicCopyFile(srcPath, dstPath); err != nil {
		t.Fatalf("AtomicCopyFile returned error: %v", err)
	}

	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "atomic copy contents" {
		t.Fatalf("dest content = %q, want %q", got, "atomic copy contents")
	}

	info, err := os.Stat(dstPath)
	if err != nil {
		t.Fatalf("stat dest: %v", err)
	}
	if !info.ModTime().Equal(mtime) {
		t.Fatalf("dest mtime = %v, want %v", info.ModTime(), mtime)
	}
}

func TestAtomicCopyFile_MissingSourceLeavesDestUntouched(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcPath := filepath.Join(srcDir, "does-not-exist.txt")
	dstPath := filepath.Join(dstDir, "dest.txt")
	writeFile(t, dstPath, "pre-existing content")

	err := AtomicCopyFile(srcPath, dstPath)
	if err == nil {
		t.Fatal("expected error copying a missing source, got nil")
	}

	got, readErr := os.ReadFile(dstPath)
	if readErr != nil {
		t.Fatalf("read dest: %v", readErr)
	}
	if string(got) != "pre-existing content" {
		t.Fatalf("dest content changed after failed copy: %q", got)
	}
}

// TestAtomicCopyFile_RetriesOnTempFileNameCollision is a regression test for
// a review finding (Gemini 3.6): the temp staging file was opened with
// O_CREATE|O_TRUNC, so if the generated name happened to already exist (a
// stale temp file left behind by a crashed prior run, or - vanishingly
// rarely - a genuine timestamp/PRNG coincidence with a concurrent copy),
// the old code would silently truncate and overwrite it instead of picking
// a different name. tempSiblingPathFunc is overridden here (mirroring
// conflict.go's timeNow seam) to force a deterministic collision on the
// first attempt.
func TestAtomicCopyFile_RetriesOnTempFileNameCollision(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()
	srcPath := filepath.Join(srcDir, "source.txt")
	writeFile(t, srcPath, "new content")
	dstPath := filepath.Join(dstDir, "dest.txt")

	collidingPath := filepath.Join(dstDir, ".dest.txt.tmp-collide")
	writeFile(t, collidingPath, "unrelated stale content - must survive")

	orig := tempSiblingPathFunc
	calls := 0
	tempSiblingPathFunc = func(dst string) string {
		calls++
		if calls == 1 {
			return collidingPath
		}
		return orig(dst)
	}
	defer func() { tempSiblingPathFunc = orig }()

	if err := AtomicCopyFile(srcPath, dstPath); err != nil {
		t.Fatalf("AtomicCopyFile returned error: %v", err)
	}
	if calls < 2 {
		t.Fatalf("expected AtomicCopyFile to retry with a new name after a collision, got %d attempt(s)", calls)
	}

	got, err := os.ReadFile(dstPath)
	if err != nil || string(got) != "new content" {
		t.Fatalf("dest content = %q, err = %v, want %q", got, err, "new content")
	}
	staleContent, err := os.ReadFile(collidingPath)
	if err != nil || string(staleContent) != "unrelated stale content - must survive" {
		t.Fatalf("pre-existing colliding temp file was clobbered: err=%v content=%q", err, staleContent)
	}
}

func TestAtomicCopyFile_LeavesNoTempFilesBehind(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcPath := filepath.Join(srcDir, "source.txt")
	writeFile(t, srcPath, "content")
	dstPath := filepath.Join(dstDir, "dest.txt")

	if err := AtomicCopyFile(srcPath, dstPath); err != nil {
		t.Fatalf("AtomicCopyFile returned error: %v", err)
	}

	entries, err := os.ReadDir(dstDir)
	if err != nil {
		t.Fatalf("read dest dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("stray temp file left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "dest.txt" {
		t.Fatalf("expected only dest.txt in dest dir, got %v", entries)
	}
}

// TestAtomicCopyFile_CopiesFileWithNearMaxLengthName is a regression test
// for a review finding (GPT-5.6 Sol): tempSiblingPath builds its staging
// name by prepending "." and appending tempFileInfix+nanos+"-"+rand (~31
// bytes worst case) to the FULL destination basename. Most filesystems
// (ext4, APFS, NTFS/SMB) cap a single path component at 255 bytes. A
// perfectly valid destination filename sitting near that limit therefore
// produces a staging name that exceeds it, and AtomicCopyFile fails with
// ENAMETOOLONG even though the destination name itself is completely legal.
// 251 'a's + ".txt" = exactly 255 bytes - valid on its own, but the old
// staging-name scheme could never create a temp file for it.
func TestAtomicCopyFile_CopiesFileWithNearMaxLengthName(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	longName := strings.Repeat("a", 251) + ".txt"
	if len(longName) != 255 {
		t.Fatalf("test fixture name is %d bytes, want exactly 255", len(longName))
	}

	srcPath := filepath.Join(srcDir, longName)
	writeFile(t, srcPath, "content near the name length limit")
	dstPath := filepath.Join(dstDir, longName)

	if err := AtomicCopyFile(srcPath, dstPath); err != nil {
		t.Fatalf("AtomicCopyFile returned error for a valid near-max-length filename: %v", err)
	}

	got, err := os.ReadFile(dstPath)
	if err != nil || string(got) != "content near the name length limit" {
		t.Fatalf("dest content = %q, err = %v", got, err)
	}
}
