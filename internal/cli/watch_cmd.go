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
	"syncd/internal/watch"
)

// watchDebounce is the fsnotify batch-coalescing window. It is small enough
// that local edits still feel "instant" and isn't expected to need tuning
// per-deployment, so it is a constant rather than a flag.
const watchDebounce = 500 * time.Millisecond

var (
	watchLocal      string
	watchRemote     string
	watchRemotePoll time.Duration
	watchDeepScan   time.Duration
	watchSilent     bool
	watchWorkers    int
	watchStateDir   string
	watchDBPath     string
	watchLogPath    string
	watchLockPath   string
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
	watchCmd.Flags().StringVar(&watchLocal, "local", "", "Local directory path (required)")
	watchCmd.Flags().StringVar(&watchRemote, "remote", "", "Remote directory path (required)")
	watchCmd.Flags().DurationVar(&watchRemotePoll, "remote-poll", 30*time.Second, "Full remote poll cadence (catches other machines' remote writes)")
	watchCmd.Flags().DurationVar(&watchDeepScan, "deep-scan", time.Hour, "Full-tree deep-scan safety-net cadence")
	watchCmd.Flags().BoolVar(&watchSilent, "silent", false, "Write structured logs only, suppress stdout")
	watchCmd.Flags().IntVar(&watchWorkers, "workers", stdruntime.NumCPU(), "Worker pool size")
	watchCmd.Flags().StringVar(&watchStateDir, "state-dir", "", "State directory for lock/db/log")
	watchCmd.Flags().StringVar(&watchDBPath, "db-path", "", "Path to SQLite manifest file")
	watchCmd.Flags().StringVar(&watchLogPath, "log-path", "", "Path to JSON log file")
	watchCmd.Flags().StringVar(&watchLockPath, "lock-path", "", "Path to lock file")
	_ = watchCmd.MarkFlagRequired("local")
	_ = watchCmd.MarkFlagRequired("remote")

	rootCmd.AddCommand(watchCmd)
}

func runWatchCmd(cmd *cobra.Command, _ []string) error {
	localAbs, err := filepath.Abs(watchLocal)
	if err != nil {
		return fmt.Errorf("resolve --local: %w", err)
	}
	remoteAbs, err := filepath.Abs(watchRemote)
	if err != nil {
		return fmt.Errorf("resolve --remote: %w", err)
	}
	if watchWorkers < 1 {
		return fmt.Errorf("--workers must be >= 1")
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := app.WatchConfig{
		LocalRoot:  localAbs,
		RemoteRoot: remoteAbs,
		Silent:     watchSilent,
		Workers:    watchWorkers,
		StateDir:   watchStateDir,
		DBPath:     watchDBPath,
		LogPath:    watchLogPath,
		LockPath:   watchLockPath,
		RemotePoll: watchRemotePoll,
		DeepScan:   watchDeepScan,
		Debounce:   watchDebounce,
	}

	return app.RunWatch(ctx, cfg, func(ctx context.Context, p app.RobustParams, matcher *fs.IgnoreMatcher, cfg app.WatchConfig) error {
		return watch.Run(ctx, p, matcher, watch.LoopConfig{
			RemotePoll: cfg.RemotePoll,
			DeepScan:   cfg.DeepScan,
			Debounce:   cfg.Debounce,
		})
	})
}
