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
