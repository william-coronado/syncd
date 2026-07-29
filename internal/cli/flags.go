package cli

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"runtime"
)

type Config struct {
	LocalRoot  string
	RemoteRoot string
	DryRun     bool
	Silent     bool
	Init       bool
	Workers    int
	StateDir   string
	DBPath     string
	LogPath    string
	LockPath   string
}

func ParseArgs(args []string) (Config, error) {
	cfg := Config{}
	fs := flag.NewFlagSet("drivesync", flag.ContinueOnError)

	fs.StringVar(&cfg.LocalRoot, "local", "", "Local directory path")
	fs.StringVar(&cfg.RemoteRoot, "remote", "", "Mounted SMB directory path")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "Calculate and print sync actions only")
	fs.BoolVar(&cfg.Silent, "silent", false, "Write structured logs only, suppress stdout")
	fs.BoolVar(&cfg.Init, "init", false, "Bootstrap local/remote directories and the remote canary file, then exit")
	fs.IntVar(&cfg.Workers, "workers", runtime.NumCPU(), "Worker pool size")
	fs.StringVar(&cfg.StateDir, "state-dir", "", "State directory for lock/db/log")
	fs.StringVar(&cfg.DBPath, "db-path", "", "Path to SQLite manifest file")
	fs.StringVar(&cfg.LogPath, "log-path", "", "Path to JSON log file")
	fs.StringVar(&cfg.LockPath, "lock-path", "", "Path to lock file")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if cfg.LocalRoot == "" || cfg.RemoteRoot == "" {
		return cfg, errors.New("both --local and --remote are required")
	}
	if cfg.Workers < 1 {
		return cfg, errors.New("--workers must be >= 1")
	}

	localAbs, err := filepath.Abs(cfg.LocalRoot)
	if err != nil {
		return cfg, fmt.Errorf("resolve --local: %w", err)
	}
	remoteAbs, err := filepath.Abs(cfg.RemoteRoot)
	if err != nil {
		return cfg, fmt.Errorf("resolve --remote: %w", err)
	}
	cfg.LocalRoot = localAbs
	cfg.RemoteRoot = remoteAbs
	return cfg, nil
}
