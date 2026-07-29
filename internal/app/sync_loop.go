package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"syncd/internal/db"
	"syncd/internal/execute"
	"syncd/internal/fs"
	"syncd/internal/reconcile"
	"syncd/internal/runtime"
)

type runSummary struct {
	Actions map[reconcile.ActionType]int `json:"actions"`
	Errors  int                          `json:"errors"`
}

// RobustParams configures a single robust, hash-based three-way sync cycle
// run by RunRobustOnce. It is the shared engine behind app.Run's always-on
// loop, the future watch loop, and the `syncd sync --hash` bridge (see
// sync_once.go).
type RobustParams struct {
	LocalRoot, RemoteRoot string
	Workers               int
	Repo                  *db.ManifestRepo
	DB                    *sql.DB
	Logger                *runtime.Logger
	Policy                fs.HashPolicy       // HashIfChanged for polls, HashAlways for deep verify
	OnlyPaths             map[string]struct{} // nil => whole tree; non-nil => restrict scan+plan to these rel paths
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
// action(s)" respectively. See app.Run and sync_once.go's runRobustOnce for
// the two call sites that branch on this.
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
//
// When p.OnlyPaths is non-nil, the local/remote/manifest file maps produced
// by the scan are filtered down to just those relative paths before
// reconcile.BuildPlan runs, so only that subset is reconciled - everything
// else is left untouched. This does not restrict empty-directory
// create/cleanup, which always considers the whole tree.
func RunRobustOnce(ctx context.Context, p RobustParams) (reconcile.Plan, int, error) {
	manifest, err := p.Repo.LoadAll(ctx)
	if err != nil {
		return reconcile.Plan{}, 0, &robustHardError{stage: "load manifest", err: err}
	}

	scans, err := fs.ScanPair(ctx, p.LocalRoot, p.RemoteRoot, manifest, p.Policy, scanProgressLogger(p.Logger))
	if err != nil {
		return reconcile.Plan{}, 0, &robustHardError{stage: "scan", err: err}
	}
	for _, issue := range scans.Local.Issues {
		p.Logger.Error("skipping local file", map[string]any{
			"path":   issue.Path,
			"reason": issue.Reason,
		})
	}

	localFiles, remoteFiles := scans.Local.Files, scans.Remote.Files
	if p.OnlyPaths != nil {
		localFiles = filterToOnlyPaths(localFiles, p.OnlyPaths)
		remoteFiles = filterToOnlyPaths(remoteFiles, p.OnlyPaths)
		manifest = filterToOnlyPaths(manifest, p.OnlyPaths)
	}

	skipped := copySkipped(scans.Local.Skipped, scans.Remote.Skipped)
	plan := reconcile.BuildPlan(localFiles, remoteFiles, manifest, skipped)

	execActions := make([]reconcile.Action, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		if action.Type != reconcile.ActionNoOp {
			execActions = append(execActions, action)
		}
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

	failures += createMissingEmptyDirs(p.Logger, p.LocalRoot, p.RemoteRoot, scans.Local.Dirs, scans.Remote.Dirs)
	cleanupEmptiedDirs(p.LocalRoot, p.RemoteRoot, deletes)

	if len(upserts) > 0 || len(deletes) > 0 {
		if err := p.Repo.ApplyUpdates(ctx, upserts, deletes); err != nil {
			return plan, failures, &robustHardError{stage: "apply manifest updates", err: err}
		}
	}

	return plan, failures, runErr
}

// filterToOnlyPaths returns a new map containing only the entries of m whose
// key is present in only.
func filterToOnlyPaths[T any](m map[string]T, only map[string]struct{}) map[string]T {
	out := make(map[string]T, len(only))
	for k, v := range m {
		if _, ok := only[k]; ok {
			out[k] = v
		}
	}
	return out
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
	manifest, err := p.Repo.LoadAll(ctx)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	localScan, err := fs.ScanPaths(p.LocalRoot, relPaths, manifest, p.Policy, true, matcher)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	remoteScan, err := fs.ScanPaths(p.RemoteRoot, relPaths, manifest, p.Policy, false, matcher)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	for _, issue := range localScan.Issues {
		p.Logger.Error("skipping local file", map[string]any{"path": issue.Path, "reason": issue.Reason})
	}
	scopedManifest := make(map[string]db.ManifestRow, len(relPaths))
	for rel := range relPaths {
		if row, ok := manifest[rel]; ok {
			scopedManifest[rel] = row
		}
	}
	skipped := copySkipped(localScan.Skipped, remoteScan.Skipped)
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
	if len(upserts) > 0 || len(deletes) > 0 {
		if err := p.Repo.ApplyUpdates(ctx, upserts, deletes); err != nil {
			return plan, failures, err
		}
	}
	return plan, failures, runErr
}

func Run(ctx context.Context, cfg runtime.Config) error {
	paths, err := runtime.ResolvePaths(cfg)
	if err != nil {
		return err
	}
	logger, err := runtime.NewLogger(cfg.Silent, paths.LogPath)
	if err != nil {
		return err
	}
	defer logger.Close()

	if cfg.Init {
		return RunInit(cfg.LocalRoot, cfg.RemoteRoot, logger)
	}

	lock, err := runtime.AcquireLock(paths.LockPath)
	if err != nil {
		return err
	}
	defer lock.Release()

	if err := runtime.CheckRemoteCanary(cfg.RemoteRoot); err != nil {
		return err
	}

	sqlDB, err := db.Open(paths.DBPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	repo := db.NewManifestRepo(sqlDB)

	if cfg.DryRun {
		manifest, err := repo.LoadAll(ctx)
		if err != nil {
			return err
		}
		scans, err := fs.ScanPair(ctx, cfg.LocalRoot, cfg.RemoteRoot, manifest, fs.HashIfChanged, scanProgressLogger(logger))
		if err != nil {
			return err
		}
		for _, issue := range scans.Local.Issues {
			logger.Error("skipping local file", map[string]any{
				"path":   issue.Path,
				"reason": issue.Reason,
			})
		}
		skipped := copySkipped(scans.Local.Skipped, scans.Remote.Skipped)
		plan := reconcile.BuildPlan(scans.Local.Files, scans.Remote.Files, manifest, skipped)
		return printPlan(logger, cfg.Silent, plan)
	}

	plan, failures, runErr := RunRobustOnce(ctx, RobustParams{
		LocalRoot:  cfg.LocalRoot,
		RemoteRoot: cfg.RemoteRoot,
		Workers:    cfg.Workers,
		Repo:       repo,
		DB:         sqlDB,
		Logger:     logger,
		Policy:     fs.HashIfChanged,
	})

	// A hard failure (manifest LoadAll, ScanPair, or ApplyUpdates) is
	// reported immediately with its own bare error text, exactly as before
	// RunRobustOnce existed - no run summary is printed for it.
	if hard, ok := runErr.(*robustHardError); ok {
		return hard.err
	}

	if err := printRunSummary(logger, cfg.Silent, plan.Actions, failures); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("sync run did not complete: %w", runErr)
	}
	if failures > 0 {
		return fmt.Errorf("sync completed with %d failed action(s)", failures)
	}
	return nil
}

// RunInit bootstraps first-time setup: creates the local and remote root
// directories if missing, and writes the remote canary file (.synclock) if
// it isn't already there. It performs no reconciliation.
func RunInit(localRoot, remoteRoot string, logger *runtime.Logger) error {
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

func copySkipped(sets ...map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{})
	for _, set := range sets {
		for k := range set {
			out[k] = struct{}{}
		}
	}
	return out
}

func printPlan(logger *runtime.Logger, silent bool, plan reconcile.Plan) error {
	if silent {
		b, err := json.Marshal(plan)
		if err != nil {
			return fmt.Errorf("marshal dry-run plan: %w", err)
		}
		logger.Info("dry-run plan", map[string]any{"plan": json.RawMessage(b)})
		return nil
	}

	for _, action := range plan.Actions {
		logger.Printf("%s\t%s\t%s", action.Type, action.Path, action.Reason)
	}
	return nil
}

func printRunSummary(logger *runtime.Logger, silent bool, actions []reconcile.Action, failures int) error {
	counts := make(map[reconcile.ActionType]int)
	for _, action := range actions {
		counts[action.Type]++
	}
	sum := runSummary{
		Actions: counts,
		Errors:  failures,
	}
	if silent {
		b, err := json.Marshal(sum)
		if err != nil {
			return fmt.Errorf("marshal run summary: %w", err)
		}
		logger.Info("sync run summary", map[string]any{"summary": json.RawMessage(b)})
		return nil
	}
	logger.Printf("sync completed with %d error(s)", failures)
	for actionType, count := range counts {
		logger.Printf("%s: %d", actionType, count)
	}
	return nil
}
