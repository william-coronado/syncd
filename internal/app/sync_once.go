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
	DryRun            bool // --dry-run: print the summary and exit without mutating anything or persisting job state. Still acquires the per-job lock (cli.runSyncJob) and opens/creates the state db (SyncOnce below) - deliberate, since this is a read of shared job state, not a mutation; see docs/follow-ups.md #3.
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
// first and so can assume both roots are already there) - except for
// --dry-run, which must not create anything on disk at all (see
// validateRoots/createRoots below for why the two are split).
func prepareRoots(opts SyncOptions) error {
	if err := validateRoots(opts); err != nil {
		return err
	}
	if opts.DryRun {
		return nil
	}
	return createRoots(opts)
}

// validateRoots is prepareRoots' data-loss guard, and runs unconditionally
// - even for --dry-run, which must report the exact same error as a real
// run against a genuinely missing source, not silently treat it as empty.
//
// It is deliberately mode-aware: mirror/update (fast) and the --hash/robust
// path are directional, with opts.Left as the source of truth, so
// fabricating an empty source there would be a footgun - FastCompare's
// mirror branch (or BuildPlan) would see "everything on the destination is
// gone from the source" and emit deletes for the destination's entire
// contents, silently wiping it under --yes. For those modes the source
// (opts.Left) must already exist. two-way (fast) is symmetric and safe - a
// missing side with no prior persisted state produces copies, not deletes -
// so it has no source-must-exist requirement at all.
func validateRoots(opts SyncOptions) error {
	if !opts.UseHash && opts.Mode == reconcile.FastTwoWay {
		return nil
	}
	if _, err := os.Stat(opts.Left); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("source directory does not exist: %s", opts.Left)
		}
		return fmt.Errorf("stat source directory %s: %w", opts.Left, err)
	}
	return nil
}

// createRoots creates whichever roots don't exist yet, and is skipped
// entirely for --dry-run (which must not create anything on disk - not
// even an empty destination directory, since that's still a real mutation
// on whatever filesystem opts.Right lives on, including a mounted SMB
// share in production).
//
// mirror/update/--hash only create the destination (opts.Right) - the
// source (opts.Left) was already validated to exist by validateRoots and
// must never be auto-created, per the data-loss guard. two-way creates
// both roots (see validateRoots' doc comment for why that's safe there).
func createRoots(opts SyncOptions) error {
	if !opts.UseHash && opts.Mode == reconcile.FastTwoWay {
		if err := os.MkdirAll(opts.Left, 0o755); err != nil {
			return fmt.Errorf("create left root: %w", err)
		}
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
	scans, err := scanPairForOpts(ctx, opts, nil, fs.HashNever)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	// Surface scan issues the same way the robust path does (RunRobustOnce):
	// per-path detail at Info (log file only), one aggregated Warn summary
	// per run - see logScanIssues' doc comment. The fast path has no
	// standing logger of its own (SyncOptions carries no log path), so build
	// a throwaway one scoped to this call, same pattern runRobustOnce below
	// already uses for its own --hash bridge.
	issueLogger, err := runtime.NewLogger(false, "")
	if err != nil {
		return fmt.Errorf("create logger: %w", err)
	}
	defer issueLogger.Close()
	logScanIssues(issueLogger, "local", scans.Local.Issues)
	logScanIssues(issueLogger, "remote", scans.Remote.Issues)

	left := toJobFileStates(scans.Local.Files)
	right := toJobFileStates(scans.Remote.Files)
	skipped := mergeSkipSet(scans.Local, scans.Remote)

	repo := db.NewJobStateRepo(sqlDB)
	var prev map[string]db.JobFileState
	if opts.Mode == reconcile.FastTwoWay {
		prev, err = repo.Load(ctx, opts.Name)
		if err != nil {
			return fmt.Errorf("load job state: %w", err)
		}
	}

	res := reconcile.FastCompare(opts.Mode, left, right, prev, skipped)

	// --dry-run prints the same summary the interactive confirm would show
	// and exits before anything is mutated - it must not require --yes (no
	// Confirm callback is ever invoked), and it must not execute the tasks
	// or persist two-way's job state, or the next real run would compute
	// its diff against a baseline that never actually happened.
	if opts.DryRun {
		fmt.Println(fastSummary(opts.Name, opts.Mode, res.Tasks))
		return nil
	}

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

// scanPairForOpts scans opts.Left/opts.Right, exactly via fs.ScanPair,
// UNLESS opts.DryRun is set - --dry-run skips createRoots, so a root that
// doesn't exist yet is a real possibility here (never true for a
// non-dry-run call, since createRoots already ran by the time this is
// called), and fs.ScanPair/ScanTree deliberately abort the whole scan if
// the ROOT itself is missing (load-bearing: that's how a vanished SMB mount
// is distinguished from an empty tree, and internal/fs is owned by another
// task - that behavior must not be weakened). So for --dry-run only, this
// treats a missing root as an empty tree for the PREVIEW rather than
// creating it or failing the scan.
func scanPairForOpts(ctx context.Context, opts SyncOptions, manifest map[string]db.ManifestRow, policy fs.HashPolicy) (fs.ScanPairResult, error) {
	if !opts.DryRun {
		return fs.ScanPair(ctx, opts.Left, opts.Right, manifest, policy, nil)
	}

	// Mirrors fs.ScanPair's own local-root-relative ignore matcher and
	// validateSMBName (true for local/left, false for remote/right), just
	// scanning each side in turn instead of concurrently - dry-run preview
	// is not the hot path.
	matcher, err := fs.LoadIgnoreMatcher(opts.Left)
	if err != nil {
		return fs.ScanPairResult{}, err
	}
	local, err := scanRootOrEmpty(ctx, opts.Left, manifest, true, matcher, policy)
	if err != nil {
		return fs.ScanPairResult{}, err
	}
	remote, err := scanRootOrEmpty(ctx, opts.Right, manifest, false, matcher, policy)
	if err != nil {
		return fs.ScanPairResult{}, err
	}
	return fs.ScanPairResult{Local: local, Remote: remote}, nil
}

// scanRootOrEmpty is fs.ScanTree, except a root that doesn't exist yet
// yields an empty ScanResult instead of an error. Used only by
// scanPairForOpts's --dry-run path - see its doc comment for why.
func scanRootOrEmpty(ctx context.Context, root string, manifest map[string]db.ManifestRow, validateSMBName bool, matcher *fs.IgnoreMatcher, policy fs.HashPolicy) (fs.ScanResult, error) {
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return fs.ScanResult{
				Files:       make(map[string]fs.FileEntry),
				Skipped:     make(map[string]struct{}),
				SkippedDirs: make(map[string]struct{}),
				Dirs:        make(map[string]struct{}),
			}, nil
		}
		return fs.ScanResult{}, fmt.Errorf("stat %s: %w", root, err)
	}
	return fs.ScanTree(ctx, root, manifest, validateSMBName, matcher, policy, nil)
}

func toJobFileStates(files map[string]fs.FileEntry) map[string]db.JobFileState {
	out := make(map[string]db.JobFileState, len(files))
	for path, f := range files {
		out[path] = db.JobFileState{Path: path, Size: f.Size, Mtime: f.Mtime}
	}
	return out
}

// summaryPathCap is the maximum number of paths listed per task/action type
// in fastSummary/robustSummary before the rest are collapsed into a single
// "... and N more" line - so a huge job's confirm prompt doesn't flood the
// terminal.
const summaryPathCap = 20

// fastSummary builds the human-readable text shown to opts.Confirm (or
// printed directly for --dry-run) before a fast-mode run mutates anything.
// It lists the affected paths grouped by task type - not just counts - since
// for a destructive mirror run this prompt is the only safety gate, and the
// user needs to see WHICH files, not just how many. Destructive types
// (DeleteL/DeleteR) are listed first, before the copies/moves.
func fastSummary(name string, mode reconcile.FastMode, tasks []reconcile.FastTask) string {
	if len(tasks) == 0 {
		return fmt.Sprintf("sync %q (%s): no changes", name, mode)
	}
	byType := make(map[reconcile.FastTaskType][]string)
	for _, t := range tasks {
		byType[t.Type] = append(byType[t.Type], t.RelPath)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sync %q (%s): %d task(s)", name, mode, len(tasks))
	for _, tt := range []reconcile.FastTaskType{
		reconcile.DeleteL, reconcile.DeleteR, // destructive first
		reconcile.CopyL2R, reconcile.CopyR2L,
		reconcile.MoveL, reconcile.MoveR,
	} {
		appendSummaryGroup(&b, string(tt), byType[tt])
	}
	return b.String()
}

// appendSummaryGroup writes one task/action-type section to b: a "Label: N"
// header, then up to summaryPathCap of paths (sorted for deterministic
// output), then a "... and N more" tail if there were more than that.
// Writes nothing if paths is empty.
func appendSummaryGroup(b *strings.Builder, label string, paths []string) {
	if len(paths) == 0 {
		return
	}
	sort.Strings(paths)
	fmt.Fprintf(b, "\n  %s: %d", label, len(paths))
	shown := paths
	var more int
	if len(paths) > summaryPathCap {
		shown = paths[:summaryPathCap]
		more = len(paths) - summaryPathCap
	}
	for _, p := range shown {
		fmt.Fprintf(b, "\n    %s", p)
	}
	if more > 0 {
		fmt.Fprintf(b, "\n    ... and %d more", more)
	}
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
// stdout, and Warn/Error always reach stderr regardless of --silent (Task 2)
// and regardless of whether a log file is open at all; only Info-level
// detail has nowhere to go when there's no log file, since Info is
// deliberately never printed to stdout/stderr (see logScanIssues, which
// relies on exactly that split: per-path detail at Info, one aggregated
// summary at Warn).
func runRobustOnce(ctx context.Context, sqlDB *sql.DB, opts SyncOptions) error {
	repo := db.NewManifestRepo(sqlDB)

	// A preview scan+plan is needed both to show the interactive confirm
	// (!opts.Assume) and for --dry-run, which must print the same summary
	// and exit before RunRobustOnce ever runs (no execution, no manifest
	// persistence) - regardless of opts.Assume, since --dry-run must not
	// require --yes either.
	if opts.DryRun || !opts.Assume {
		manifest, err := repo.LoadAll(ctx)
		if err != nil {
			return fmt.Errorf("load manifest: %w", err)
		}
		scans, err := scanPairForOpts(ctx, opts, manifest, fs.HashAlways)
		if err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		skipped := mergeSkipSet(scans.Local, scans.Remote)
		plan := reconcile.BuildPlan(scans.Local.Files, scans.Remote.Files, manifest, skipped)
		execActions := make([]reconcile.Action, 0, len(plan.Actions))
		for _, action := range plan.Actions {
			if action.Type != reconcile.ActionNoOp {
				execActions = append(execActions, action)
			}
		}

		summary := robustSummary(opts.Name, execActions)
		if opts.DryRun {
			fmt.Println(summary)
			return nil
		}
		if !confirmOrDefault(opts.Confirm)(summary) {
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
	// A hard failure (manifest LoadAll, ScanPair, or ApplyUpdates) is
	// reported with this call site's own original per-stage wrapping,
	// matching this function's pre-RunRobustOnce error text exactly.
	if hard, ok := runErr.(*robustHardError); ok {
		return fmt.Errorf("%s: %w", hard.stage, hard.err)
	}
	if runErr != nil {
		return fmt.Errorf("sync run did not complete: %w", runErr)
	}
	if failures > 0 {
		return fmt.Errorf("sync completed with %d failed action(s)", failures)
	}
	return nil
}

// robustSummary is the --hash-path counterpart to fastSummary: see its doc
// comment for the path-listing/cap/destructive-first rationale.
func robustSummary(name string, actions []reconcile.Action) string {
	if len(actions) == 0 {
		return fmt.Sprintf("sync %q (hash): no changes", name)
	}
	byType := make(map[reconcile.ActionType][]string)
	for _, a := range actions {
		byType[a.Type] = append(byType[a.Type], a.Path)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sync %q (hash): %d action(s)", name, len(actions))
	for _, at := range []reconcile.ActionType{
		reconcile.ActionDeleteLocal, reconcile.ActionDeleteRemote, // destructive first
		reconcile.ActionUpload, reconcile.ActionDownload,
		reconcile.ActionConflict,
	} {
		appendSummaryGroup(&b, string(at), byType[at])
	}
	return b.String()
}
