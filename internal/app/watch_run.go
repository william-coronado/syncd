package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"syncd/internal/db"
	"syncd/internal/fs"
	"syncd/internal/reconcile"
	"syncd/internal/runtime"
)

// WatchConfig configures RunWatch: the always-on, event-driven daemon
// (`syncd watch`). It mirrors runtime.Config's path/runtime-resolution
// fields (state dir / db / log / lock overrides, local+remote roots,
// worker count, silent logging) plus the watch loop's three cadences.
type WatchConfig struct {
	LocalRoot  string
	RemoteRoot string
	Silent     bool
	Workers    int
	StateDir   string
	DBPath     string
	LogPath    string
	LockPath   string

	RemotePoll, DeepScan, Debounce time.Duration

	// WaitForRemote makes RunWatch wait for the remote root and its canary
	// to appear instead of failing when they are missing at startup - what a
	// service started at login/boot, before the share is mounted, needs.
	WaitForRemote bool
	// MaxDelete caps the deletes one cycle may make (see
	// RobustParams.MaxDelete); the zero value imposes no limit.
	MaxDelete reconcile.DeleteLimit
	// NoTrash removes deleted local files outright instead of moving them
	// to the state directory's trash. TrashDays is how long trashed files
	// are kept (<= 0: forever).
	NoTrash   bool
	TrashDays int
	// FirstSync is the policy for files that differ on the pair's first
	// cycle (see FirstSyncPolicy).
	FirstSync FirstSyncPolicy
}

// WatchLoopFunc runs the actual event/poll/deep-scan loop (watch.Run) once
// RunWatch has finished its setup. It is injected by the caller rather than
// called directly from this package because the watch package already
// imports app (for RobustParams/RunRobustOnce - see
// internal/watch/incremental.go and loop.go); app importing watch back would
// be an import cycle. internal/cli, which imports both app and watch, is
// where the two are wired together for real (see cli/watch_cmd.go); tests of
// RunWatch's setup/lifecycle can inject a fake instead of pulling in a real
// fsnotify watcher.
type WatchLoopFunc func(ctx context.Context, p RobustParams, matcher *fs.IgnoreMatcher, cfg WatchConfig) error

// RunWatch bootstraps and runs the always-on watch daemon: it resolves both
// roots' symlinks once up front and uses the resolved paths for everything
// downstream (see resolveWatchRoot - review finding), rejects overlapping
// roots (same rootsOverlap check SyncOnce uses - a destination nested inside
// its own source, or vice versa, double-keys files across the two scans and
// corrupts reconciliation, notably on the first deep scan; review finding),
// resolves runtime paths, builds the logger, acquires the daemon lock (held
// for the whole process lifetime - only one watcher may run per local root
// at a time), verifies the remote canary once up front (the loop re-checks
// it on every cycle thereafter), opens the manifest db, loads the ignore
// matcher, and then hands off to runLoop (watch.Run in production), which
// blocks until ctx is cancelled.
func RunWatch(ctx context.Context, cfg WatchConfig, runLoop WatchLoopFunc) error {
	if cfg.WaitForRemote {
		if !waitForRemote(ctx, cfg.RemoteRoot) {
			return nil // cancelled while waiting: a clean shutdown
		}
	}
	resolvedLocal, err := resolveWatchRoot("local", cfg.LocalRoot)
	if err != nil {
		return err
	}
	resolvedRemote, err := resolveWatchRoot("remote", cfg.RemoteRoot)
	if err != nil {
		return err
	}
	cfg.LocalRoot = resolvedLocal
	cfg.RemoteRoot = resolvedRemote

	overlap, err := rootsOverlap(cfg.LocalRoot, cfg.RemoteRoot)
	if err != nil {
		return fmt.Errorf("resolve watch roots: %w", err)
	}
	if overlap {
		return fmt.Errorf("local and remote watch roots must not be the same directory or nested inside one another: %s, %s", cfg.LocalRoot, cfg.RemoteRoot)
	}

	paths, err := runtime.ResolvePaths(runtime.Config{
		LocalRoot:  cfg.LocalRoot,
		RemoteRoot: cfg.RemoteRoot,
		StateDir:   cfg.StateDir,
		DBPath:     cfg.DBPath,
		LogPath:    cfg.LogPath,
		LockPath:   cfg.LockPath,
	})
	if err != nil {
		return err
	}

	logger, err := runtime.NewLogger(cfg.Silent, paths.LogPath)
	if err != nil {
		return err
	}
	defer logger.Close()
	// The default log is named per root pair, so say where it is.
	logger.Printf("syncd watch: local=%s remote=%s log=%s", cfg.LocalRoot, cfg.RemoteRoot, paths.LogPath)

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

	matcher, err := fs.LoadIgnoreMatcher(cfg.LocalRoot)
	if err != nil {
		return err
	}

	var trash *fs.Trash
	if !cfg.NoTrash {
		trash = fs.NewTrash(paths.TrashDir)
		go pruneTrashPeriodically(ctx, paths.TrashDir, cfg.TrashDays, logger)
	}

	p := RobustParams{
		LocalRoot:  cfg.LocalRoot,
		RemoteRoot: cfg.RemoteRoot,
		Workers:    cfg.Workers,
		Repo:       repo,
		DB:         sqlDB,
		Logger:     logger,
		MaxDelete:  cfg.MaxDelete,
		Trash:      trash,
		FirstSync:  cfg.FirstSync,
		// One window for the daemon's whole life: the limit is a rolling
		// total across cycles, not a per-cycle allowance.
		DeleteWindow: NewDeleteWindow(DefaultDeleteWindow),
		ApproveDeletes: func(fingerprint string) bool {
			return runtime.ConsumeDeleteApproval(paths.ApprovePath, fingerprint)
		},
		OnDeleteRefused: func(r runtime.DeleteRefusal) {
			if err := runtime.WriteDeleteRefusal(paths.RefusalPath, r); err != nil {
				logger.Warn("could not record the refused deletions; 'syncd approve-deletes' will not find them", map[string]any{"error": err.Error()})
			}
		},
	}

	return runLoop(ctx, p, matcher, cfg)
}

// remoteWaitInterval is how often waitForRemote re-checks; a variable so
// tests don't have to wait out the real interval.
var remoteWaitInterval = 10 * time.Second

// waitForRemote blocks until remoteRoot's canary is readable, reporting
// false if ctx is cancelled first. It runs before the logger exists (the
// log is named after the resolved roots, and an unmounted root can't be
// resolved), so it reports to stderr - once per distinct reason, not once
// per attempt.
func waitForRemote(ctx context.Context, remoteRoot string) bool {
	var lastReason string
	for {
		err := runtime.CheckRemoteCanary(remoteRoot)
		if err == nil {
			if lastReason != "" {
				fmt.Fprintf(os.Stderr, "syncd watch: remote %s is available\n", remoteRoot)
			}
			return true
		}
		if reason := err.Error(); reason != lastReason {
			fmt.Fprintf(os.Stderr, "syncd watch: waiting for remote %s (%s); if the share is mounted and has never been set up, run 'syncd init'\n", remoteRoot, reason)
			lastReason = reason
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(remoteWaitInterval):
		}
	}
}

// pruneTrashPeriodically expires old trash at startup and daily after that,
// until ctx is cancelled.
func pruneTrashPeriodically(ctx context.Context, dir string, keepDays int, logger *runtime.Logger) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		if n, err := fs.PruneTrash(dir, keepDays); err != nil {
			logger.Warn("could not prune trash", map[string]any{"dir": dir, "error": err.Error()})
		} else if n > 0 {
			logger.Printf("pruned %d day(s) of trash older than %d days from %s", n, keepDays, dir)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ApproveDeletes approves, once, the cycle the watch daemon for cfg's root
// pair most recently refused for exceeding --max-delete (see
// RobustParams.ApproveDeletes), and returns that refusal so the caller can
// show what was approved. It resolves the roots and state paths exactly as
// RunWatch does, so it reads the record and writes the token where that
// daemon does. The approval is bound to the refused plan: if the trees
// change so that the daemon plans different deletions, it is not honored.
func ApproveDeletes(cfg WatchConfig) (runtime.DeleteRefusal, error) {
	resolvedLocal, err := resolveWatchRoot("local", cfg.LocalRoot)
	if err != nil {
		return runtime.DeleteRefusal{}, err
	}
	resolvedRemote, err := resolveWatchRoot("remote", cfg.RemoteRoot)
	if err != nil {
		return runtime.DeleteRefusal{}, err
	}
	paths, err := runtime.ResolvePaths(runtime.Config{
		LocalRoot:  resolvedLocal,
		RemoteRoot: resolvedRemote,
		StateDir:   cfg.StateDir,
		LockPath:   cfg.LockPath,
	})
	if err != nil {
		return runtime.DeleteRefusal{}, err
	}
	refusal, err := runtime.ReadDeleteRefusal(paths.RefusalPath)
	if err != nil {
		return runtime.DeleteRefusal{}, err
	}
	return refusal, runtime.WriteDeleteApproval(paths.ApprovePath, refusal.Fingerprint)
}

// resolveWatchRoot resolves root's symlinks (filepath.Abs, used to resolve
// --local/--remote before RunWatch is ever called, does not resolve
// symlinks) and confirms the result is a directory. NewWatcher
// (internal/watch/watcher.go) already does this same resolution for its own
// fsnotify tree, since filepath.WalkDir lstats its root argument and never
// follows a symlink there - a walk over an unresolved symlinked root visits
// nothing. That resolution was local to NewWatcher alone: RobustParams (and
// every ScanTree-driven poll/deep-scan cycle built from it) still carried
// the raw root, so a full scan over an unresolved symlinked root also saw
// that side as completely empty - and once a manifest baseline existed,
// BuildPlan read every already-synced file on the OTHER side as deleted and
// removed it (review finding, GPT-5.6 Sol). Resolving once here, before
// RobustParams is ever built, means every downstream consumer (the ignore
// matcher, the scanner, the watcher) agrees on the same canonical path.
func resolveWatchRoot(label, root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve %s watch root %s: %w", label, root, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %s watch root %s: %w", label, root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s watch root is not a directory: %s", label, root)
	}
	return resolved, nil
}
