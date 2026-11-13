package app

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
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
	// about to execute and may decline them (return false), in which case
	// RunRobustOnce returns ErrAborted having changed nothing - no file
	// action, no directory create/remove, no manifest write. It is how
	// `syncd sync --hash`'s interactive confirm approves exactly the plan
	// that then runs, rather than a separate preview scan's plan. nil
	// (watch, --yes) means "always proceed".
	ConfirmPlan func(actions []reconcile.Action) bool
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

	if p.ConfirmPlan != nil && !p.ConfirmPlan(execActions) {
		return plan, 0, ErrAborted
	}

	state := execute.SyncState{
		LocalRoot:   p.LocalRoot,
		RemoteRoot:  p.RemoteRoot,
		LocalFiles:  localFiles,
		RemoteFiles: remoteFiles,
	}
	// runErr (e.g. context cancellation) is deliberately not returned yet:
	// results may be partial, but whatever did complete should still be
	// committed to the manifest below before we report failure.
	results, runErr := execute.RunActions(ctx, p.Workers, state, execActions)

	upserts, deletes, failures := collectManifestUpdates(results, p.Logger)

	dirsToAdd, dirsToRemove, dirFailures := reconcileDirectories(p.Logger, p.LocalRoot, p.RemoteRoot, scans.Local.Dirs, scans.Remote.Dirs, dirBaseline, skipped)
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

// RunRobustScoped reconciles ONLY relPaths, scanning just those paths on
// both sides via fs.ScanPaths (no tree walk). It mirrors RunRobustOnce's
// scan -> plan -> execute -> manifest-commit flow but omits whole-tree
// empty-dir CREATION (new empty dirs, and dirs emptied by a recursive
// delete outside this batch, are picked up by the periodic deep scan
// instead). Emptied-dir cleanup driven by this batch's own deletes still
// runs, same as RunRobustOnce.
func RunRobustScoped(ctx context.Context, p RobustParams, relPaths map[string]struct{}) (reconcile.Plan, int, error) {
	matcher, err := fs.LoadIgnoreMatcher(p.LocalRoot)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	// Only this batch's rows: loading the whole manifest per fsnotify batch
	// made the scoped (hot) path's cost scale with the tree, not the batch.
	scopedManifest, err := p.Repo.LoadPaths(ctx, relPaths)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	localScan, err := fs.ScanPaths(p.LocalRoot, relPaths, scopedManifest, p.Policy, true, matcher)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	remoteScan, err := fs.ScanPaths(p.RemoteRoot, relPaths, db.RemoteStatView(scopedManifest), p.Policy, false, matcher)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	logScanIssues(p.Logger, "local", localScan.Issues)
	logScanIssues(p.Logger, "remote", remoteScan.Issues)
	skipped := mergeSkipSet(localScan, remoteScan)
	plan := reconcile.BuildPlan(localScan.Files, remoteScan.Files, scopedManifest, skipped)
	execActions := make([]reconcile.Action, 0, len(plan.Actions))
	for _, a := range plan.Actions {
		if a.Type != reconcile.ActionNoOp {
			execActions = append(execActions, a)
		}
	}
	state := execute.SyncState{LocalRoot: p.LocalRoot, RemoteRoot: p.RemoteRoot, LocalFiles: localScan.Files, RemoteFiles: remoteScan.Files}
	results, runErr := execute.RunActions(ctx, p.Workers, state, execActions)
	upserts, deletes, failures := collectManifestUpdates(results, p.Logger)
	cleanupEmptiedDirs(p.LocalRoot, p.RemoteRoot, deletes)

	noopUpserts, noopDeletes := noopManifestUpdates(plan, localScan.Files, remoteScan.Files, scopedManifest)
	upserts = append(upserts, noopUpserts...)
	deletes = append(deletes, noopDeletes...)

	if err := commitManifestUpdates(ctx, p.Repo, upserts, deletes); err != nil {
		return plan, failures, err
	}
	return plan, failures, runErr
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
