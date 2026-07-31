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

// TestTrimComponentBytes_NeverExceedsMaxBytesEvenWhenExtAloneIsHuge is a
// regression test for a review finding (Gemini 3.6): when len(suffix) +
// len(ext) alone already exceeds maxBytes, the old implementation clamped
// `allowed` to a floor of 1 (rather than letting it reach 0) and always
// appended the full, oversized ext regardless - so the returned string
// could still exceed maxBytes (255) even after trimming base down to a
// single rune, silently violating the function's whole reason to exist. The
// conflict suffix (the part that makes the file identifiable as a conflict
// copy at all) must be preserved in full; an abnormally long "extension" is
// the part that gives way.
func TestTrimComponentBytes_NeverExceedsMaxBytesEvenWhenExtAloneIsHuge(t *testing.T) {
	suffix := "_conflict_20260102_030405"
	ext := "." + strings.Repeat("x", 300) // a pathologically long "extension"
	component := "name" + suffix + ext

	got := trimComponentBytes(component, 255, suffix, ext)

	if len(got) > 255 {
		t.Fatalf("trimComponentBytes result exceeds maxBytes 255: got %d bytes (%q)", len(got), got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("trimComponentBytes truncated mid-rune, producing invalid UTF-8: %q", got)
	}
	if !strings.Contains(got, suffix) {
		t.Fatalf("expected the conflict suffix to survive truncation (prioritized over the oversized ext), got %q", got)
	}
}

// TestRunConflict_RollsBackLocalRenameWhenDownloadFails is a regression
// test for a review finding (Gemini 3.6): runConflict renames the local
// canonical file aside BEFORE downloading remote's version into its place.
// If that download fails (network error, permission error, remote file
// vanished, disk space...), the old code just returned the error, leaving
// localCanonicalPath missing on disk until some LATER cycle's scan/plan
// happens to notice and re-download it - a real (if self-healing) window
// where the file looks deleted for no reason a user caused. A failed
// download must roll the rename back immediately so this cycle's failure
// doesn't leave the canonical path missing at all.
func TestRunConflict_RollsBackLocalRenameWhenDownloadFails(t *testing.T) {
	localRoot, remoteRoot := t.TempDir(), t.TempDir()
	action := reconcile.Action{Type: reconcile.ActionConflict, Path: "a.txt"}

	if err := os.WriteFile(filepath.Join(localRoot, "a.txt"), []byte("local-content"), 0o644); err != nil {
		t.Fatalf("write local a.txt: %v", err)
	}
	// remote's "a.txt" is missing from disk even though the reconciler's
	// state claims it exists (e.g. a transient SMB read error, or it
	// vanished between scan and execute) - the download step must fail.

	state := SyncState{
		LocalRoot: localRoot, RemoteRoot: remoteRoot,
		LocalFiles:  map[string]fs.FileEntry{"a.txt": {Path: "a.txt", Size: int64(len("local-content"))}},
		RemoteFiles: map[string]fs.FileEntry{"a.txt": {Path: "a.txt", Size: 5}},
	}

	res := runConflict(state, action)
	if res.Error == nil {
		t.Fatal("expected an error since remote's a.txt is missing from disk")
	}

	content, err := os.ReadFile(filepath.Join(localRoot, "a.txt"))
	if err != nil || string(content) != "local-content" {
		t.Fatalf("expected the local canonical file rolled back after the failed download: err=%v content=%q", err, content)
	}
	entries, err := os.ReadDir(localRoot)
	if err != nil {
		t.Fatalf("read local root: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected only the rolled-back a.txt in local root (no leftover conflict-renamed copy), got %v", entries)
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
