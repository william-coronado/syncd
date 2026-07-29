package execute

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestConflictPath_ShortNameUnaffectedByTruncation(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	got := conflictPath("short.txt", ts)

	want := "short_conflict_20260102_030405.txt"
	if got != want {
		t.Fatalf("conflictPath() = %q, want %q", got, want)
	}
}
