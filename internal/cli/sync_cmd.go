package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/dustin/go-humanize"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"

	"syncd/internal/app"
	"syncd/internal/execute"
	"syncd/internal/reconcile"
)

var (
	syncMode    string
	syncLeft    string
	syncRight   string
	syncJobName string
	syncCfgFile string
	syncUseHash bool
	syncAssume  bool
)

// syncCmd is the ad-hoc/fast path of the merged tool: `syncd sync`. It runs
// either a single one-off job (given --left/--right) or every job (or one
// named job, via --job) from the batch config file (~/.syncd.yaml by
// default, or --config).
var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Run an ad-hoc or batch-configured sync job",
	Long: `Run a fast, size+mtime-based sync job (mirror, update, or two-way).

Either pass --left and --right for a one-off job, or omit them to run every
job listed in the config file (~/.syncd.yaml by default, or --config); use
--job to run just one named job from that file. Pass --hash to use the
robust, hash-based engine instead of the fast reconciler for this run.`,
	RunE: runSyncCmd,
}

func init() {
	syncCmd.Flags().StringVar(&syncMode, "mode", "mirror", "Sync mode for a one-off job: mirror, update, two-way")
	syncCmd.Flags().StringVar(&syncLeft, "left", "", "Left folder path (one-off job; requires --right)")
	syncCmd.Flags().StringVar(&syncRight, "right", "", "Right folder path (one-off job; requires --left)")
	syncCmd.Flags().StringVar(&syncJobName, "job", "", "Run only this named job from the config file")
	syncCmd.Flags().StringVar(&syncCfgFile, "config", "", "Config file (default $HOME/.syncd.yaml)")
	syncCmd.Flags().BoolVar(&syncUseHash, "hash", false, "Use the robust hash-based engine instead of the fast size+mtime reconciler")
	syncCmd.Flags().BoolVar(&syncAssume, "yes", false, "Skip the interactive confirmation prompt")

	rootCmd.AddCommand(syncCmd)
}

func runSyncCmd(cmd *cobra.Command, _ []string) error {
	jobs, err := resolveSyncJobs()
	if err != nil {
		return err
	}

	// Fast-mode jobs all share one jobs.db: its job_state table is already
	// namespaced per job via a (job, path) composite primary key, so
	// concurrent/sequential jobs can't corrupt each other's state there.
	// --hash jobs instead get their own per-job db computed inside the loop
	// below (see hashJobDBPath) - file_manifest has no job/name column, only
	// a path primary key, so sharing one db across --hash jobs would let two
	// jobs whose trees contain the same relative path corrupt each other's
	// three-way baseline.
	var jobsDBPath string
	if !syncUseHash {
		var err error
		jobsDBPath, err = defaultJobsDBPath()
		if err != nil {
			return err
		}
	}

	ctx := cmd.Context()
	var failures int
	for _, job := range jobs {
		mode, err := parseFastMode(job.Mode)
		if err != nil {
			return fmt.Errorf("job %q: %w", job.Name, err)
		}

		pterm.DefaultSection.Println("sync job: " + job.Name)

		// execute.RunActions (the --hash/robust path) has no ProgressSink
		// parameter, so a bar started here would never see AddBytes and
		// would sit on screen forever without a Done() to stop it. Only
		// build one for the fast path, which actually drives it.
		var progress execute.ProgressSink
		if !syncUseHash {
			progress = newPtermProgressSink()
		}

		dbPath := jobsDBPath
		if syncUseHash {
			dbPath, err = hashJobDBPath(job.Name)
			if err != nil {
				return fmt.Errorf("job %q: resolve hash manifest db: %w", job.Name, err)
			}
		}

		opts := app.SyncOptions{
			Name:     job.Name,
			Left:     job.Left,
			Right:    job.Right,
			Mode:     mode,
			UseHash:  syncUseHash,
			Assume:   syncAssume,
			Workers:  runtime.NumCPU(),
			DBPath:   dbPath,
			Confirm:  ptermConfirm,
			Progress: progress,
		}

		runErr := app.SyncOnce(ctx, opts)
		switch {
		case errors.Is(runErr, app.ErrAborted):
			pterm.Warning.Printf("sync job %q aborted by user\n", job.Name)
		case runErr != nil:
			failures++
			pterm.Error.Printf("sync job %q failed: %v\n", job.Name, runErr)
		default:
			pterm.Success.Printf("sync job %q complete\n", job.Name)
		}
	}

	if failures > 0 {
		return fmt.Errorf("%d of %d sync job(s) failed", failures, len(jobs))
	}
	return nil
}

// resolveSyncJobs applies the --left/--right vs. config-file precedence: an
// explicit --left/--right pair always means "run just this one-off job" and
// the config file is not consulted at all; otherwise every job (or, with
// --job, just the named one) is loaded from the config file.
func resolveSyncJobs() ([]SyncJob, error) {
	if syncLeft != "" || syncRight != "" {
		if syncLeft == "" || syncRight == "" {
			return nil, errors.New("--left and --right must be given together")
		}
		return []SyncJob{{Name: adHocJobName(syncLeft, syncRight), Left: syncLeft, Right: syncRight, Mode: syncMode}}, nil
	}

	cfg, err := loadSyncConfig(syncCfgFile)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if len(cfg.Jobs) == 0 {
		return nil, errors.New("no jobs found in config or command line arguments; use --left/--right or a config file (~/.syncd.yaml)")
	}

	if syncJobName == "" {
		return cfg.Jobs, nil
	}
	for _, j := range cfg.Jobs {
		if j.Name == syncJobName {
			return []SyncJob{j}, nil
		}
	}
	return nil, fmt.Errorf("job %q not found in config", syncJobName)
}

// adHocJobName derives a stable job name for a one-off --left/--right pair
// from a short hash of the pair itself, rather than a constant. Mirror and
// update ignore Name entirely, but two-way persists state keyed by it in
// job_state - a constant name would let two different ad-hoc two-way pairs
// collide on the same row and corrupt each other's state.
func adHocJobName(left, right string) string {
	sum := sha256.Sum256([]byte(left + "\x00" + right))
	return "cli-" + hex.EncodeToString(sum[:6])
}

func parseFastMode(mode string) (reconcile.FastMode, error) {
	switch reconcile.FastMode(mode) {
	case "":
		return reconcile.FastMirror, nil
	case reconcile.FastMirror, reconcile.FastUpdate, reconcile.FastTwoWay:
		return reconcile.FastMode(mode), nil
	default:
		return "", fmt.Errorf("invalid mode %q (want mirror, update, or two-way)", mode)
	}
}

// syncdStateDir returns (creating it if necessary) the shared syncd state
// directory, ~/.local/state/syncd - the parent of jobs.db, watch's
// manifest.db, and each --hash job's own hash-<id>.db.
func syncdStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	dir := filepath.Join(home, ".local", "state", "syncd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	return dir, nil
}

// defaultJobsDBPath is where fast-mode (mirror/update/two-way) job state is
// persisted for ad-hoc/batch `syncd sync` jobs. It is deliberately separate
// from the always-on daemon's manifest.db, and is never used for --hash jobs
// - those get their own per-job db from hashJobDBPath instead.
func defaultJobsDBPath() (string, error) {
	dir, err := syncdStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "jobs.db"), nil
}

// hashJobDBPath returns the manifest db path for a single `--hash` sync job:
// ~/.local/state/syncd/hash-<id>.db, where <id> is jobName run through
// sanitizeJobDBName. jobName is either a named config job's Name or, for an
// ad-hoc --left/--right --hash run, the adHocJobName(left, right) hash that
// resolveSyncJobs already assigns as that job's Name - so this one path
// derivation covers both cases uniformly.
//
// Each --hash job needs its own db because file_manifest (unlike job_state,
// the fast path's table) has no job/name column, just a path primary key:
// two --hash jobs sharing one db - as they did when every job reused
// defaultJobsDBPath's jobs.db - would silently corrupt each other's
// three-way baseline for any relative path their trees happen to share.
func hashJobDBPath(jobName string) (string, error) {
	dir, err := syncdStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hash-"+sanitizeJobDBName(jobName)+".db"), nil
}

// sanitizeJobDBName maps a job name - arbitrary user-supplied text from the
// config file, or a "cli-<hex>" adHocJobName hash - to a safe filename
// component: only ASCII letters, digits, '.', '_', and '-' survive; anything
// else (including path separators) becomes '_'.
func sanitizeJobDBName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// ptermConfirm is the interactive Confirm callback passed to app.SyncOnce:
// it prints the summary app.SyncOnce built and asks the user to approve
// before anything is mutated.
func ptermConfirm(summary string) bool {
	pterm.Println(summary)
	result, _ := pterm.DefaultInteractiveConfirm.Show("Do you want to continue with the file transfers?")
	return result
}

// ptermProgressSink is a pterm-backed execute.ProgressSink. AddBytes is
// invoked from multiple RunFastTasks/RunActions worker goroutines
// concurrently, so access to the underlying bar is guarded by mu.
type ptermProgressSink struct {
	mu    sync.Mutex
	bar   *pterm.ProgressbarPrinter
	total int64
}

// newPtermProgressSink starts a pterm progress bar and returns it as an
// execute.ProgressSink. It returns a true nil interface (not a typed nil)
// if the bar fails to start (e.g. non-interactive output), so callers can
// pass it straight through to app.SyncOnce's nil-safe Progress field.
func newPtermProgressSink() execute.ProgressSink {
	bar, err := pterm.DefaultProgressbar.WithTitle("Syncing...").Start()
	if err != nil {
		return nil
	}
	return &ptermProgressSink{bar: bar}
}

func (p *ptermProgressSink) AddBytes(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total += n
	p.bar.Add(int(n)) // Add grows bar.Total automatically if Current exceeds it
	p.bar.UpdateTitle(fmt.Sprintf("Syncing... %s transferred", humanize.Bytes(uint64(p.total))))
}

func (p *ptermProgressSink) Done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = p.bar.Stop()
}
