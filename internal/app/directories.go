package app

import (
	"os"
	"path"
	"path/filepath"
	"sort"

	"syncd/internal/reconcile"
	"syncd/internal/runtime"
)

// reconcileDirectories mirrors directories that exist on one side but not
// the other, using a persisted baseline (dirBaseline: every directory
// confirmed present on BOTH sides as of the last cycle - see db.DirRepo) to
// distinguish "a brand-new empty directory the user is organizing ahead of
// time" (not in baseline -> create on the missing side) from "a directory
// the user deliberately deleted" (in baseline -> propagate the delete to the
// side that still has it), instead of unconditionally recreating anything
// missing on either side (review finding "Medium 9" - deleting an empty
// directory used to be silently resurrected on the next cycle).
//
// Directories that contain files are already created as a side-effect of
// file copy operations (see fs.EnsureParents), so the create branch here
// only matters for directories with no files in them at all. Returns the
// baseline additions/removals this cycle implies, for the caller to persist
// via db.DirRepo.ApplyUpdates; failures are logged and tallied rather than
// aborting the run - a directory create/delete failure never loses file
// data (os.Remove is non-recursive and refuses a non-empty directory).
//
// skipped is the scan's SkipSet (see app.mergeSkipSet): fs.ScanTree omits a
// directory it couldn't enumerate (e.g. permission denied) from Dirs
// entirely - it never got to record the directory as present, only that it
// was skipped - so without this check that directory would look exactly
// like "deleted locally" (in the baseline, absent from localDirs) and its
// still-intact remote counterpart would be deleted. A skipped directory (or
// one beneath a skipped directory) is left completely alone this cycle: no
// create, no delete, no baseline change (review finding P1 #2, Codex).
func reconcileDirectories(logger *runtime.Logger, localRoot, remoteRoot string, localDirs, remoteDirs, dirBaseline map[string]struct{}, skipped reconcile.SkipSet) (toAdd, toRemove []string, failures int) {
	allDirs := make(map[string]struct{}, len(localDirs)+len(remoteDirs)+len(dirBaseline))
	for d := range localDirs {
		allDirs[d] = struct{}{}
	}
	for d := range remoteDirs {
		allDirs[d] = struct{}{}
	}
	// A directory gone from both live scans but still in the baseline
	// (the stale-cleanup case) must still be visited, or it would never be
	// swept from dirBaseline.
	for d := range dirBaseline {
		allDirs[d] = struct{}{}
	}

	// Visited deepest-first (reverse lexicographic: any path having another
	// as a strict prefix always sorts after it, so "parent/child" is always
	// removed before "parent") - os.Remove is non-recursive, so removing a
	// parent before its still-present child would fail with ENOTEMPTY purely
	// because of iteration order, not because anything is actually wrong.
	dirs := make([]string, 0, len(allDirs))
	for d := range allDirs {
		dirs = append(dirs, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))

	for _, dir := range dirs {
		if skipped.Contains(dir) {
			continue
		}
		_, inLocal := localDirs[dir]
		_, inRemote := remoteDirs[dir]
		_, inBaseline := dirBaseline[dir]

		switch {
		case inLocal && inRemote:
			if !inBaseline {
				toAdd = append(toAdd, dir)
			}
		case inLocal && !inRemote:
			if inBaseline {
				if err := removeEmptyDir(filepath.Join(localRoot, filepath.FromSlash(dir))); err != nil {
					failures++
					logger.Error("failed to remove local directory", map[string]any{"path": dir, "error": err.Error()})
					continue
				}
				toRemove = append(toRemove, dir)
			} else if err := os.MkdirAll(filepath.Join(remoteRoot, filepath.FromSlash(dir)), 0o755); err != nil {
				failures++
				logger.Error("failed to create remote directory", map[string]any{"path": dir, "error": err.Error()})
			} else {
				toAdd = append(toAdd, dir)
			}
		case !inLocal && inRemote:
			if inBaseline {
				if err := removeEmptyDir(filepath.Join(remoteRoot, filepath.FromSlash(dir))); err != nil {
					failures++
					logger.Error("failed to remove remote directory", map[string]any{"path": dir, "error": err.Error()})
					continue
				}
				toRemove = append(toRemove, dir)
			} else if err := os.MkdirAll(filepath.Join(localRoot, filepath.FromSlash(dir)), 0o755); err != nil {
				failures++
				logger.Error("failed to create local directory", map[string]any{"path": dir, "error": err.Error()})
			} else {
				toAdd = append(toAdd, dir)
			}
		default: // gone from both sides
			if inBaseline {
				toRemove = append(toRemove, dir)
			}
		}
	}
	return toAdd, toRemove, failures
}

// removeEmptyDir removes dir if it exists, tolerating its prior absence -
// os.Remove refuses (ENOTEMPTY) if dir unexpectedly still contains entries,
// which is the desired safety: this never recurses or forces a delete.
func removeEmptyDir(dir string) error {
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// cleanupEmptiedDirs removes directories, on both local and remote, that
// held one of this run's deleted files and are now empty on that side. A
// directory that was already empty before this run - and that this run
// didn't touch - is never considered, so pre-existing empty directories the
// user keeps intentionally are left alone.
func cleanupEmptiedDirs(localRoot, remoteRoot string, deletedRelPaths []string) {
	touched := make(map[string]struct{})
	for _, relPath := range deletedRelPaths {
		dir := path.Dir(relPath)
		if dir == "." {
			continue // never remove the sync root itself
		}
		touched[dir] = struct{}{}
	}
	for relDir := range touched {
		removeIfEmpty(filepath.Join(localRoot, filepath.FromSlash(relDir)))
		removeIfEmpty(filepath.Join(remoteRoot, filepath.FromSlash(relDir)))
	}
}

func removeIfEmpty(absDir string) {
	entries, err := os.ReadDir(absDir)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(absDir)
}
