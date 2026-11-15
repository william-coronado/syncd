package fs

import (
	"errors"
	"testing"

	"syncd/internal/db"
)

func TestCheckCaseAliasesIncludesDirectoriesAndBaseline(t *testing.T) {
	for _, tc := range []struct {
		name        string
		baseline    map[string]db.ManifestRow
		left, right string
		wantError   bool
	}{
		{"same spelling", nil, "dir/file", "dir/file", false},
		{"distinct", nil, "dir/one", "dir/two", false},
		{"file case", nil, "Report.txt", "report.txt", true},
		{"directory case", nil, "Dir/one", "dir/two", true},
		// A baseline spelling gone from both sides is a stale row (both
		// sides renamed alike); it must not block the pair forever.
		{"stale baseline case", map[string]db.ManifestRow{"old.txt": {}}, "Old.txt", "Old.txt", false},
		// Without hashes (the fast path) a one-sided rename stays refused.
		{"unhashed one-sided rename", map[string]db.ManifestRow{"old.txt": {}}, "Old.txt", "old.txt", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckCaseAliases(tc.baseline, ScanResult{Files: map[string]FileEntry{tc.left: {}}}, ScanResult{Files: map[string]FileEntry{tc.right: {}}})
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}

func TestCaseAliasesIncludeSkippedEntries(t *testing.T) {
	local := ScanResult{Files: map[string]FileEntry{"report": {}}}
	remote := ScanResult{Skipped: map[string]struct{}{"Report": {}}}
	if err := CheckCaseAliases(nil, local, remote); err == nil {
		t.Fatal("skipped entry was omitted from case checks")
	}
}

func TestCaseOnlyRenameOfUnchangedFileIsAllowed(t *testing.T) {
	tracked := map[string]db.ManifestRow{"dir/Report.txt": {Path: "dir/Report.txt", Size: 4, Hash: "h"}}
	renamed := ScanResult{Files: map[string]FileEntry{"dir/report.txt": {Size: 4, Hash: "h"}}}
	unchanged := ScanResult{Files: map[string]FileEntry{"dir/Report.txt": {Size: 4, Hash: "h"}}}
	edited := ScanResult{Files: map[string]FileEntry{"dir/Report.txt": {Size: 4, Hash: "other"}}}
	if err := CheckCaseAliases(tracked, renamed, unchanged); err != nil {
		t.Fatalf("local case-only rename refused: %v", err)
	}
	if err := CheckCaseAliases(tracked, unchanged, renamed); err != nil {
		t.Fatalf("remote case-only rename refused: %v", err)
	}
	if err := CheckCaseAliases(tracked, renamed, edited); !errors.Is(err, ErrCaseAlias) {
		t.Fatalf("rename against an edited old spelling must be refused, got %v", err)
	}
	both := ScanResult{Files: map[string]FileEntry{"dir/report.txt": {}, "dir/Report.txt": {Size: 4, Hash: "h"}}}
	if err := CheckCaseAliases(tracked, both, unchanged); !errors.Is(err, ErrCaseAlias) {
		t.Fatalf("two spellings on one side must be refused, got %v", err)
	}
	dirRename := ScanResult{Files: map[string]FileEntry{"Dir/Report.txt": {Size: 4, Hash: "h"}}}
	if err := CheckCaseAliases(tracked, dirRename, unchanged); !errors.Is(err, ErrCaseAlias) {
		t.Fatalf("directory case rename must be refused, got %v", err)
	}
}
