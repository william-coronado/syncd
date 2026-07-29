package runtime

import (
	"fmt"
	"os"
	"path/filepath"

	"drivesync/internal/cli"
)

type RuntimePaths struct {
	StateDir string
	DBPath   string
	LogPath  string
	LockPath string
}

func ResolvePaths(cfg cli.Config) (RuntimePaths, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return RuntimePaths{}, fmt.Errorf("resolve home directory: %w", err)
	}

	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(homeDir, ".local", "state", "drivesync")
	}
	dbPath := cfg.DBPath
	if dbPath == "" {
		dbPath = filepath.Join(stateDir, "manifest.db")
	}
	logPath := cfg.LogPath
	if logPath == "" {
		logPath = filepath.Join(stateDir, "sync.log")
	}
	lockPath := cfg.LockPath
	if lockPath == "" {
		lockPath = filepath.Join(stateDir, "sync.lock")
	}

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return RuntimePaths{}, fmt.Errorf("create state dir: %w", err)
	}

	return RuntimePaths{
		StateDir: stateDir,
		DBPath:   dbPath,
		LogPath:  logPath,
		LockPath: lockPath,
	}, nil
}
