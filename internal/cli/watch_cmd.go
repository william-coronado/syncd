package cli

import (
	"context"
	"fmt"
	"os/signal"
	"path/filepath"
	stdruntime "runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"syncd/internal/app"
	"syncd/internal/fs"
	"syncd/internal/reconcile"
	"syncd/internal/watch"
)

// watchDebounce is the fsnotify batch-coalescing window. It is small enough
// that local edits still feel "instant" and isn't expected to need tuning
// per-deployment, so it is a constant rather than a flag.
const watchDebounce = 500 * time.Millisecond

// watchFlags holds the `syncd watch` flag values. `syncd service install`
// registers the same set (see addWatchFlags), so a service can be given
// exactly the flags the daemon itself accepts.
type watchFlags struct {
	local, remote        string
	remotePoll, deepScan time.Duration
	silent               bool
	workers              int
	stateDir             string
	dbPath               string
	logPath              string
	lockPath             string
	waitForRemote        bool
	maxDelete            string
	noTrash              bool
	trashDays            int
}

var watchOpts watchFlags

// addWatchFlags registers the watch daemon's flags on cmd, bound to o.
func addWatchFlags(cmd *cobra.Command, o *watchFlags) {
	f := cmd.Flags()
	f.StringVar(&o.local, "local", "", "Local directory path (required)")
	f.StringVar(&o.remote, "remote", "", "Remote directory path (required)")
	f.DurationVar(&o.remotePoll, "remote-poll", 30*time.Second, "Full remote poll cadence (catches other machines' remote writes)")
	f.DurationVar(&o.deepScan, "deep-scan", time.Hour, "Full-tree deep-scan safety-net cadence")
	f.BoolVar(&o.silent, "silent", false, "Write structured logs only, suppress stdout")
	f.IntVar(&o.workers, "workers", stdruntime.NumCPU(), "Worker pool size")
	f.StringVar(&o.stateDir, "state-dir", "", "State directory for lock/db/log/trash")
	f.StringVar(&o.dbPath, "db-path", "", "Path to SQLite manifest file")
	f.StringVar(&o.logPath, "log-path", "", "Path to JSON log file")
	f.StringVar(&o.lockPath, "lock-path", "", "Path to lock file")
	f.BoolVar(&o.waitForRemote, "wait-for-remote", false, "Wait for the remote share to be mounted instead of exiting when it is missing at startup")
	f.StringVar(&o.maxDelete, "max-delete", reconcile.DefaultDeleteLimit, maxDeleteUsage+"; a cycle over the limit is skipped until 'syncd approve-deletes' allows it")
	f.BoolVar(&o.noTrash, "no-trash", false, noTrashUsage)
	f.IntVar(&o.trashDays, "trash-days", defaultTrashDays, trashDaysUsage)
	_ = cmd.MarkFlagRequired("local")
	_ = cmd.MarkFlagRequired("remote")
}

// Shared by `syncd sync` and `syncd watch`.
const (
	maxDeleteUsage   = "Most files one unattended run may delete from either side: a count (100), a percentage of the files there (50%; never trips on 10 or fewer), or off"
	noTrashUsage     = "Delete files outright instead of moving those on a local disk to <state-dir>/trash"
	trashDaysUsage   = "Days to keep trashed files (0 keeps them forever)"
	defaultTrashDays = 30
)

// watchCmd is the always-on daemon path of the merged tool: `syncd watch`.
// It combines an fsnotify watcher for near-instant local-change propagation
// with periodic full remote polls (catches writes made by other machines on
// the same remote) and a periodic full deep scan (catches new empty
// directories and recursively-deleted local directories, which the
// fsnotify-driven scoped reconcile can't see - see internal/watch/loop.go).
var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Run the always-on watch daemon (local events + remote poll + deep scan)",
	Long: `Run syncd as an always-on daemon: local filesystem changes under --local are
reconciled to --remote almost immediately via an fsnotify watcher, a full
remote poll runs every --remote-poll to catch changes made directly on the
remote by another machine, and a full deep scan runs every --deep-scan as a
safety net (it is the only pass that picks up newly created empty
directories and directories deleted recursively on the local side).

Run 'syncd init --local ... --remote ...' first to bootstrap the roots and
the remote canary file.`,
	RunE: runWatchCmd,
}

func init() {
	addWatchFlags(watchCmd, &watchOpts)
	rootCmd.AddCommand(watchCmd)
}

// validateWatchFlags rejects CLI-supplied watch flag values that would
// otherwise reach lower layers unchecked: workers < 1, or a non-positive
// --remote-poll/--deep-scan, which would panic time.NewTicker deep inside
// watch.Run (that library also defends on its own - see loop.go - but the
// CLI should give a clear error up front rather than let it fall through).
func validateWatchFlags(workers int, remotePoll, deepScan time.Duration) error {
	if workers < 1 {
		return fmt.Errorf("--workers must be >= 1")
	}
	if remotePoll <= 0 {
		return fmt.Errorf("--remote-poll must be > 0 (got %s)", remotePoll)
	}
	if deepScan <= 0 {
		return fmt.Errorf("--deep-scan must be > 0 (got %s)", deepScan)
	}
	return nil
}

// watchConfig validates o and turns it into the daemon's configuration,
// with both roots made absolute.
func (o watchFlags) watchConfig() (app.WatchConfig, error) {
	localAbs, err := filepath.Abs(o.local)
	if err != nil {
		return app.WatchConfig{}, fmt.Errorf("resolve --local: %w", err)
	}
	remoteAbs, err := filepath.Abs(o.remote)
	if err != nil {
		return app.WatchConfig{}, fmt.Errorf("resolve --remote: %w", err)
	}
	if err := validateWatchFlags(o.workers, o.remotePoll, o.deepScan); err != nil {
		return app.WatchConfig{}, err
	}
	maxDelete, err := reconcile.ParseDeleteLimit(o.maxDelete)
	if err != nil {
		return app.WatchConfig{}, fmt.Errorf("--max-delete: %w", err)
	}
	if o.trashDays < 0 {
		return app.WatchConfig{}, fmt.Errorf("--trash-days must be >= 0")
	}
	return app.WatchConfig{
		LocalRoot:     localAbs,
		RemoteRoot:    remoteAbs,
		Silent:        o.silent,
		Workers:       o.workers,
		StateDir:      o.stateDir,
		DBPath:        o.dbPath,
		LogPath:       o.logPath,
		LockPath:      o.lockPath,
		RemotePoll:    o.remotePoll,
		DeepScan:      o.deepScan,
		Debounce:      watchDebounce,
		WaitForRemote: o.waitForRemote,
		MaxDelete:     maxDelete,
		NoTrash:       o.noTrash,
		TrashDays:     o.trashDays,
	}, nil
}

func runWatchCmd(cmd *cobra.Command, _ []string) error {
	cfg, err := watchOpts.watchConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return app.RunWatch(ctx, cfg, func(ctx context.Context, p app.RobustParams, matcher *fs.IgnoreMatcher, cfg app.WatchConfig) error {
		return watch.Run(ctx, p, matcher, watch.LoopConfig{
			RemotePoll: cfg.RemotePoll,
			DeepScan:   cfg.DeepScan,
			Debounce:   cfg.Debounce,
		})
	})
}
