package fs

import (
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
		{"baseline case", map[string]db.ManifestRow{"old.txt": {}}, "Old.txt", "Old.txt", true},
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
