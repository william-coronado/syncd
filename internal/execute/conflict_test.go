package execute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"syncd/internal/fs"
	"syncd/internal/reconcile"
)

func TestConflictPath_NamingFormat(t *testing.T) {
	ts := time.Date(2026, 7, 21, 15, 4, 5, 0, time.UTC)

	got := conflictPath("docs/report.pdf", ts)

	want := "docs/report_conflict_20260721_150405.pdf"
	if got != want {
		t.Fatalf("conflictPath() = %q, want %q", got, want)
	}
}

func TestConflictPath_PreservesExtensionAndDir(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	got := conflictPath("a/b/c/photo.jpeg", ts)

	if !strings.HasSuffix(got, ".jpeg") {
		t.Fatalf("expected extension preserved, got %q", got)
	}
	if filepath.Dir(got) != "a/b/c" {
		t.Fatalf("expected directory preserved, got dir %q from %q", filepath.Dir(got), got)
	}
}

func TestConflictPath_NoExtension(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	got := conflictPath("README", ts)

	want := "README_conflict_20260102_030405"
	if got != want {
		t.Fatalf("conflictPath() = %q, want %q", got, want)
	}
}

func TestConflictPath_TruncatesLongBasenameAtRuneBoundary(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	// Multi-byte rune (3 bytes each in UTF-8) repeated so the base name is
	// long enough to force truncation once the conflict suffix is appended.
	longBase := strings.Repeat("漢", 120) // "漢" x120 = 360 bytes
	relPath := longBase + ".txt"

	got := conflictPath(relPath, ts)

	if len(got) > 255 {
		t.Fatalf("conflictPath result exceeds 255 bytes: %d bytes (%q)", len(got), got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("conflictPath truncated mid-rune, producing invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, ".txt") {
		t.Fatalf("expected extension preserved after truncation, got %q", got)
	}
	if !strings.Contains(got, "_conflict_20260102_030405") {
		t.Fatalf("expected conflict suffix preserved after truncation, got %q", got)
	}
}

// TestRunConflict_SecondConflictWithinSameSecondDoesNotOverwriteFirst is a
// regression test for review finding "Medium 6": conflictPath has only
// 1-second resolution, and runConflict's renameFile is a bare os.Rename,
// which silently REPLACES an existing destination on Unix. Two conflicts on
// the same relPath within the same wall-clock second must produce two
// distinct preserved conflict files, not have the second call's rename
// clobber the first call's already-renamed-aside conflict copy.
func TestRunConflict_SecondConflictWithinSameSecondDoesNotOverwriteFirst(t *testing.T) {
	orig := timeNow
	fixed := time.Date(2026, 7, 21, 15, 4, 5, 0, time.UTC)
	timeNow = func() time.Time { return fixed }
	defer func() { timeNow = orig }()

	localRoot, remoteRoot := t.TempDir(), t.TempDir()
	action := reconcile.Action{Type: reconcile.ActionConflict, Path: "a.txt"}

	writeBoth := func(localContent, remoteContent string) {
		if err := os.WriteFile(filepath.Join(localRoot, "a.txt"), []byte(localContent), 0o644); err != nil {
			t.Fatalf("write local a.txt: %v", err)
		}
		if err := os.WriteFile(filepath.Join(remoteRoot, "a.txt"), []byte(remoteContent), 0o644); err != nil {
			t.Fatalf("write remote a.txt: %v", err)
		}
	}

	// First conflict: local="first-local", remote="first-remote".
	writeBoth("first-local", "first-remote")
	state := SyncState{
		LocalRoot: localRoot, RemoteRoot: remoteRoot,
		LocalFiles:  map[string]fs.FileEntry{"a.txt": {Path: "a.txt", Size: int64(len("first-local"))}},
		RemoteFiles: map[string]fs.FileEntry{"a.txt": {Path: "a.txt", Size: int64(len("first-remote"))}},
	}
	res1 := runConflict(state, action)
	if res1.Error != nil {
		t.Fatalf("first runConflict: %v", res1.Error)
	}

	// Second conflict on the same path, same (fixed) second: local canonical
	// was just overwritten with remote's content by the first call, so
	// simulate a fresh local edit conflicting again with a further remote
	// edit.
	writeBoth("second-local", "second-remote")
	state2 := SyncState{
		LocalRoot: localRoot, RemoteRoot: remoteRoot,
		LocalFiles:  map[string]fs.FileEntry{"a.txt": {Path: "a.txt", Size: int64(len("second-local"))}},
		RemoteFiles: map[string]fs.FileEntry{"a.txt": {Path: "a.txt", Size: int64(len("second-remote"))}},
	}
	res2 := runConflict(state2, action)
	if res2.Error != nil {
		t.Fatalf("second runConflict: %v", res2.Error)
	}

	firstConflictRel := res1.Updates[1].Path
	secondConflictRel := res2.Updates[1].Path
	if firstConflictRel == secondConflictRel {
		t.Fatalf("expected distinct conflict paths for two same-second conflicts, both got %q", firstConflictRel)
	}

	firstContent, err := os.ReadFile(filepath.Join(localRoot, filepath.FromSlash(firstConflictRel)))
	if err != nil {
		t.Fatalf("read first conflict file %q: %v (overwritten by the second conflict?)", firstConflictRel, err)
	}
	if string(firstContent) != "first-local" {
		t.Errorf("first conflict file content = %q, want %q - it was overwritten", firstContent, "first-local")
	}
}

func TestConflictPath_ShortNameUnaffectedByTruncation(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	got := conflictPath("short.txt", ts)

	want := "short_conflict_20260102_030405.txt"
	if got != want {
		t.Fatalf("conflictPath() = %q, want %q", got, want)
	}
}
