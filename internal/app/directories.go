package app

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/text/unicode/norm"

	"syncd/internal/fs"
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
	local, remote, err := openTrees(localRoot, remoteRoot)
	if err != nil {
		logger.Warn("open directories", map[string]any{"error": err.Error()})
		return nil, nil, 1
	}
	defer local.Close()
	defer remote.Close()
	matcher, err := local.LoadIgnoreMatcher()
	if err != nil {
		logger.Warn("load ignore rules", map[string]any{"error": err.Error()})
		return nil, nil, 1
	}
	return applyDirOps(logger, local, remote, planDirectories(localDirs, remoteDirs, dirBaseline, skipped), dirRemoval{matcher: matcher})
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
// (removal is non-recursive and refuses a non-empty directory), and a
// failed op leaves the baseline unchanged for that path.
//
// A directory removal that is refused only because of ignored entries is
// handled by rm (see dirRemoval.remove): junk is cleared and the directory
// removed, or the directory is kept without counting as a failure.
func applyDirOps(logger *runtime.Logger, localRoot, remoteRoot *fs.Tree, ops []DirOp, rm dirRemoval) (toAdd, toRemove []string, failures int) {
	for _, op := range ops {
		var err error
		var what string
		var kept bool
		switch op.Kind {
		case dirBaselineAdd:
			toAdd = append(toAdd, op.Path)
			continue
		case dirBaselineRemove:
			toRemove = append(toRemove, op.Path)
			continue
		case DirRemoveLocal:
			kept, err = rm.remove(localRoot, op.Path)
			what = "failed to remove local directory"
		case DirRemoveRemote:
			kept, err = rm.remove(remoteRoot, op.Path)
			what = "failed to remove remote directory"
		case DirCreateLocal:
			what, err = "failed to create local directory", localRoot.MkdirAll(filepath.FromSlash(op.Path))
		case DirCreateRemote:
			what, err = "failed to create remote directory", remoteRoot.MkdirAll(filepath.FromSlash(op.Path))
		}
		if err != nil {
			failures++
			logger.Error(what, map[string]any{"path": op.Path, "error": err.Error()})
			continue
		}
		if kept {
			// Baseline unchanged, so the other side does not recreate it;
			// re-planned (and re-kept) each cycle until the files go.
			logger.Info("kept a directory deleted on the other side: it still holds ignored files", map[string]any{"path": op.Path, "op": string(op.Kind)})
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
// deepest-first, and non-recursive removal leaves non-empty directories intact.
func cleanupEmptiedDirs(localRoot, remoteRoot string, paths []string) {
	local, remote, err := openTrees(localRoot, remoteRoot)
	if err != nil {
		return
	}
	defer local.Close()
	defer remote.Close()
	cleanupEmptiedDirsInTrees(local, remote, paths)
}

func cleanupEmptiedDirsInTrees(localRoot, remoteRoot *fs.Tree, deletedRelPaths []string) {
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
		_ = removeEmptyDirInRoot(localRoot, relDir)
		_ = removeEmptyDirInRoot(remoteRoot, relDir)
	}
}

// dirRemoval carries what removing a directory deleted on the other side
// needs beyond the trees. Ignored entries are never scanned, so the planner
// cannot see that such a directory is not really empty; matcher identifies
// them, and trash receives the junk that may be cleared.
type dirRemoval struct {
	matcher *fs.IgnoreMatcher
	trash   *fs.Trash
}

// isDisposableJunk names the ignored files that may be deleted so that a
// directory deleted on the other side can go: Finder's .DS_Store and
// AppleDouble ("._*") metadata, which only describe the directory and its
// (already deleted) files. Finder leaves .DS_Store in every folder it
// opens, so without this nearly every deleted folder on a Mac was refused
// with "directory not empty" on every cycle.
func isDisposableJunk(name string) bool {
	return name == ".DS_Store" || strings.HasPrefix(name, "._")
}

// remove removes directory rel from root if it is empty. If it is not, and
// every remaining entry is ignored:
//
//   - all of them disposable junk (isDisposableJunk, regular files): they go
//     through the trash like any other sync-driven delete, then the
//     directory is removed;
//   - anything else: the directory is kept (kept=true, err=nil). An ignored
//     file is never synced, so it is data that exists only here; deleting
//     it to remove a directory would destroy it.
//
// A directory still holding anything the sync tracks is a real failure,
// returned as removeEmptyDirInRoot's error: its files are reconciled first,
// and the directory is retried next cycle.
func (rm dirRemoval) remove(root *fs.Tree, rel string) (kept bool, err error) {
	err = removeEmptyDirInRoot(root, rel)
	if !isNotEmpty(err) || rm.matcher == nil {
		return false, err
	}
	entries, listErr := root.ReadDir(filepath.FromSlash(rel))
	if listErr != nil {
		return false, err
	}
	var junk []string
	for _, e := range entries {
		child := path.Join(rel, norm.NFC.String(e.Name()))
		if !rm.matcher.MatchPathOrAncestor(child, e.IsDir()) {
			return false, err
		}
		if e.Type().IsRegular() && isDisposableJunk(e.Name()) {
			junk = append(junk, e.Name())
		}
	}
	if len(junk) < len(entries) {
		return true, nil
	}
	for _, name := range junk {
		if err := rm.trash.RemoveFrom(root, filepath.Join(filepath.FromSlash(rel), name), nil); err != nil {
			return false, fmt.Errorf("remove %s: %w", path.Join(rel, name), err)
		}
	}
	return false, removeEmptyDirInRoot(root, rel)
}

// isNotEmpty reports a directory removal refused for having entries (POSIX
// allows either errno).
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

func removeEmptyDirInRoot(root *fs.Tree, rel string) error {
	// Lstat verifies this is still a directory; Remove remains non-recursive.
	info, err := root.Lstat(filepath.FromSlash(rel))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", rel)
	}
	err = root.Remove(filepath.FromSlash(rel), true)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
