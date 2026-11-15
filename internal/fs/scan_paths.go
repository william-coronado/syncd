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
		// One confined walk to the parent serves both the stat and the
		// hash (each Tree call would re-walk from the root).
		parent, leaf, err := tree.parent(filepath.FromSlash(rel), 0)
		var info os.FileInfo
		if err == nil {
			info, err = statAt(parent, leaf)
			if err == nil && info.Mode()&os.ModeSymlink != 0 {
				err = fmt.Errorf("%w: %s", ErrSymlink, rel)
			}
		}
		hashFile := func() (string, error) {
			f, err := openRegularAt(parent, leaf)
			if err != nil {
				return "", err
			}
			defer f.Close()
			return hashReader(f)
		}
		entry, issue, ok := scanPathEntry(rel, abs, info, err, manifest, policy, validateSMBName, scanStart, hashFile)
		if parent != nil {
			parent.Close()
		}
		if issue != nil {
			result.Issues = append(result.Issues, *issue)
			result.Skipped[rel] = struct{}{}
		}
		if ok {
			result.Files[rel] = entry
		}
	}
	return result, nil
}

// scanPathEntry classifies one scoped path from its stat result. ok
// reports a file entry; issue, a path to skip this cycle.
func scanPathEntry(rel, abs string, info os.FileInfo, err error, manifest map[string]db.ManifestRow, policy HashPolicy, validateSMBName bool, scanStart int64, hashFile func() (string, error)) (entry FileEntry, issue *ScanIssue, ok bool) {
	skip := func(reason string) (FileEntry, *ScanIssue, bool) {
		return FileEntry{}, &ScanIssue{Path: rel, Reason: reason}, false
	}
	if err != nil {
		if os.IsNotExist(err) {
			return FileEntry{}, nil, false // absent on this side
		}
		if errors.Is(err, ErrSymlink) {
			return skip(ErrSymlink.Error())
		}
		return skip(fmt.Sprintf("lstat: %s", err))
	}
	if info.IsDir() {
		return FileEntry{}, nil, false // scoped incremental handles files; dirs via deep scan
	}
	if !info.Mode().IsRegular() {
		return skip("not a regular file")
	}
	if validateSMBName && hasSMBForbiddenChar(filepath.Base(abs)) {
		return skip("filename contains SMB-forbidden characters")
	}
	size := info.Size()
	mtime := info.ModTime().Unix()
	hash := ""
	switch policy {
	case HashNever:
	case HashAlways:
		h, hashErr := hashFile()
		if hashErr != nil {
			return skip(fmt.Sprintf("hash file: %s", hashErr))
		}
		hash = h
	default: // HashIfChanged
		if row, ok := manifest[rel]; ok && row.Size == size && row.Mtime == mtime && mtime < scanStart {
			hash = row.Hash
		} else {
			h, hashErr := hashFile()
			if hashErr != nil {
				return skip(fmt.Sprintf("hash file: %s", hashErr))
			}
			hash = h
		}
	}
	return FileEntry{Path: rel, AbsPath: abs, Size: size, Mtime: mtime, Hash: hash, ModTime: info.ModTime(), Info: info}, nil, true
}
