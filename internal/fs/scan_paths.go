package fs

import (
	"errors"
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
	tree, err := OpenTree(root)
	if err != nil {
		return ScanResult{}, err
	}
	defer tree.Close()
	return tree.ScanPaths(relPaths, manifest, policy, validateSMBName, matcher)
}

func (tree *Tree) ScanPaths(relPaths map[string]struct{}, manifest map[string]db.ManifestRow, policy HashPolicy, validateSMBName bool, matcher *IgnoreMatcher) (ScanResult, error) {
	root := tree.Name()

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
		info, err := tree.Lstat(filepath.FromSlash(rel))
		if err != nil {
			if os.IsNotExist(err) {
				continue // absent on this side
			}
			reason := fmt.Sprintf("lstat: %s", err)
			if errors.Is(err, ErrSymlink) {
				reason = ErrSymlink.Error()
			}
			result.Issues = append(result.Issues, ScanIssue{Path: rel, Reason: reason})
			result.Skipped[rel] = struct{}{}
			continue
		}
		if info.IsDir() {
			continue // scoped incremental handles files; dirs via deep scan
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
			h, hashErr := hashRegularInRoot(tree, filepath.FromSlash(rel))
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
				h, hashErr := hashRegularInRoot(tree, filepath.FromSlash(rel))
				if hashErr != nil {
					result.Issues = append(result.Issues, ScanIssue{Path: rel, Reason: fmt.Sprintf("hash file: %s", hashErr)})
					result.Skipped[rel] = struct{}{}
					continue
				}
				hash = h
			}
		}
		result.Files[rel] = FileEntry{Path: rel, AbsPath: abs, Size: size, Mtime: mtime, Hash: hash, ModTime: info.ModTime(), Info: info}
	}
	return result, nil
}
