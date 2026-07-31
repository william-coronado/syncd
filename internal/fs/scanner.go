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

// ScanResult holds one tree scan's outcome. A per-entry problem — a symlink,
// a non-regular file, an unreadable file or subdirectory, an SMB-forbidden
// filename — never fails the scan as a whole: it's recorded in Issues and
// Skipped, and the walk continues past it. Only a failure on the root itself
// or ctx cancellation returns an error from ScanTree/ScanPaths (see their doc
// comments).
type ScanResult struct {
	Files   map[string]FileEntry
	Issues  []ScanIssue
	Skipped map[string]struct{}
	// SkippedDirs holds the root-relative path of every directory whose
	// contents could not be enumerated (e.g. permission denied) - a subset
	// of Skipped's keys, specifically the ones that are directories. Its
	// descendants were never visited, so they appear in neither Files nor
	// Skipped: reconcilers must treat every path beneath a SkippedDirs entry
	// as unknown too (see reconcile.SkipSet, which both reconcilers use for
	// this), not just the directory's own path - otherwise an unreadable
	// subdirectory's whole contents read as "deleted" on this side. Always
	// empty from ScanPaths, which never walks a subtree.
	SkippedDirs map[string]struct{}
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

// ScanTree walks root and returns every file found, hashed per policy. A
// problem with one entry — a symlink (never followed; see the ScanIssue
// reason "symlink not supported"), a device/socket/FIFO, an unreadable file,
// or an unreadable subdirectory — is recorded in the result's Issues and
// Skipped and does not stop the walk: one bad file must never keep every
// other file from syncing. Only a WalkDir error on root itself, or ctx
// cancellation, aborts the whole scan and returns an error.
func ScanTree(ctx context.Context, root string, manifest map[string]db.ManifestRow, validateSMBName bool, matcher *IgnoreMatcher, policy HashPolicy, progress ScanProgressFunc) (ScanResult, error) {
	result := ScanResult{
		Files:       make(map[string]FileEntry),
		Skipped:     make(map[string]struct{}),
		SkippedDirs: make(map[string]struct{}),
		Dirs:        make(map[string]struct{}),
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
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if walkErr != nil {
			// A failure on root itself (e.g. the root doesn't exist, or
			// vanished mid-scan) is a whole-scan problem, not a per-entry
			// one: it must abort, not be silently skipped, or a vanished SMB
			// mount would look like an empty-but-successful scan.
			if path == root {
				return walkErr
			}
			relPath, relErr := NormalizeRelativePath(root, path)
			if relErr != nil {
				relPath = path
			}
			result.Issues = append(result.Issues, ScanIssue{Path: relPath, Reason: walkErr.Error()})
			// Load-bearing order: this write to Skipped must happen before the
			// d.IsDir() check below. reconcile.SkipSet.Contains only ever
			// matches a directory's OWN path via the exact Skipped set -
			// dirPrefixes(path) never returns path itself as one of its own
			// prefixes - so if this line moved after the IsDir branch, a
			// skipped directory's own path would silently stop being treated
			// as skipped by every reconciler (see
			// TestScanTree_SkippedDirRecordedInBothSkippedAndSkippedDirs).
			result.Skipped[relPath] = struct{}{}
			if d != nil && d.IsDir() {
				// Its contents couldn't be read (e.g. permission denied) -
				// nothing more to walk under it. Record it as a skipped
				// DIRECTORY too (not just an exact skipped path): its
				// descendants are never visited, so reconcile.SkipSet needs
				// this to treat every path beneath it as unknown as well.
				result.SkippedDirs[relPath] = struct{}{}
				return filepath.SkipDir
			}
			return nil
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
		// Symlinks are never followed: syncd copies file contents, so
		// following one would silently materialize a full copy at the
		// destination (and could dereference outside the sync root), and a
		// symlink recorded as a regular file would carry an lstat size that
		// disagrees with its followed-content hash. d.Type() is already in
		// hand from readdir, so this costs no extra stat.
		if d.Type()&os.ModeSymlink != 0 {
			result.Issues = append(result.Issues, ScanIssue{Path: relPath, Reason: "symlink not supported"})
			result.Skipped[relPath] = struct{}{}
			return nil
		}
		// Devices, sockets, and FIFOs are skipped the same way - hashing a
		// FIFO would block forever waiting for a writer.
		if !d.Type().IsRegular() {
			result.Issues = append(result.Issues, ScanIssue{Path: relPath, Reason: "not a regular file"})
			result.Skipped[relPath] = struct{}{}
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
			// Per-entry: e.g. the file vanished between readdir and stat.
			result.Issues = append(result.Issues, ScanIssue{Path: relPath, Reason: fmt.Sprintf("stat file: %s", err)})
			result.Skipped[relPath] = struct{}{}
			return nil
		}
		size := info.Size()
		mtime := info.ModTime().Unix()

		hash := ""
		switch policy {
		case HashNever:
			// leave hash empty; reconciler compares on size+mtime
		case HashAlways:
			h, hashErr := FileBLAKE3(path)
			if hashErr != nil {
				result.Issues = append(result.Issues, ScanIssue{Path: relPath, Reason: fmt.Sprintf("hash file: %s", hashErr)})
				result.Skipped[relPath] = struct{}{}
				return nil
			}
			hash = h
		default: // HashIfChanged
			if row, ok := manifest[relPath]; ok && row.Size == size && row.Mtime == mtime && mtime < scanStart {
				hash = row.Hash
			} else {
				h, hashErr := FileBLAKE3(path)
				if hashErr != nil {
					result.Issues = append(result.Issues, ScanIssue{Path: relPath, Reason: fmt.Sprintf("hash file: %s", hashErr)})
					result.Skipped[relPath] = struct{}{}
					return nil
				}
				hash = h
			}
		}
		recordScannedFile(&result, relPath, FileEntry{
			Path:    relPath,
			AbsPath: path,
			Size:    size,
			Mtime:   mtime,
			Hash:    hash,
		})
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

// recordScannedFile adds entry to result.Files at relPath, unless relPath is
// already occupied (by a previous entry, or already flagged in
// result.Skipped) - which can only happen when two physically different
// on-disk names normalize (via NormalizeRelativePath) to the same relPath:
// an NFC vs. NFD spelling of visually-identical text. Linux and Samba store
// filenames as literal byte sequences with no normalization (unlike
// HFS+/APFS, which has traditionally forced NFD on write), so both spellings
// can genuinely coexist as distinct directory entries there. Silently
// letting whichever the walk visits last win would make the other
// permanently and silently invisible to reconciliation - never synced,
// never deleted, just gone (review finding, GPT-5.6 Sol). Neither can be
// distinguished by relPath alone (that's the whole problem), so both are
// excluded via Issues/Skipped instead, the same as any other per-entry scan
// problem.
//
// The check is against result.Skipped, not just result.Files: the first
// collision deletes the earlier entry from Files, so a naive
// "already in Files" check would miss a THIRD entry at the same relPath and
// silently write it back in, un-skipping a collision that's still a
// collision.
func recordScannedFile(result *ScanResult, relPath string, entry FileEntry) {
	if _, alreadySkipped := result.Skipped[relPath]; alreadySkipped {
		return
	}
	if _, collided := result.Files[relPath]; collided {
		delete(result.Files, relPath)
		result.Issues = append(result.Issues, ScanIssue{
			Path:   relPath,
			Reason: "duplicate path after Unicode normalization (NFC/NFD collision)",
		})
		result.Skipped[relPath] = struct{}{}
		return
	}
	result.Files[relPath] = entry
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
