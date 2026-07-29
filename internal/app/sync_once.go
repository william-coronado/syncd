package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"syncd/internal/db"
	"syncd/internal/execute"
	"syncd/internal/fs"
	"syncd/internal/reconcile"
)

// ErrAborted is returned by SyncOnce when the caller's Confirm callback
// declines the run. It is not a failure - the caller (the `syncd sync` CLI
// command) treats it as a clean, user-initiated cancellation rather than an
// error to report as such.
var ErrAborted = errors.New("sync: aborted by user")

// SyncOptions configures a single ad-hoc/fast (or --hash/robust) sync job
// run by SyncOnce.
type SyncOptions struct {
	Name, Left, Right string
	Mode              reconcile.FastMode
	UseHash           bool
	Assume            bool // --yes: skip interactive confirm
	Workers           int
	DBPath            string
	Confirm           func(summary string) bool // nil => auto-yes; cli passes a pterm confirm
	Progress          execute.ProgressSink      // nil => no progress bar
}

// SyncOnce runs a single fast job (mirror/update/two-way). If opts.UseHash
// is true it instead runs the robust three-way (hash-based) engine for this
// one job, reusing the same scan/plan/execute/manifest-update shape as
// app.Run's always-on loop.
func SyncOnce(ctx context.Context, opts SyncOptions) error {
	if opts.Workers < 1 {
		opts.Workers = 1
	}

	// Ad-hoc jobs are commonly pointed at a destination that doesn't exist
	// yet (that's the point of `mirror`), unlike the always-on daemon which
	// requires --init first. fs.ScanTree walks with filepath.WalkDir, which
	// errors if root is missing, so create both roots up front.
	if err := os.MkdirAll(opts.Left, 0o755); err != nil {
		return fmt.Errorf("create left root: %w", err)
	}
	if err := os.MkdirAll(opts.Right, 0o755); err != nil {
		return fmt.Errorf("create right root: %w", err)
	}

	sqlDB, err := db.Open(opts.DBPath)
	if err != nil {
		return fmt.Errorf("open state db: %w", err)
	}
	defer sqlDB.Close()

	if opts.UseHash {
		return runRobustOnce(ctx, sqlDB, opts)
	}
	return runFastOnce(ctx, sqlDB, opts)
}

// confirmOrDefault returns opts.Confirm, defaulting to an always-yes
// function when nil (per the SyncOptions.Confirm doc: nil => auto-yes).
func confirmOrDefault(confirm func(string) bool) func(string) bool {
	if confirm != nil {
		return confirm
	}
	return func(string) bool { return true }
}

// runFastOnce executes the size+mtime fast reconciler (mirror/update/two-way).
func runFastOnce(ctx context.Context, sqlDB *sql.DB, opts SyncOptions) error {
	scans, err := fs.ScanPair(ctx, opts.Left, opts.Right, nil, fs.HashNever, nil)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	left := toJobFileStates(scans.Local.Files)
	right := toJobFileStates(scans.Remote.Files)

	repo := db.NewJobStateRepo(sqlDB)
	var prev map[string]db.JobFileState
	if opts.Mode == reconcile.FastTwoWay {
		prev, err = repo.Load(ctx, opts.Name)
		if err != nil {
			return fmt.Errorf("load job state: %w", err)
		}
	}

	res := reconcile.FastCompare(opts.Mode, left, right, prev)

	if !opts.Assume {
		if !confirmOrDefault(opts.Confirm)(fastSummary(opts.Name, opts.Mode, res.Tasks)) {
			return ErrAborted
		}
	}

	if err := execute.RunFastTasks(ctx, opts.Workers, opts.Left, opts.Right, res.Tasks, opts.Progress); err != nil {
		return fmt.Errorf("run fast tasks: %w", err)
	}

	if opts.Mode == reconcile.FastTwoWay {
		if err := repo.Replace(ctx, opts.Name, res.NewState); err != nil {
			return fmt.Errorf("persist job state: %w", err)
		}
	}
	return nil
}

func toJobFileStates(files map[string]fs.FileEntry) map[string]db.JobFileState {
	out := make(map[string]db.JobFileState, len(files))
	for path, f := range files {
		out[path] = db.JobFileState{Path: path, Size: f.Size, Mtime: f.Mtime}
	}
	return out
}

// fastSummary builds the human-readable text shown to opts.Confirm before a
// fast-mode run mutates anything.
func fastSummary(name string, mode reconcile.FastMode, tasks []reconcile.FastTask) string {
	if len(tasks) == 0 {
		return fmt.Sprintf("sync %q (%s): no changes", name, mode)
	}
	counts := make(map[reconcile.FastTaskType]int)
	for _, t := range tasks {
		counts[t.Type]++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sync %q (%s): %d task(s)", name, mode, len(tasks))
	for _, tt := range []reconcile.FastTaskType{
		reconcile.CopyL2R, reconcile.CopyR2L,
		reconcile.DeleteL, reconcile.DeleteR,
		reconcile.MoveL, reconcile.MoveR,
	} {
		if n := counts[tt]; n > 0 {
			fmt.Fprintf(&b, "\n  %s: %d", tt, n)
		}
	}
	return b.String()
}

// runRobustOnce runs the robust, hash-based three-way engine for a single
// ad-hoc job (the --hash bridge). It deliberately mirrors the
// scan -> plan -> execute -> manifest-update shape of app.Run's always-on
// loop (see sync_loop.go) rather than duplicating its logic from scratch:
// it reuses the same reconcile.BuildPlan/execute.RunActions/db.ManifestRepo
// pieces and the package-private copySkipped helper from sync_loop.go.
//
// Known simplification (flagged per the task-7 brief, which explicitly
// allows a minimal --hash path for now): unlike app.Run, this does not
// create/clean up empty directories (createMissingEmptyDirs/
// cleanupEmptiedDirs in directories.go) since those helpers take a
// *runtime.Logger that SyncOnce has no equivalent of, and does not emit
// scan-progress logging. A later task (Task 9, per the brief) is expected
// to extract a proper shared RunRobustOnce helper used by both app.Run and
// SyncOnce; this is a deliberately minimal bridge until then.
func runRobustOnce(ctx context.Context, sqlDB *sql.DB, opts SyncOptions) error {
	repo := db.NewManifestRepo(sqlDB)
	manifest, err := repo.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}

	scans, err := fs.ScanPair(ctx, opts.Left, opts.Right, manifest, fs.HashAlways, nil)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	skipped := copySkipped(scans.Local.Skipped, scans.Remote.Skipped)
	plan := reconcile.BuildPlan(scans.Local.Files, scans.Remote.Files, manifest, skipped)

	execActions := make([]reconcile.Action, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		if action.Type != reconcile.ActionNoOp {
			execActions = append(execActions, action)
		}
	}

	if !opts.Assume {
		if !confirmOrDefault(opts.Confirm)(robustSummary(opts.Name, execActions)) {
			return ErrAborted
		}
	}

	state := execute.SyncState{
		LocalRoot:   opts.Left,
		RemoteRoot:  opts.Right,
		LocalFiles:  scans.Local.Files,
		RemoteFiles: scans.Remote.Files,
	}
	results, runErr := execute.RunActions(ctx, opts.Workers, state, execActions)

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

	if len(upserts) > 0 || len(deletes) > 0 {
		if err := repo.ApplyUpdates(ctx, upserts, deletes); err != nil {
			return fmt.Errorf("apply manifest updates: %w", err)
		}
	}

	if runErr != nil {
		return fmt.Errorf("sync run did not complete: %w", runErr)
	}
	if failures > 0 {
		return fmt.Errorf("sync completed with %d failed action(s)", failures)
	}
	return nil
}

func robustSummary(name string, actions []reconcile.Action) string {
	if len(actions) == 0 {
		return fmt.Sprintf("sync %q (hash): no changes", name)
	}
	counts := make(map[reconcile.ActionType]int)
	for _, a := range actions {
		counts[a.Type]++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sync %q (hash): %d action(s)", name, len(actions))
	for _, at := range []reconcile.ActionType{
		reconcile.ActionUpload, reconcile.ActionDownload,
		reconcile.ActionDeleteLocal, reconcile.ActionDeleteRemote,
		reconcile.ActionConflict,
	} {
		if n := counts[at]; n > 0 {
			fmt.Fprintf(&b, "\n  %s: %d", at, n)
		}
	}
	return b.String()
}
