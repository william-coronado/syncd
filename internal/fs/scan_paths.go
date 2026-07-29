package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"syncd/internal/db"
)

// ScanPaths stat/hashes ONLY the given root-relative paths under root - it
// does NOT walk the tree. For each rel path: Lstat root/rel; skip if
// missing (absent side), a directory, or ignored by matcher; if
// validateSMBName and the basename has an SMB-forbidden char, record it in
// Issues+Skipped; otherwise produce a FileEntry with Hash per policy
// (HashIfChanged reuses the manifest cache exactly as ScanTree does).
// Returns a ScanResult (Dirs is always empty here).
func ScanPaths(root string, relPaths map[string]struct{}, manifest map[string]db.ManifestRow, policy HashPolicy, validateSMBName bool, matcher *IgnoreMatcher) (ScanResult, error) {
	result := ScanResult{
		Files:   make(map[string]FileEntry),
		Skipped: make(map[string]struct{}),
		Dirs:    make(map[string]struct{}),
	}
	scanStart := time.Now().Unix()
	for rel := range relPaths {
		if matcher.Match(rel, false) {
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue // absent on this side
			}
			return ScanResult{}, fmt.Errorf("lstat %s: %w", abs, err)
		}
		if info.IsDir() {
			continue // scoped incremental handles files; dirs via deep scan
		}
		if validateSMBName && hasSMBForbiddenChar(filepath.Base(abs)) {
			result.Issues = append(result.Issues, ScanIssue{Path: rel, Reason: "filename contains SMB-forbidden characters"})
			result.Skipped[rel] = struct{}{}
			continue
		}
		size := info.Size()
		mtime := info.ModTime().Unix()
		hash := ""
		switch policy {
		case HashNever:
		case HashAlways:
			hash, err = FileBLAKE3(abs)
			if err != nil {
				return ScanResult{}, fmt.Errorf("hash file %s: %w", abs, err)
			}
		default: // HashIfChanged
			if row, ok := manifest[rel]; ok && row.Size == size && row.Mtime == mtime && mtime < scanStart {
				hash = row.Hash
			} else {
				hash, err = FileBLAKE3(abs)
				if err != nil {
					return ScanResult{}, fmt.Errorf("hash file %s: %w", abs, err)
				}
			}
		}
		result.Files[rel] = FileEntry{Path: rel, AbsPath: abs, Size: size, Mtime: mtime, Hash: hash}
	}
	return result, nil
}
