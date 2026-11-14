package fs

import (
	"fmt"
	"path"
	"sort"

	"golang.org/x/text/cases"
	"syncd/internal/db"
)

// CheckCaseAliases refuses ambiguous names before reconciliation. Sync pairs
// must be portable to case-insensitive destinations (notably APFS and SMB).
// Include directory components and baseline paths: a copy and delete of two
// spellings of the same file must never be scheduled together.
func CheckCaseAliases(manifest map[string]db.ManifestRow, scans ...ScanResult) error {
	paths := make(map[string]struct{})
	add := func(p string) {
		for ; p != "." && p != "/"; p = path.Dir(p) {
			paths[p] = struct{}{}
		}
	}
	for p := range manifest {
		add(p)
	}
	for _, scan := range scans {
		for p := range scan.Files {
			add(p)
		}
		for p := range scan.Dirs {
			add(p)
		}
	}
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	fold := cases.Fold()
	seen := make(map[string]string)
	for _, p := range names {
		key := fold.String(p)
		if prev, ok := seen[key]; ok && prev != p {
			return fmt.Errorf("case-insensitive path collision: %q and %q; give these paths distinct names before syncing", prev, p)
		}
		seen[key] = p
	}
	return nil
}
