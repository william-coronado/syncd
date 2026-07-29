package app

import (
	"context"
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
		return runInit(cfg, logger)
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
	if cfg.DryRun {
		return printPlan(logger, cfg.Silent, plan)
	}

	execActions := make([]reconcile.Action, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		if action.Type != reconcile.ActionNoOp {
			execActions = append(execActions, action)
		}
	}

	state := execute.SyncState{
		LocalRoot:   cfg.LocalRoot,
		RemoteRoot:  cfg.RemoteRoot,
		LocalFiles:  scans.Local.Files,
		RemoteFiles: scans.Remote.Files,
	}
	// runErr (e.g. context cancellation) is deliberately not returned yet:
	// results may be partial, but whatever did complete should still be
	// committed to the manifest below before we report failure.
	results, runErr := execute.RunActions(ctx, cfg.Workers, state, execActions)

	var (
		upserts  []db.ManifestRow
		deletes  []string
		failures int
	)
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

	failures += createMissingEmptyDirs(logger, cfg.LocalRoot, cfg.RemoteRoot, scans.Local.Dirs, scans.Remote.Dirs)
	cleanupEmptiedDirs(cfg.LocalRoot, cfg.RemoteRoot, deletes)

	if len(upserts) > 0 || len(deletes) > 0 {
		if err := repo.ApplyUpdates(ctx, upserts, deletes); err != nil {
			return err
		}
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

// runInit bootstraps first-time setup: creates the local and remote root
// directories if missing, and writes the remote canary file (.synclock) if
// it isn't already there. It performs no reconciliation.
func runInit(cfg runtime.Config, logger *runtime.Logger) error {
	if err := os.MkdirAll(cfg.LocalRoot, 0o755); err != nil {
		return fmt.Errorf("create local root: %w", err)
	}
	if err := os.MkdirAll(cfg.RemoteRoot, 0o755); err != nil {
		return fmt.Errorf("create remote root: %w", err)
	}
	created, err := runtime.EnsureRemoteCanary(cfg.RemoteRoot)
	if err != nil {
		return err
	}
	if created {
		logger.Printf("created remote canary file in %s", cfg.RemoteRoot)
	} else {
		logger.Printf("remote canary file already present in %s", cfg.RemoteRoot)
	}
	logger.Printf("drivesync initialized: local=%s remote=%s", cfg.LocalRoot, cfg.RemoteRoot)
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
