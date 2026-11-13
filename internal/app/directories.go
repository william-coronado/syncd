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
	return applyDirOps(logger, localRoot, remoteRoot, planDirectories(localDirs, remoteDirs, dirBaseline, skipped))
}

// DirOpKind is one directory operation planDirectories can decide on. The
// first four change a tree and are shown in the --hash confirm/dry-run
// summary; the baseline-only kinds just update db.DirRepo.
type DirOpKind string

const (
	DirCreateLocal  DirOpKind = "MKDIR_LOCAL"
	DirCreateRemote DirOpKind = "MKDIR_REMOTE"
	DirRemoveLocal  DirOpKind = "RMDIR_LOCAL"
	DirRemoveRemote DirOpKind = "RMDIR_REMOTE"
	// dirBaselineAdd: present on both sides but not yet baselined.
	dirBaselineAdd DirOpKind = "baseline-add"
	// dirBaselineRemove: gone from both sides, still baselined.
	dirBaselineRemove DirOpKind = "baseline-remove"
)

// DirOp is a planned directory operation on Path (root-relative).
type DirOp struct {
	Kind DirOpKind
	Path string
}

// changesTree reports whether op creates or removes a real directory (as
// opposed to only updating the baseline).
func (op DirOp) changesTree() bool {
	return op.Kind != dirBaselineAdd && op.Kind != dirBaselineRemove
}

// visibleDirOps returns just the ops that change a tree - what a summary
// should list.
func visibleDirOps(ops []DirOp) []DirOp {
	var out []DirOp
	for _, op := range ops {
		if op.changesTree() {
			out = append(out, op)
		}
	}
	return out
}

// planDirectories decides reconcileDirectories' operations without
// touching disk, so they can be shown for confirmation first (see
// RobustParams.ConfirmPlan). Ops are ordered deepest-first (reverse
// lexicographic: "parent/child" always before "parent") - os.Remove is
// non-recursive, so removing a parent before its still-present child would
// fail purely because of iteration order.
func planDirectories(localDirs, remoteDirs, dirBaseline map[string]struct{}, skipped reconcile.SkipSet) []DirOp {
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
	dirs := make([]string, 0, len(allDirs))
	for d := range allDirs {
		dirs = append(dirs, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))

	var ops []DirOp
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
				ops = append(ops, DirOp{dirBaselineAdd, dir})
			}
		case inLocal && !inRemote:
			if inBaseline {
				ops = append(ops, DirOp{DirRemoveLocal, dir})
			} else {
				ops = append(ops, DirOp{DirCreateRemote, dir})
			}
		case !inLocal && inRemote:
			if inBaseline {
				ops = append(ops, DirOp{DirRemoveRemote, dir})
			} else {
				ops = append(ops, DirOp{DirCreateLocal, dir})
			}
		default: // gone from both sides
			if inBaseline {
				ops = append(ops, DirOp{dirBaselineRemove, dir})
			}
		}
	}
	return ops
}

// applyDirOps performs planDirectories' ops in order and returns the
// baseline additions/removals they imply, for the caller to persist via
// db.DirRepo.ApplyUpdates. Failures are logged and tallied rather than
// aborting - a directory create/delete failure never loses file data
// (os.Remove is non-recursive and refuses a non-empty directory), and a
// failed op leaves the baseline unchanged for that path.
func applyDirOps(logger *runtime.Logger, localRoot, remoteRoot string, ops []DirOp) (toAdd, toRemove []string, failures int) {
	abs := func(root, rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
	for _, op := range ops {
		var err error
		var what string
		switch op.Kind {
		case dirBaselineAdd:
			toAdd = append(toAdd, op.Path)
			continue
		case dirBaselineRemove:
			toRemove = append(toRemove, op.Path)
			continue
		case DirRemoveLocal:
			what, err = "failed to remove local directory", removeEmptyDir(abs(localRoot, op.Path))
		case DirRemoveRemote:
			what, err = "failed to remove remote directory", removeEmptyDir(abs(remoteRoot, op.Path))
		case DirCreateLocal:
			what, err = "failed to create local directory", os.MkdirAll(abs(localRoot, op.Path), 0o755)
		case DirCreateRemote:
			what, err = "failed to create remote directory", os.MkdirAll(abs(remoteRoot, op.Path), 0o755)
		}
		if err != nil {
			failures++
			logger.Error(what, map[string]any{"path": op.Path, "error": err.Error()})
			continue
		}
		switch op.Kind {
		case DirRemoveLocal, DirRemoveRemote:
			toRemove = append(toRemove, op.Path)
		default:
			toAdd = append(toAdd, op.Path)
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
// held one of this run's deleted files (or held a directory this run
// emptied out) and are now empty on that side. A directory that was already
// empty before this run - and that this run didn't touch - is never
// considered, so pre-existing empty directories the user keeps
// intentionally are left alone.
//
// Every ancestor of each deleted path is a candidate, not just its immediate
// parent: removing "sub/nested/emptied" (itself emptied by a delete) can in
// turn empty "sub/nested", and so on up the tree. Candidates are visited
// deepest-first (same reasoning as reconcileDirectories' nested-deletion
// ordering) so a now-empty parent is reconsidered only after its child is
// actually gone; removeIfEmpty's own os.ReadDir check stops the cascade the
// moment it hits an ancestor that still holds something else (review
// finding, Gemini 3.6).
func cleanupEmptiedDirs(localRoot, remoteRoot string, deletedRelPaths []string) {
	touched := make(map[string]struct{})
	for _, relPath := range deletedRelPaths {
		for dir := path.Dir(relPath); dir != "."; dir = path.Dir(dir) {
			touched[dir] = struct{}{}
		}
	}
	dirs := make([]string, 0, len(touched))
	for d := range touched {
		dirs = append(dirs, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))

	for _, relDir := range dirs {
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
