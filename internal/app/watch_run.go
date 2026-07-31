package app

import (
	"context"
	"time"

	"syncd/internal/db"
	"syncd/internal/fs"
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

// RunWatch bootstraps and runs the always-on watch daemon: it resolves
// runtime paths, builds the logger, acquires the daemon lock (held for the
// whole process lifetime - only one watcher may run per local root at a
// time), verifies the remote canary once up front (the loop re-checks it on
// every cycle thereafter), opens the manifest db, loads the ignore matcher,
// and then hands off to runLoop (watch.Run in production), which blocks
// until ctx is cancelled.
func RunWatch(ctx context.Context, cfg WatchConfig, runLoop WatchLoopFunc) error {
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

	p := RobustParams{
		LocalRoot:  cfg.LocalRoot,
		RemoteRoot: cfg.RemoteRoot,
		Workers:    cfg.Workers,
		Repo:       repo,
		DB:         sqlDB,
		Logger:     logger,
	}

	return runLoop(ctx, p, matcher, cfg)
}
