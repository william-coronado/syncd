package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"syncd/internal/db"
	"syncd/internal/execute"
	"syncd/internal/fs"
	"syncd/internal/reconcile"
	"syncd/internal/runtime"
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

	if err := prepareRoots(opts); err != nil {
		return err
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

// prepareRoots ensures both sync roots exist before scanning (ad-hoc jobs
// are commonly pointed at a destination that doesn't exist yet - that's the
// point of `mirror` - unlike the always-on daemon, which requires --init
// first and so can assume both roots are already there).
//
// It is deliberately mode-aware: mirror/update (fast) and the --hash/robust
// path are directional, with opts.Left as the source of truth, so
// fabricating an empty source there would be a footgun - FastCompare's
// mirror branch (or BuildPlan) would see "everything on the destination is
// gone from the source" and emit deletes for the destination's entire
// contents, silently wiping it under --yes. For those modes the source
// (opts.Left) must already exist; only the destination (opts.Right) is
// auto-created. two-way (fast) is symmetric and safe - a missing side with
// no prior persisted state produces copies, not deletes - so creating both
// roots there is fine.
func prepareRoots(opts SyncOptions) error {
	if !opts.UseHash && opts.Mode == reconcile.FastTwoWay {
		if err := os.MkdirAll(opts.Left, 0o755); err != nil {
			return fmt.Errorf("create left root: %w", err)
		}
		if err := os.MkdirAll(opts.Right, 0o755); err != nil {
			return fmt.Errorf("create right root: %w", err)
		}
		return nil
	}

	if _, err := os.Stat(opts.Left); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("source directory does not exist: %s", opts.Left)
		}
		return fmt.Errorf("stat source directory %s: %w", opts.Left, err)
	}
	if err := os.MkdirAll(opts.Right, 0o755); err != nil {
		return fmt.Errorf("create right root: %w", err)
	}
	return nil
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
// ad-hoc job (the --hash bridge) by delegating the scan -> plan -> execute
// -> manifest-update (+ empty-dir) cycle to the shared app.RunRobustOnce
// (see run.go / sync_loop.go), so the --hash path benefits from the same
// empty-dir create/cleanup and scan-progress logging as app.Run's always-on
// loop.
//
// RunRobustOnce has no pause point between planning and executing, but
// SyncOnce supports an interactive confirm-before-mutate step (ptermConfirm
// from the CLI). So when a confirmation is actually needed (!opts.Assume),
// this builds a preview plan itself first - a second, hash-based scan -
// purely to produce the summary text and get sign-off, then calls
// RunRobustOnce for the (re-scanned) real run. The common automated case
// (--yes) skips the preview and goes straight to RunRobustOnce, paying for
// only one scan.
//
// RunRobustOnce takes a *runtime.Logger, which SyncOptions has no
// equivalent of; a non-silent logger (no log file) is constructed here,
// which is fine for this ad-hoc case - non-silent Loggers route Printf to
// stdout and drop structured Info/Error events, matching how app.Run itself
// behaves outside --silent.
func runRobustOnce(ctx context.Context, sqlDB *sql.DB, opts SyncOptions) error {
	repo := db.NewManifestRepo(sqlDB)

	if !opts.Assume {
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
		if !confirmOrDefault(opts.Confirm)(robustSummary(opts.Name, execActions)) {
			return ErrAborted
		}
	}

	logger, err := runtime.NewLogger(false, "")
	if err != nil {
		return fmt.Errorf("create logger: %w", err)
	}
	defer logger.Close()

	_, failures, runErr := RunRobustOnce(ctx, RobustParams{
		LocalRoot:  opts.Left,
		RemoteRoot: opts.Right,
		Workers:    opts.Workers,
		Repo:       repo,
		DB:         sqlDB,
		Logger:     logger,
		Policy:     fs.HashAlways,
	})
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
