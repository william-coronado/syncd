package fs

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"syncd/internal/db"
)

var smbForbidden = []string{`\`, ":", "*", "?", `"`, "<", ">", "|"}

type ScanIssue struct {
	Path   string
	Reason string
}

type FileEntry struct {
	Path    string
	AbsPath string
	Size    int64
	Mtime   int64
	Hash    string
}

type ScanResult struct {
	Files   map[string]FileEntry
	Issues  []ScanIssue
	Skipped map[string]struct{}
	// Dirs holds every directory encountered under root (relative, NFC
	// normalized, excluding root itself), regardless of whether it contains
	// files. Used for empty-directory create/cleanup, which manifest-based
	// file reconciliation can't see on its own.
	Dirs map[string]struct{}
}

type ScanPairResult struct {
	Local  ScanResult
	Remote ScanResult
}

// ScanProgressFunc reports incremental progress from a single ScanTree call.
// scanned is the count of files scanned so far; done is true exactly once,
// on the final call after the walk completes.
type ScanProgressFunc func(scanned int, done bool)

// ScanPairProgressFunc is like ScanProgressFunc but distinguishes which side
// of the pair (side is "local" or "remote") a progress update came from,
// since ScanPair scans both concurrently.
type ScanPairProgressFunc func(side string, scanned int, done bool)

// HashPolicy controls whether ScanTree/ScanPair compute a content hash for
// each scanned file.
type HashPolicy int

const (
	// HashIfChanged hashes only when a file's size+mtime differ from the
	// manifest (default; this is the historical, always-on-sync behavior).
	HashIfChanged HashPolicy = iota
	// HashNever skips hashing entirely; FileEntry.Hash is left empty and
	// callers compare on size+mtime alone.
	HashNever
	// HashAlways hashes every scanned file regardless of manifest state.
	HashAlways
)

func ScanPair(ctx context.Context, localRoot, remoteRoot string, manifest map[string]db.ManifestRow, policy HashPolicy, progress ScanPairProgressFunc) (ScanPairResult, error) {
	// Ignore rules live in the local root's .syncdignore and are applied
	// identically to both trees, so a pattern excludes a path regardless of
	// which side it actually exists on.
	matcher, err := LoadIgnoreMatcher(localRoot)
	if err != nil {
		return ScanPairResult{}, err
	}

	var localProgress, remoteProgress ScanProgressFunc
	if progress != nil {
		localProgress = func(scanned int, done bool) { progress("local", scanned, done) }
		remoteProgress = func(scanned int, done bool) { progress("remote", scanned, done) }
	}

	type item struct {
		key string
		res ScanResult
		err error
	}
	ch := make(chan item, 2)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		res, err := ScanTree(ctx, localRoot, manifest, true, matcher, policy, localProgress)
		ch <- item{key: "local", res: res, err: err}
	}()
	go func() {
		defer wg.Done()
		res, err := ScanTree(ctx, remoteRoot, manifest, false, matcher, policy, remoteProgress)
		ch <- item{key: "remote", res: res, err: err}
	}()
	wg.Wait()
	close(ch)

	var out ScanPairResult
	for it := range ch {
		if it.err != nil {
			return ScanPairResult{}, it.err
		}
		if it.key == "local" {
			out.Local = it.res
		} else {
			out.Remote = it.res
		}
	}
	return out, nil
}

// progressReportInterval caps how often ScanTree calls progress during a
// walk, so a large tree reports periodically instead of going silent until
// it's entirely done (which, over a slow network mount, is indistinguishable
// from having hung) without spamming a line per file.
const progressReportInterval = 3 * time.Second

func ScanTree(ctx context.Context, root string, manifest map[string]db.ManifestRow, validateSMBName bool, matcher *IgnoreMatcher, policy HashPolicy, progress ScanProgressFunc) (ScanResult, error) {
	result := ScanResult{
		Files:   make(map[string]FileEntry),
		Skipped: make(map[string]struct{}),
		Dirs:    make(map[string]struct{}),
	}
	// mtime has only 1-second resolution, so a file touched within the same
	// second as this scan can't be trusted to have a stable mtime yet (the
	// classic "racy mtime" problem: two edits in the same second can leave
	// size and mtime both unchanged). Only reuse a cached manifest hash for
	// files whose mtime is strictly before this scan started.
	scanStart := time.Now().Unix()
	scanned := 0
	lastReport := time.Now()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if path == root {
			return nil
		}
		relPath, err := NormalizeRelativePath(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if matcher.Match(relPath, true) {
				return filepath.SkipDir
			}
			result.Dirs[relPath] = struct{}{}
			return nil
		}
		if matcher.Match(relPath, false) {
			return nil
		}
		if validateSMBName && hasSMBForbiddenChar(d.Name()) {
			result.Issues = append(result.Issues, ScanIssue{
				Path:   relPath,
				Reason: "filename contains SMB-forbidden characters",
			})
			result.Skipped[relPath] = struct{}{}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat file %s: %w", path, err)
		}
		size := info.Size()
		mtime := info.ModTime().Unix()

		hash := ""
		switch policy {
		case HashNever:
			// leave hash empty; reconciler compares on size+mtime
		case HashAlways:
			hash, err = FileBLAKE3(path)
			if err != nil {
				return fmt.Errorf("hash file %s: %w", path, err)
			}
		default: // HashIfChanged
			if row, ok := manifest[relPath]; ok && row.Size == size && row.Mtime == mtime && mtime < scanStart {
				hash = row.Hash
			} else {
				hash, err = FileBLAKE3(path)
				if err != nil {
					return fmt.Errorf("hash file %s: %w", path, err)
				}
			}
		}
		result.Files[relPath] = FileEntry{
			Path:    relPath,
			AbsPath: path,
			Size:    size,
			Mtime:   mtime,
			Hash:    hash,
		}
		scanned++
		if progress != nil && time.Since(lastReport) >= progressReportInterval {
			progress(scanned, false)
			lastReport = time.Now()
		}
		return nil
	})
	if err != nil {
		return ScanResult{}, fmt.Errorf("walk tree %s: %w", root, err)
	}
	if progress != nil {
		progress(scanned, true)
	}
	return result, nil
}

func hasSMBForbiddenChar(name string) bool {
	for _, bad := range smbForbidden {
		if strings.Contains(name, bad) {
			return true
		}
	}
	return false
}

func EnsureParents(path string) error {
	parent := filepath.Dir(path)
	return os.MkdirAll(parent, 0o755)
}
