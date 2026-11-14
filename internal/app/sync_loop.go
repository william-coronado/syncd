package app

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"syncd/internal/db"
	"syncd/internal/execute"
	"syncd/internal/fs"
	"syncd/internal/reconcile"
	"syncd/internal/runtime"
)

// RobustParams configures a single robust, hash-based three-way sync cycle
// run by RunRobustOnce. It is the shared engine behind the `syncd watch`
// daemon (internal/watch) and the `syncd sync --hash` bridge (see
// sync_once.go).
type RobustParams struct {
	LocalRoot, RemoteRoot string
	Workers               int
	Repo                  *db.ManifestRepo
	DB                    *sql.DB
	Logger                *runtime.Logger
	Policy                fs.HashPolicy // HashIfChanged for polls, HashAlways for deep verify
	// ConfirmPlan, if set, is shown the non-NoOp actions RunRobustOnce is
	// about to execute, plus the directory creates/removes it will make
	// (dirOps lists only those that change a tree), and may decline them
	// (return false), in which case
	// RunRobustOnce returns ErrAborted having changed nothing - no file
	// action, no directory create/remove, no manifest write. It is how
	// `syncd sync --hash`'s interactive confirm approves exactly the plan
	// that then runs, rather than a separate preview scan's plan. nil
	// (watch, --yes) means "always proceed".
	ConfirmPlan func(actions []reconcile.Action, dirOps []DirOp) bool
	// MaxDelete caps how many tracked files one cycle may delete from
	// either side; a cycle over it returns reconcile.ErrDeleteLimit having
	// changed nothing. The zero value imposes no limit.
	MaxDelete reconcile.DeleteLimit
	// ApproveDeletes, if set, is asked when a cycle exceeds MaxDelete and
	// may let that one cycle run anyway (`syncd approve-deletes`).
	ApproveDeletes func() bool
	// Trash receives deleted files that live on a local disk; nil removes
	// them outright.
	Trash *fs.Trash
}

// checkDeleteLimit enforces p.MaxDelete against the deletes in actions.
// total (the number of tracked files, the denominator for a percentage
// limit) is only fetched when there is something to check.
func (p RobustParams) checkDeleteLimit(actions []reconcile.Action, total func() (int, error)) error {
	if p.MaxDelete.Unlimited() {
		return nil
	}
	var local, remote int
	for _, a := range actions {
		switch a.Type {
		case reconcile.ActionDeleteLocal:
			local++
		case reconcile.ActionDeleteRemote:
			remote++
		}
	}
	if local == 0 && remote == 0 {
		return nil
	}
	n, err := total()
	if err != nil {
		return err
	}
	err = p.MaxDelete.Check("local", local, n)
	if err == nil {
		err = p.MaxDelete.Check("remote", remote, n)
	}
	if err != nil && p.ApproveDeletes != nil && p.ApproveDeletes() {
		p.Logger.Warn("delete limit exceeded, but this cycle was approved; proceeding", map[string]any{
			"delete_local": local, "delete_remote": remote, "tracked": n,
		})
		return nil
	}
	return err
}

// robustHardError marks an error from RunRobustOnce that happened before -
// or in the course of committing - a run's results: Repo.LoadAll,
// fs.ScanPair, or Repo.ApplyUpdates. It is distinct from the
// execute.RunActions runErr (e.g. context cancellation), which RunRobustOnce
// returns bare after the manifest commit has already been attempted.
//
// Callers use this distinction to reproduce the pre-RunRobustOnce
// error-reporting shape of their own call site: a hard failure is reported
// immediately (no run-summary logging), using that call site's own
// (possibly stage-specific) wrapping of the plain underlying error; a runErr
// or failures>0 result is reported only after a run summary is emitted,
// wrapped as "sync run did not complete" / "sync completed with N failed
// action(s)" respectively. See sync_once.go's runRobustOnce for the call
// site that branches on this.
type robustHardError struct {
	stage string // "load manifest", "scan", or "apply manifest updates"
	err   error
}

func (e *robustHardError) Error() string { return e.err.Error() }
func (e *robustHardError) Unwrap() error { return e.err }

// RunRobustOnce performs one scan -> plan -> execute -> manifest-update
// (plus empty-dir create/cleanup) cycle of the robust, hash-based three-way
// engine and returns the executed plan and the number of failed actions.
//
// The returned error is either nil, a *robustHardError (LoadAll/ScanPair/
// ApplyUpdates failed - see its doc comment), or the bare execute.RunActions
// runErr (e.g. context cancellation) after a manifest commit was attempted.
func RunRobustOnce(ctx context.Context, p RobustParams) (reconcile.Plan, int, error) {
	manifest, err := p.Repo.LoadAll(ctx)
	if err != nil {
		return reconcile.Plan{}, 0, &robustHardError{stage: "load manifest", err: err}
	}

	dirRepo := db.NewDirRepo(p.DB)
	dirBaseline, err := dirRepo.LoadAll(ctx)
	if err != nil {
		return reconcile.Plan{}, 0, &robustHardError{stage: "load dir manifest", err: err}
	}

	scans, err := fs.ScanPair(ctx, p.LocalRoot, p.RemoteRoot, manifest, p.Policy, scanProgressLogger(p.Logger))
	if err != nil {
		return reconcile.Plan{}, 0, &robustHardError{stage: "scan", err: err}
	}
	logScanIssues(p.Logger, "local", scans.Local.Issues)
	logScanIssues(p.Logger, "remote", scans.Remote.Issues)

	localFiles, remoteFiles := scans.Local.Files, scans.Remote.Files
	skipped := mergeSkipSet(scans.Local, scans.Remote)
	plan := reconcile.BuildPlan(localFiles, remoteFiles, manifest, skipped)

	execActions := make([]reconcile.Action, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		if action.Type != reconcile.ActionNoOp {
			execActions = append(execActions, action)
		}
	}

	// Planned from the scan now, applied after the file actions below (the
	// same scan-time view the apply step always used), so the confirm can
	// show directory operations too.
	dirOps := planDirectories(scans.Local.Dirs, scans.Remote.Dirs, dirBaseline, skipped)
	if err := p.checkDeleteLimit(execActions, func() (int, error) { return len(manifest), nil }); err != nil {
		return plan, 0, err
	}
	if p.ConfirmPlan != nil && !p.ConfirmPlan(execActions, visibleDirOps(dirOps)) {
		return plan, 0, ErrAborted
	}

	state := execute.SyncState{
		LocalRoot:   p.LocalRoot,
		RemoteRoot:  p.RemoteRoot,
		LocalFiles:  localFiles,
		RemoteFiles: remoteFiles,
		Trash:       p.Trash,
	}
	// runErr (e.g. context cancellation) is deliberately not returned yet:
	// results may be partial, but whatever did complete should still be
	// committed to the manifest below before we report failure.
	results, runErr := execute.RunActions(ctx, p.Workers, state, execActions)

	upserts, deletes, failures := collectManifestUpdates(results, p.Logger)

	dirsToAdd, dirsToRemove, dirFailures := applyDirOps(p.Logger, p.LocalRoot, p.RemoteRoot, dirOps)
	failures += dirFailures
	cleanupEmptiedDirs(p.LocalRoot, p.RemoteRoot, deletes)

	noopUpserts, noopDeletes := noopManifestUpdates(plan, localFiles, remoteFiles, manifest)
	upserts = append(upserts, noopUpserts...)
	deletes = append(deletes, noopDeletes...)

	if err := commitManifestUpdates(ctx, p.Repo, upserts, deletes); err != nil {
		return plan, failures, &robustHardError{stage: "apply manifest updates", err: err}
	}
	// Detached context for the same reason as commitManifestUpdates: a run
	// cancelled mid-execute.RunActions must not lose directory baseline
	// updates that already-completed create/removes imply.
	if err := dirRepo.ApplyUpdates(context.WithoutCancel(ctx), dirsToAdd, dirsToRemove); err != nil {
		return plan, failures, &robustHardError{stage: "apply dir manifest updates", err: err}
	}

	return plan, failures, runErr
}

// commitManifestUpdates persists this cycle's manifest upserts/deletes,
// using a context detached from cancellation (context.WithoutCancel), not
// ctx itself. execute.RunActions' runErr (e.g. ctx cancelled mid-batch by
// SIGINT) is deliberately not checked before this is called - whatever
// actions DID complete should still be recorded - but ctx may already be
// Done by the time we get here, and ApplyUpdates' BeginTx fails immediately
// against a cancelled context, which would silently drop real,
// already-applied filesystem changes from the manifest (review finding
// "Medium 7"). It is a no-op (no transaction opened) when there is nothing
// to commit.
func commitManifestUpdates(ctx context.Context, repo *db.ManifestRepo, upserts []db.ManifestRow, deletes []string) error {
	if len(upserts) == 0 && len(deletes) == 0 {
		return nil
	}
	return repo.ApplyUpdates(context.WithoutCancel(ctx), upserts, deletes)
}

// noopManifestUpdates derives the manifest upserts/deletes that
// ActionNoOp plan entries require on their own, since they are excluded from
// execActions and never reach execute.RunActions (there is nothing to copy -
// the point of NoOp is that both sides already agree), and so would
// otherwise never touch the manifest at all:
//
//   - a live matching pair (present, hash-equal, on both sides) that isn't
//     already correctly baselined gets a manifest upsert. Without this, a
//     pair that has been identical since its very first scan - never having
//     gone through an Upload/Download/Conflict action - stays untracked
//     forever, so a later one-sided delete falls into BuildPlan's
//     `!inManifest` branch and gets re-uploaded ("resurrected") instead of
//     propagated as a delete (review finding "High 4").
//   - a stale manifest row for a path now absent from both sides (BuildPlan's
//     "already deleted on both sides" NoOp) is removed, so it doesn't linger
//     in the manifest indefinitely.
func noopManifestUpdates(plan reconcile.Plan, localFiles, remoteFiles map[string]fs.FileEntry, manifest map[string]db.ManifestRow) (upserts []db.ManifestRow, deletes []string) {
	now := time.Now().Unix()
	for _, action := range plan.Actions {
		if action.Type != reconcile.ActionNoOp {
			continue
		}
		path := action.Path
		l, inLocal := localFiles[path]
		r, inRemote := remoteFiles[path]
		if inLocal && inRemote {
			row, ok := manifest[path]
			remoteSize, remoteMtime := row.RemoteStat()
			if !ok || row.Hash != l.Hash || row.Size != l.Size || row.Mtime != l.Mtime || remoteSize != r.Size || remoteMtime != r.Mtime {
				// Record each side's own stat: an identical pair can still
				// differ in mtime (copied without preserving it), and the
				// remote HashIfChanged cache needs the REMOTE stat to hit.
				upserts = append(upserts, db.ManifestRow{
					Path: path, Size: l.Size, Mtime: l.Mtime, Hash: l.Hash, LastSynced: now,
					RemoteSize: r.Size, RemoteMtime: r.Mtime,
				})
			}
			continue
		}
		if !inLocal && !inRemote {
			if _, ok := manifest[path]; ok {
				deletes = append(deletes, path)
			}
		}
	}
	return upserts, deletes
}

// collectManifestUpdates dedups a batch of execute.Result into the final set
// of manifest upserts and deletes to commit, and counts failed actions. When
// the same path appears more than once across results (e.g. a delete
// followed by a recreate within one run), the later update wins and any
// earlier one for that path is discarded - hence the seen-maps rather than
// appending directly. Extracted from RunRobustOnce so RunRobustScoped can
// reuse the exact same dedup logic; behavior is unchanged from its inline
// original.
func collectManifestUpdates(results []execute.Result, logger *runtime.Logger) (upserts []db.ManifestRow, deletes []string, failures int) {
	seenUpserts := make(map[string]db.ManifestRow)
	seenDeletes := make(map[string]struct{})
	for _, res := range results {
		if res.Error != nil {
			failures++
			logger.Error("action failed", map[string]any{
				"action": res.Action.Type,
				"path":   res.Action.Path,
				"error":  res.Error.Error(),
			})
			continue
		}
		for _, update := range res.Updates {
			if update.Delete {
				seenDeletes[update.Path] = struct{}{}
				delete(seenUpserts, update.Path)
				continue
			}
			seenUpserts[update.Path] = update.Row
			delete(seenDeletes, update.Path)
		}
	}

	for _, row := range seenUpserts {
		upserts = append(upserts, row)
	}
	for path := range seenDeletes {
		deletes = append(deletes, path)
	}
	sort.Slice(upserts, func(i, j int) bool { return upserts[i].Path < upserts[j].Path })
	sort.Strings(deletes)
	return upserts, deletes, failures
}

// RunRobustScoped reconciles ONLY relPaths (an fsnotify batch) rather than
// the whole tree. A file path is stat/hashed on both sides via fs.ScanPaths
// - no tree walk. A path that is a DIRECTORY on either side (fsnotify
// reports only the top-level path of a recursive delete, a directory
// renamed in or out, or a directory created with contents) is expanded:
// that subtree alone is walked on whichever side(s) it exists
// (fs.ScanSubtree), its manifest rows are loaded by prefix, and its
// directories are reconciled against the directory baseline - so a
// recursive delete or a directory rename propagates in this cycle instead
// of waiting for the next remote poll. The same prefix load is done for a
// path that is now absent locally, since it may have been a directory.
//
// It otherwise mirrors RunRobustOnce's scan -> plan -> execute ->
// manifest-commit flow. Directories outside the batch's subtrees (e.g. new
// empty directories elsewhere) are left to the periodic full cycles.
func RunRobustScoped(ctx context.Context, p RobustParams, relPaths map[string]struct{}) (reconcile.Plan, int, error) {
	matcher, err := fs.LoadIgnoreMatcher(p.LocalRoot)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	subtrees, prefixes, unknown := scopedSubtrees(p.LocalRoot, p.RemoteRoot, relPaths, matcher)

	// Only this batch's rows (plus everything beneath its directory
	// paths): loading the whole manifest per fsnotify batch made the
	// scoped (hot) path's cost scale with the tree, not the batch.
	scopedManifest, err := p.Repo.LoadPaths(ctx, relPaths)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	for _, prefix := range prefixes {
		rows, err := p.Repo.LoadPrefix(ctx, prefix)
		if err != nil {
			return reconcile.Plan{}, 0, err
		}
		for k, v := range rows {
			scopedManifest[k] = v
		}
	}
	remoteManifest := db.RemoteStatView(scopedManifest)

	localScan, err := fs.ScanPaths(p.LocalRoot, relPaths, scopedManifest, p.Policy, true, matcher)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	remoteScan, err := fs.ScanPaths(p.RemoteRoot, relPaths, remoteManifest, p.Policy, false, matcher)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	for _, st := range subtrees {
		if st.local {
			sub, err := fs.ScanSubtree(ctx, p.LocalRoot, st.rel, scopedManifest, true, matcher, p.Policy)
			if err != nil {
				return reconcile.Plan{}, 0, &robustHardError{stage: "scan", err: err}
			}
			mergeScanResult(&localScan, sub)
		}
		if st.remote {
			sub, err := fs.ScanSubtree(ctx, p.RemoteRoot, st.rel, remoteManifest, false, matcher, p.Policy)
			if err != nil {
				return reconcile.Plan{}, 0, &robustHardError{stage: "scan", err: err}
			}
			mergeScanResult(&remoteScan, sub)
		}
	}
	// A batch path that couldn't be stat'd on one side (e.g. an ancestor
	// lost search permission) is unknown on that side - and so is
	// everything beneath it. Mark it as a skipped DIRECTORY, not just an
	// exact skip (all ScanPaths records for it), so SkipSet suppresses
	// every descendant too: otherwise the other side's expanded subtree
	// plus the manifest prefix would read the unreadable side's tracked
	// children as deleted, and delete the intact copies on the readable
	// side.
	for _, u := range unknown {
		localScan.Skipped[u.rel] = struct{}{}
		localScan.SkippedDirs[u.rel] = struct{}{}
		issue := fs.ScanIssue{Path: u.rel, Reason: fmt.Sprintf("lstat: %s; treating its whole subtree as unknown this cycle", u.err)}
		if u.side == "remote" {
			remoteScan.Issues = append(remoteScan.Issues, issue)
		} else {
			localScan.Issues = append(localScan.Issues, issue)
		}
	}
	logScanIssues(p.Logger, "local", localScan.Issues)
	logScanIssues(p.Logger, "remote", remoteScan.Issues)
	skipped := mergeSkipSet(localScan, remoteScan)
	plan := reconcile.BuildPlan(localScan.Files, remoteScan.Files, scopedManifest, skipped)
	execActions := nonNoOpActions(plan)
	// The scoped manifest holds only this batch's rows, so a percentage
	// limit is measured against the whole manifest's size.
	if err := p.checkDeleteLimit(execActions, func() (int, error) { return p.Repo.Count(ctx) }); err != nil {
		return plan, 0, err
	}

	// Loaded BEFORE executing anything: once file actions have run, nothing
	// that can fail on a cancelled ctx may stand between them and
	// commitManifestUpdates, or completed changes would go unrecorded in
	// the three-way baseline.
	dirRepo := db.NewDirRepo(p.DB)
	var dirBaseline map[string]struct{}
	if len(subtrees) > 0 {
		if dirBaseline, err = dirRepo.LoadAll(ctx); err != nil {
			return plan, 0, &robustHardError{stage: "load dir manifest", err: err}
		}
	}

	state := execute.SyncState{LocalRoot: p.LocalRoot, RemoteRoot: p.RemoteRoot, LocalFiles: localScan.Files, RemoteFiles: remoteScan.Files, Trash: p.Trash}
	results, runErr := execute.RunActions(ctx, p.Workers, state, execActions)
	scopedActionsDone()
	upserts, deletes, failures := collectManifestUpdates(results, p.Logger)

	var dirsToAdd, dirsToRemove []string
	if len(subtrees) > 0 {
		var dirFailures int
		dirsToAdd, dirsToRemove, dirFailures = reconcileDirectories(p.Logger, p.LocalRoot, p.RemoteRoot,
			localScan.Dirs, remoteScan.Dirs, baselineWithin(dirBaseline, subtrees), skipped)
		failures += dirFailures
	}
	cleanupEmptiedDirs(p.LocalRoot, p.RemoteRoot, deletes)

	noopUpserts, noopDeletes := noopManifestUpdates(plan, localScan.Files, remoteScan.Files, scopedManifest)
	upserts = append(upserts, noopUpserts...)
	deletes = append(deletes, noopDeletes...)

	if err := commitManifestUpdates(ctx, p.Repo, upserts, deletes); err != nil {
		return plan, failures, err
	}
	// Detached context, as in RunRobustOnce: completed directory
	// creates/removes must be recorded even if ctx was cancelled.
	if err := dirRepo.ApplyUpdates(context.WithoutCancel(ctx), dirsToAdd, dirsToRemove); err != nil {
		return plan, failures, &robustHardError{stage: "apply dir manifest updates", err: err}
	}
	return plan, failures, runErr
}

// scopedSubtree is a batch path that is a directory on at least one side.
type scopedSubtree struct {
	rel           string
	local, remote bool // which side(s) it is a real directory on
}

// scopedUnknown is a batch path whose Lstat failed on side ("local" or
// "remote") for a reason other than not-existing.
type scopedUnknown struct {
	rel, side string
	err       error
}

// scopedLstat is os.Lstat, indirected so tests can simulate a permission
// error on one side deterministically (chmod can't, when tests run as
// root). Same pattern as watch's newTicker/newWatcher.
var scopedLstat = os.Lstat

// scopedActionsDone runs right after RunRobustScoped's file actions finish;
// a no-op in production. Tests use it to cancel ctx at exactly that point.
var scopedActionsDone = func() {}

// scopedSubtrees classifies a batch for RunRobustScoped: the directory
// paths to walk (outermost only - a path beneath another one in the batch
// is already covered by its walk), and the prefixes whose manifest rows
// must be loaded: every walked directory, plus every path now absent
// locally (it may have been a directory whose tracked contents need
// reconciling). Ignored paths are dropped.
//
// A path whose Lstat fails on either side for any reason other than
// not-existing is returned in unknown instead, and neither expanded nor
// prefix-loaded: its state on that side is not known, so nothing beneath
// it may be planned from this cycle (see RunRobustScoped).
func scopedSubtrees(localRoot, remoteRoot string, relPaths map[string]struct{}, matcher *fs.IgnoreMatcher) ([]scopedSubtree, []string, []scopedUnknown) {
	stat := func(root, rel string) (dir, missing bool, err error) {
		info, err := scopedLstat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			if os.IsNotExist(err) {
				return false, true, nil
			}
			return false, false, err
		}
		return info.IsDir(), false, nil // Lstat: a symlink is never a directory here
	}
	var subtrees []scopedSubtree
	var prefixes []string
	var unknown []scopedUnknown
	for rel := range relPaths {
		if matcher.MatchPathOrAncestor(rel, true) && matcher.MatchPathOrAncestor(rel, false) {
			continue // ignored whether file or directory: nothing to do
		}
		localDir, localMissing, localErr := stat(localRoot, rel)
		remoteDir, _, remoteErr := stat(remoteRoot, rel)
		if localErr != nil || remoteErr != nil {
			if localErr != nil {
				unknown = append(unknown, scopedUnknown{rel: rel, side: "local", err: localErr})
			}
			if remoteErr != nil {
				unknown = append(unknown, scopedUnknown{rel: rel, side: "remote", err: remoteErr})
			}
			continue
		}
		if (localDir || remoteDir) && !matcher.MatchPathOrAncestor(rel, true) {
			subtrees = append(subtrees, scopedSubtree{rel: rel, local: localDir, remote: remoteDir})
		} else if localMissing && !matcher.MatchPathOrAncestor(rel, true) {
			prefixes = append(prefixes, rel)
		}
	}
	// Keep only outermost subtrees.
	sort.Slice(subtrees, func(i, j int) bool { return subtrees[i].rel < subtrees[j].rel })
	outer := subtrees[:0]
	for _, st := range subtrees {
		if len(outer) > 0 && isWithin(st.rel, outer[len(outer)-1].rel) {
			continue
		}
		outer = append(outer, st)
	}
	for _, st := range outer {
		prefixes = append(prefixes, st.rel)
	}
	sort.Strings(prefixes)
	sort.Slice(unknown, func(i, j int) bool { return unknown[i].rel < unknown[j].rel })
	return outer, prefixes, unknown
}

// isWithin reports whether rel is dir itself or beneath it, by whole
// path segments.
func isWithin(rel, dir string) bool {
	return rel == dir || strings.HasPrefix(rel, dir+"/")
}

// baselineWithin restricts the directory baseline to the walked subtrees,
// so reconcileDirectories never mistakes a directory outside them (which
// this cycle didn't scan) for one deleted on both sides.
func baselineWithin(baseline map[string]struct{}, subtrees []scopedSubtree) map[string]struct{} {
	out := make(map[string]struct{})
	for d := range baseline {
		for _, st := range subtrees {
			if isWithin(d, st.rel) {
				out[d] = struct{}{}
				break
			}
		}
	}
	return out
}

// mergeScanResult folds src (a subtree scan of the same root) into dst.
func mergeScanResult(dst *fs.ScanResult, src fs.ScanResult) {
	for k, v := range src.Files {
		dst.Files[k] = v
	}
	for k := range src.Skipped {
		dst.Skipped[k] = struct{}{}
		delete(dst.Files, k)
	}
	for k := range src.SkippedDirs {
		dst.SkippedDirs[k] = struct{}{}
	}
	for k := range src.Dirs {
		dst.Dirs[k] = struct{}{}
	}
	dst.Issues = append(dst.Issues, src.Issues...)
}

// RunInit bootstraps first-time setup: creates the local and remote root
// directories if missing, and writes the remote canary file (.synclock) if
// it isn't already there. It performs no reconciliation.
func RunInit(localRoot, remoteRoot string, logger *runtime.Logger) error {
	// Same guard watch and sync apply: a remote nested in the local root
	// (or vice versa) would put the canary inside the tree being synced,
	// and every later `watch` against this pair refuses to start anyway.
	overlap, err := rootsOverlap(localRoot, remoteRoot)
	if err != nil {
		return fmt.Errorf("resolve init roots: %w", err)
	}
	if overlap {
		return fmt.Errorf("local and remote roots must not be the same directory or nested inside one another: %s, %s", localRoot, remoteRoot)
	}
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		return fmt.Errorf("create local root: %w", err)
	}
	if err := os.MkdirAll(remoteRoot, 0o755); err != nil {
		return fmt.Errorf("create remote root: %w", err)
	}
	created, err := runtime.EnsureRemoteCanary(remoteRoot)
	if err != nil {
		return err
	}
	if created {
		logger.Printf("created remote canary file in %s", remoteRoot)
	} else {
		logger.Printf("remote canary file already present in %s", remoteRoot)
	}
	logger.Printf("syncd initialized: local=%s remote=%s", localRoot, remoteRoot)
	return nil
}

// scanProgressLogger turns scan progress into log lines so a long-running
// scan (e.g. a large tree over a slow SMB mount) is visibly still working
// instead of looking identical to a hang until it finishes.
func scanProgressLogger(logger *runtime.Logger) fs.ScanPairProgressFunc {
	return func(side string, scanned int, done bool) {
		if done {
			logger.Printf("scan %s: done, %d file(s)", side, scanned)
			return
		}
		logger.Printf("scan %s: %d file(s) so far...", side, scanned)
	}
}

// mergeSkipSet builds the reconcile.SkipSet both reconcilers use to suppress
// action generation for paths this scan couldn't read, from one or more
// fs.ScanResults (typically a local+remote pair) - merging both their exact
// Skipped paths and their SkippedDirs, so a directory unreadable on either
// side covers its whole subtree via reconcile.SkipSet.Contains regardless of
// which side hit the problem.
func mergeSkipSet(scans ...fs.ScanResult) reconcile.SkipSet {
	exact := make(map[string]struct{})
	dirs := make(map[string]struct{})
	for _, s := range scans {
		for k := range s.Skipped {
			exact[k] = struct{}{}
		}
		for k := range s.SkippedDirs {
			dirs[k] = struct{}{}
		}
	}
	return reconcile.NewSkipSet(exact, dirs)
}

// logScanIssues records each scan issue from one side (side is "local" or
// "remote") at Info level - log file only, never stderr - and, if there were
// any, a single aggregated Warn line (which per Logger's contract always
// reaches stderr, regardless of --silent) so the condition stays visible to
// the operator without flooding stderr/the log file.
//
// This is the fix for the review's Important 4 finding: RunRobustOnce runs
// on every --remote-poll tick (30s by default), and the previous code logged
// one Logger.Error per issue - since Task 2 made Error unconditionally reach
// both stderr and the log file, a tree with 50 permanently-unreadable
// symlinks produced ~100 stderr lines/minute forever, growing the log by
// ~17MB/day. Per-path detail is still fully available (in the log file, at
// Info), just not re-blasted to stderr every cycle for a condition the
// operator often cannot immediately fix. This also fixes the asymmetry the
// same finding called out: the old code only ever logged Local.Issues, so a
// remote-side (over SMB) read/hash failure was reported to nobody; callers
// must invoke this for both sides.
func logScanIssues(logger *runtime.Logger, side string, issues []fs.ScanIssue) {
	for _, issue := range issues {
		logger.Info("skipping file", map[string]any{
			"side":   side,
			"path":   issue.Path,
			"reason": issue.Reason,
		})
	}
	if len(issues) > 0 {
		logger.Warn(fmt.Sprintf("skipped %d %s file(s) during scan; see log for per-path detail", len(issues), side), map[string]any{
			"side":  side,
			"count": len(issues),
		})
	}
}
