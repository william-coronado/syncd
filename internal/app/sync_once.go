package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// StateKey identifies this job's persisted state (job_state rows for
	// fast two-way, via stateKey below) independently of its display Name -
	// cli.runSyncJob derives it from Name plus a hash of the canonicalized
	// root pair, so two jobs sharing a Name (or one job repointed at a new
	// pair) never reuse each other's state. Falls back to Name when unset,
	// so callers that don't need the distinction (tests, direct API use)
	// can leave it empty.
	StateKey string
	Mode     reconcile.FastMode
	UseHash  bool
	Assume   bool // --yes: skip interactive confirm
	DryRun   bool // --dry-run: print the summary and exit without mutating anything or persisting job state. Still acquires the per-job lock (cli.runSyncJob) and opens/creates the state db (SyncOnce below) - deliberate, since this is a read of shared job state, not a mutation; see docs/follow-ups.md #3.
	Workers  int
	DBPath   string
	Confirm  func(summary string) bool // nil => auto-yes; cli passes a pterm confirm
	Progress execute.ProgressSink      // nil => no progress bar
}

// SyncOnce runs a single fast job (mirror/update/two-way). If opts.UseHash
// is true it instead runs the robust three-way (hash-based) engine for this
// one job, reusing the same scan/plan/execute/manifest-update shape as
// app.Run's always-on loop.
func SyncOnce(ctx context.Context, opts SyncOptions) error {
	if opts.Workers < 1 {
		opts.Workers = 1
	}

	// Opened before prepareRoots (rather than after, as before) so
	// validateRoots can check for prior two-way job_state - see its own doc
	// comment and hasPriorTwoWayState for why that ordering matters.
	sqlDB, err := db.Open(opts.DBPath)
	if err != nil {
		return fmt.Errorf("open state db: %w", err)
	}
	defer sqlDB.Close()

	hasPriorState, err := hasPriorTwoWayState(ctx, sqlDB, opts)
	if err != nil {
		return fmt.Errorf("check prior job state: %w", err)
	}

	if err := prepareRoots(opts, hasPriorState); err != nil {
		return err
	}

	if opts.UseHash {
		return runRobustOnce(ctx, sqlDB, opts)
	}
	return runFastOnce(ctx, sqlDB, opts)
}

// hasPriorTwoWayState reports whether persisted job_state rows already exist
// for opts' state key. Only meaningful for fast two-way (mirror/update/--hash
// already require their source to exist unconditionally, regardless of
// prior state - see validateRoots), where it decides whether an
// auto-created empty root is safe.
func hasPriorTwoWayState(ctx context.Context, sqlDB *sql.DB, opts SyncOptions) (bool, error) {
	if opts.UseHash || opts.Mode != reconcile.FastTwoWay {
		return false, nil
	}
	prev, err := db.NewJobStateRepo(sqlDB).Load(ctx, stateKey(opts))
	if err != nil {
		return false, err
	}
	return len(prev) > 0, nil
}

// stateKey returns opts.StateKey if the caller set one, else opts.Name -
// SyncOnce itself doesn't require a canonical StateKey (existing tests and
// any direct caller can keep using bare Name), but cli.runSyncJob always
// sets it, deriving it from the job's canonicalized root pair so two jobs
// sharing a Name never collide on persisted job_state (review finding P1
// #1, Codex).
func stateKey(opts SyncOptions) string {
	if opts.StateKey != "" {
		return opts.StateKey
	}
	return opts.Name
}

// prepareRoots ensures both sync roots exist before scanning (ad-hoc jobs
// are commonly pointed at a destination that doesn't exist yet - that's the
// point of `mirror` - unlike the always-on daemon, which requires --init
// first and so can assume both roots are already there) - except for
// --dry-run, which must not create anything on disk at all (see
// validateRoots/createRoots below for why the two are split).
func prepareRoots(opts SyncOptions, hasPriorState bool) error {
	if err := validateRoots(opts, hasPriorState); err != nil {
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
// (opts.Left) must already exist.
//
// two-way (fast) is symmetric and safe to auto-create ONLY when
// hasPriorState is false - "a missing side with no prior persisted state
// produces copies, not deletes" is true precisely because there is nothing
// in job_state for FastCompare to read as "this path used to exist here".
// Once job_state DOES have rows for this job (a prior successful sync), that
// reasoning breaks: a left root that has vanished entirely (wrong mount
// point, accidental deletion, replaced disk - not just one file removed
// from within it) would otherwise be silently recreated empty, and
// FastCompare's lDeleted branch would read every previously-tracked path as
// "deleted locally", emitting a DeleteR for each and wiping right under
// --yes (review finding, GLM-5.2). So once hasPriorState is true, two-way
// gets the exact same must-already-exist guard as mirror/update, for BOTH
// sides.
func validateRoots(opts SyncOptions, hasPriorState bool) error {
	overlap, err := rootsOverlap(opts.Left, opts.Right)
	if err != nil {
		return fmt.Errorf("resolve sync roots: %w", err)
	}
	if overlap {
		return fmt.Errorf("left and right sync roots must not be the same directory or nested inside one another: %s, %s", opts.Left, opts.Right)
	}
	if !opts.UseHash && opts.Mode == reconcile.FastTwoWay {
		if !hasPriorState {
			return nil
		}
		if err := requireExistingDir(opts.Left, "left"); err != nil {
			return err
		}
		return requireExistingDir(opts.Right, "right")
	}
	return requireExistingDir(opts.Left, "source")
}

// requireExistingDir errors unless path exists and is a real directory.
// os.Lstat, not os.Stat: filepath.WalkDir (fs.ScanTree) Lstats the root
// itself, so a symlink there is never followed and never recurses into -
// its callback fires once at path==root and returns immediately, producing
// a silently EMPTY scan rather than an error. Using Stat here (which
// follows symlinks) would let a symlink-to-a-real-directory root pass
// validation while still scanning as empty, so mirror/two-way would delete
// every file already on the other side.
func requireExistingDir(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s directory does not exist: %s", label, path)
		}
		return fmt.Errorf("stat %s directory %s: %w", label, path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory: %s", label, path)
	}
	return nil
}

// rootsOverlap reports whether a and b denote the same directory, or one is
// nested inside the other - checked unconditionally for every mode
// (including two-way, which has no source-must-exist requirement of its
// own), since scanning a destination that lives inside its own source scans
// the same files twice under different relative-path keys (e.g. --left
// /data --right /data/backup: a file under backup/ is keyed "backup/foo"
// from the left scan but "foo" from the right scan), so a reconciler sees no
// correspondence between them and treats one side's real files as
// extraneous on the other - mirror would then delete them.
//
// Symlinks are resolved via filepath.EvalSymlinks so a and b that merely
// alias the same directory through a symlink are also caught; a path that
// doesn't exist yet (legitimate for two-way's auto-created roots) is
// compared on its cleaned absolute form only, since there is nothing to
// resolve.
func rootsOverlap(a, b string) (bool, error) {
	absA, err := filepath.Abs(a)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", a, err)
	}
	absB, err := filepath.Abs(b)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", b, err)
	}
	resolvedA := resolveIfExists(absA)
	resolvedB := resolveIfExists(absB)
	return pathContainsOrEquals(resolvedA, resolvedB) || pathContainsOrEquals(resolvedB, resolvedA), nil
}

// resolveIfExists returns p with symlinks resolved via filepath.EvalSymlinks
// when p exists, or p unchanged (already filepath.Abs'd by the caller) when
// it doesn't - a path that isn't there yet has nothing to resolve, and that
// is not an error condition here (two-way legitimately auto-creates roots).
func resolveIfExists(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// pathContainsOrEquals reports whether child is parent itself, or a
// descendant of it, after both have already been made absolute (and
// symlink-resolved where possible) by the caller.
func pathContainsOrEquals(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
		prev, err = repo.Load(ctx, stateKey(opts))
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

	// Nothing to do: report it, but don't ask "continue with the file
	// transfers?" about zero transfers, or start an empty progress bar.
	// Two-way state is still persisted below - a first run over two
	// already-identical trees must still establish its baseline.
	if len(res.Tasks) == 0 {
		fmt.Println(fastSummary(opts.Name, opts.Mode, res.Tasks))
	} else {
		if !opts.Assume {
			if !confirmOrDefault(opts.Confirm)(fastSummary(opts.Name, opts.Mode, res.Tasks)) {
				return ErrAborted
			}
		}
		if err := execute.RunFastTasks(ctx, opts.Workers, opts.Left, opts.Right, res.Tasks, opts.Progress); err != nil {
			return fmt.Errorf("run fast tasks: %w", err)
		}
	}

	if opts.Mode == reconcile.FastTwoWay {
		if err := repo.Replace(ctx, stateKey(opts), res.NewState); err != nil {
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
	remote, err := scanRootOrEmpty(ctx, opts.Right, db.RemoteStatView(manifest), false, matcher, policy)
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
// (see sync_loop.go), so the --hash path benefits from the same empty-dir
// create/cleanup and scan-progress logging as the always-on watch loop.
//
// The interactive confirm-before-mutate step (!opts.Assume) runs INSIDE that
// one cycle, via RobustParams.ConfirmPlan, between planning and executing:
// the user approves exactly the actions that then execute. (It used to
// build a separate preview plan from a second full HashAlways scan - twice
// the hashing of both trees, and the plan that actually ran came from the
// later scan, so it could include actions the user never saw.) A plan with
// no file actions runs without prompting.
//
// --dry-run still builds its own preview (scanPairForOpts), since it must
// tolerate a destination that doesn't exist yet and must never reach
// RunRobustOnce at all.
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

	if opts.DryRun {
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
		fmt.Println(robustSummary(opts.Name, nonNoOpActions(plan)))
		return nil
	}

	var confirmPlan func([]reconcile.Action) bool
	if !opts.Assume {
		confirm := confirmOrDefault(opts.Confirm)
		confirmPlan = func(actions []reconcile.Action) bool {
			summary := robustSummary(opts.Name, actions)
			if len(actions) == 0 {
				fmt.Println(summary)
				return true
			}
			return confirm(summary)
		}
	}

	logger, err := runtime.NewLogger(false, "")
	if err != nil {
		return fmt.Errorf("create logger: %w", err)
	}
	defer logger.Close()

	_, failures, runErr := RunRobustOnce(ctx, RobustParams{
		LocalRoot:   opts.Left,
		RemoteRoot:  opts.Right,
		Workers:     opts.Workers,
		Repo:        repo,
		DB:          sqlDB,
		Logger:      logger,
		Policy:      fs.HashAlways,
		ConfirmPlan: confirmPlan,
	})
	if errors.Is(runErr, ErrAborted) {
		return ErrAborted
	}
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

// nonNoOpActions returns plan's actions minus the NoOps - the ones that
// would actually execute.
func nonNoOpActions(plan reconcile.Plan) []reconcile.Action {
	out := make([]reconcile.Action, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		if action.Type != reconcile.ActionNoOp {
			out = append(out, action)
		}
	}
	return out
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
