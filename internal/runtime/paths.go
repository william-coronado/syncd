package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// trashDirName must match fs.TrashDirName (runtime is a leaf package and
// cannot import fs).
const trashDirName = "trash"

// stateDirMode and stateFileMode keep syncd's state private to the user:
// the log and the manifest list every synced path. (Directories that
// already exist keep their mode; the files in them are owner-only either
// way, see NewLogger and db.Open.)
const (
	stateDirMode  = 0o700
	stateFileMode = 0o600
)

type RuntimePaths struct {
	StateDir string
	DBPath   string
	LogPath  string
	LockPath string
	// TrashDir is where deleted local files are moved (see fs.Trash).
	TrashDir string
	// ApprovePath is the one-shot token `syncd approve-deletes` writes to
	// let this root pair's next over-limit cycle run (see DeleteApproval).
	ApprovePath string
	// RefusalPath is where the watch daemon records the cycle it most
	// recently refused for exceeding its delete limit (see DeleteRefusal).
	RefusalPath string
}

func ResolvePaths(cfg Config) (RuntimePaths, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return RuntimePaths{}, fmt.Errorf("resolve home directory: %w", err)
	}

	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(homeDir, ".local", "state", "syncd")
	}
	dbPath := cfg.DBPath
	if dbPath == "" {
		dbPath = filepath.Join(stateDir, defaultManifestDBName(cfg.LocalRoot, cfg.RemoteRoot))
	}
	logPath := cfg.LogPath
	if logPath == "" {
		logPath = filepath.Join(stateDir, defaultLogName(cfg.LocalRoot, cfg.RemoteRoot))
	}
	lockPath := cfg.LockPath
	if lockPath == "" {
		name, err := defaultLockName(cfg.LocalRoot, cfg.RemoteRoot)
		if err != nil {
			return RuntimePaths{}, err
		}
		lockPath = filepath.Join(stateDir, name)
	}

	if err := os.MkdirAll(stateDir, stateDirMode); err != nil {
		return RuntimePaths{}, fmt.Errorf("create state dir: %w", err)
	}

	return RuntimePaths{
		StateDir: stateDir,
		DBPath:   dbPath,
		LogPath:  logPath,
		LockPath: lockPath,
		TrashDir: filepath.Join(stateDir, trashDirName),
		// Named after the lock, so it is per root pair exactly as the lock is.
		ApprovePath: strings.TrimSuffix(lockPath, ".lock") + ".approve-deletes",
		RefusalPath: strings.TrimSuffix(lockPath, ".lock") + ".refused-deletes",
	}, nil
}

// defaultManifestDBName derives the default watch manifest db filename from
// cfg.LocalRoot+RemoteRoot, so two `syncd watch` invocations against
// different root pairs never share a manifest.db by default. file_manifest
// (internal/db/sqlite.go) is keyed only by relative path, with no root-pair
// column - two unrelated pairs sharing one db would let pair B's scan read
// pair A's rows as its own baseline, treating pair B's genuinely-new files
// as "already synced, now deleted" and deleting or overwriting them (see
// review finding "Critical 3"). Mirrors cli/sync_cmd.go's hashJobDBPath,
// which already gives each --hash sync job its own db for the identical
// reason; this closes the same gap for the always-on watch daemon.
//
// Falls back to the historical fixed name "manifest.db" when both roots are
// empty, so callers that never set them (existing tests, or any future
// caller of ResolvePaths that isn't root-pair-scoped) keep today's behavior
// rather than silently landing on a path nobody asked for.
func defaultManifestDBName(localRoot, remoteRoot string) string {
	if localRoot == "" && remoteRoot == "" {
		return "manifest.db"
	}
	sum := sha256.Sum256([]byte(localRoot + "\x00" + remoteRoot))
	return "manifest-" + hex.EncodeToString(sum[:6]) + ".db"
}

// defaultLockName uses the same canonical, unordered identity as sync.
func defaultLockName(localRoot, remoteRoot string) (string, error) {
	if localRoot == "" && remoteRoot == "" {
		return "sync.lock", nil
	}
	key, err := RootPairKey(localRoot, remoteRoot)
	if err != nil {
		return "", err
	}
	return "sync-" + key + ".lock", nil
}

// defaultLogName gives the default log file the same ordered root-pair identity
// as defaultManifestDBName. With one fixed "sync.log", two
// watch daemons for different pairs interleaved their events in one file,
// and one daemon's in-place rotation renamed the file out from under the
// other, whose writes then landed in sync.log.old (docs/follow-ups.md #27).
// Falls back to "sync.log" when both roots are empty, as the others do.
func defaultLogName(localRoot, remoteRoot string) string {
	if localRoot == "" && remoteRoot == "" {
		return "sync.log"
	}
	sum := sha256.Sum256([]byte(localRoot + "\x00" + remoteRoot))
	return "sync-" + hex.EncodeToString(sum[:6]) + ".log"
}
