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
// missing (absent side), a directory, ignored by matcher, a symlink, or a
// non-regular file (device/socket/FIFO); if validateSMBName and the
// basename has an SMB-forbidden char, record it in Issues+Skipped;
// otherwise produce a FileEntry with Hash per policy (HashIfChanged reuses
// the manifest cache exactly as ScanTree does). Returns a ScanResult (Dirs
// and SkippedDirs are always empty here: ScanPaths never walks a subtree, so
// there is no "unreadable subdirectory" case for it to record).
//
// Like ScanTree, a per-entry problem (an unreadable file, a symlink, a
// non-regular file) is recorded in Issues+Skipped and does not fail the
// call - there's no whole-scan concept here since each path is looked up
// independently, so ScanPaths does not abort on anything short of a caller
// bug (there is no ctx to cancel).
func ScanPaths(root string, relPaths map[string]struct{}, manifest map[string]db.ManifestRow, policy HashPolicy, validateSMBName bool, matcher *IgnoreMatcher) (ScanResult, error) {
	result := ScanResult{
		Files:       make(map[string]FileEntry),
		Skipped:     make(map[string]struct{}),
		SkippedDirs: make(map[string]struct{}),
		Dirs:        make(map[string]struct{}),
	}
	scanStart := time.Now().Unix()
	for rel := range relPaths {
		if matcher.MatchPathOrAncestor(rel, false) {
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue // absent on this side
			}
			result.Issues = append(result.Issues, ScanIssue{Path: rel, Reason: fmt.Sprintf("lstat: %s", err)})
			result.Skipped[rel] = struct{}{}
			continue
		}
		if info.IsDir() {
			continue // scoped incremental handles files; dirs via deep scan
		}
		// Symlinks are never followed (see ScanTree's doc comment for why);
		// devices/sockets/FIFOs are skipped the same way so hashing one
		// can't block forever.
		if info.Mode()&os.ModeSymlink != 0 {
			result.Issues = append(result.Issues, ScanIssue{Path: rel, Reason: "symlink not supported"})
			result.Skipped[rel] = struct{}{}
			continue
		}
		if !info.Mode().IsRegular() {
			result.Issues = append(result.Issues, ScanIssue{Path: rel, Reason: "not a regular file"})
			result.Skipped[rel] = struct{}{}
			continue
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
			h, hashErr := FileBLAKE3(abs)
			if hashErr != nil {
				result.Issues = append(result.Issues, ScanIssue{Path: rel, Reason: fmt.Sprintf("hash file: %s", hashErr)})
				result.Skipped[rel] = struct{}{}
				continue
			}
			hash = h
		default: // HashIfChanged
			if row, ok := manifest[rel]; ok && row.Size == size && row.Mtime == mtime && mtime < scanStart {
				hash = row.Hash
			} else {
				h, hashErr := FileBLAKE3(abs)
				if hashErr != nil {
					result.Issues = append(result.Issues, ScanIssue{Path: rel, Reason: fmt.Sprintf("hash file: %s", hashErr)})
					result.Skipped[rel] = struct{}{}
					continue
				}
				hash = h
			}
		}
		result.Files[rel] = FileEntry{Path: rel, AbsPath: abs, Size: size, Mtime: mtime, Hash: hash}
	}
	return result, nil
}
