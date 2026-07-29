package app

import (
	"os"
	"path/filepath"
	"testing"

	"drivesync/internal/runtime"
)

func newTestLogger(t *testing.T) *runtime.Logger {
	t.Helper()
	logger, err := runtime.NewLogger(true, filepath.Join(t.TempDir(), "sync.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	return logger
}

func TestCreateMissingEmptyDirs_CreatesOnBothSidesAsNeeded(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	logger := newTestLogger(t)

	localDirs := map[string]struct{}{"only-local": {}, "shared": {}}
	remoteDirs := map[string]struct{}{"only-remote": {}, "shared": {}}

	failures := createMissingEmptyDirs(logger, localRoot, remoteRoot, localDirs, remoteDirs)
	if failures != 0 {
		t.Fatalf("expected 0 failures, got %d", failures)
	}

	if info, err := os.Stat(filepath.Join(remoteRoot, "only-local")); err != nil || !info.IsDir() {
		t.Fatalf("expected only-local mirrored to remote, stat err = %v", err)
	}
	if info, err := os.Stat(filepath.Join(localRoot, "only-remote")); err != nil || !info.IsDir() {
		t.Fatalf("expected only-remote mirrored to local, stat err = %v", err)
	}
	// "shared" already exists conceptually on both sides in the input maps;
	// this only asserts we don't error attempting to create it - since
	// neither temp dir actually has it on disk, MkdirAll would create it if
	// (incorrectly) attempted, so absence would indicate a bug too. Since
	// it's present in both maps, no creation should be attempted at all.
	if _, err := os.Stat(filepath.Join(remoteRoot, "shared")); !os.IsNotExist(err) {
		t.Fatalf("did not expect 'shared' to be created on remote (present in both sides' maps already), err = %v", err)
	}
}

func TestCreateMissingEmptyDirs_NoOpWhenAllShared(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	logger := newTestLogger(t)

	dirs := map[string]struct{}{"a": {}, "a/b": {}}

	failures := createMissingEmptyDirs(logger, localRoot, remoteRoot, dirs, dirs)
	if failures != 0 {
		t.Fatalf("expected 0 failures, got %d", failures)
	}
	entries, err := os.ReadDir(localRoot)
	if err != nil {
		t.Fatalf("read local root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no directories created locally, got %v", entries)
	}
}

func TestCleanupEmptiedDirs_RemovesOnlyDirsEmptiedByDeletes(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()

	// "emptied" existed on both sides with exactly the deleted file in it.
	if err := os.MkdirAll(filepath.Join(localRoot, "emptied"), 0o755); err != nil {
		t.Fatalf("mkdir local emptied: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(remoteRoot, "emptied"), 0o755); err != nil {
		t.Fatalf("mkdir remote emptied: %v", err)
	}

	// "still-has-files" also had a deletion, but still contains another file.
	if err := os.MkdirAll(filepath.Join(localRoot, "still-has-files"), 0o755); err != nil {
		t.Fatalf("mkdir local still-has-files: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "still-has-files", "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}

	// "untouched-empty" was already empty before this run and never had a
	// delete in it - must be left alone entirely.
	if err := os.MkdirAll(filepath.Join(localRoot, "untouched-empty"), 0o755); err != nil {
		t.Fatalf("mkdir untouched-empty: %v", err)
	}

	deleted := []string{"emptied/gone.txt", "still-has-files/gone.txt"}
	cleanupEmptiedDirs(localRoot, remoteRoot, deleted)

	if _, err := os.Stat(filepath.Join(localRoot, "emptied")); !os.IsNotExist(err) {
		t.Fatalf("expected local 'emptied' dir to be removed, err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "emptied")); !os.IsNotExist(err) {
		t.Fatalf("expected remote 'emptied' dir to be removed, err = %v", err)
	}
	if info, err := os.Stat(filepath.Join(localRoot, "still-has-files")); err != nil || !info.IsDir() {
		t.Fatalf("expected 'still-has-files' to remain (not empty), stat err = %v", err)
	}
	if info, err := os.Stat(filepath.Join(localRoot, "untouched-empty")); err != nil || !info.IsDir() {
		t.Fatalf("expected untouched pre-existing empty dir to be left alone, stat err = %v", err)
	}
}
