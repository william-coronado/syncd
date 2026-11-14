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
		lockPath = filepath.Join(stateDir, defaultLockName(cfg.LocalRoot, cfg.RemoteRoot))
	}

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
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

// defaultLockName mirrors defaultManifestDBName's root-pair hashing for the
// default lock filename: the default database is already isolated per root
// pair (see defaultManifestDBName), but the default lock was left as the
// fixed "sync.lock" - so two `syncd watch` instances for unrelated root
// pairs could never run concurrently, contrary to RunWatch's stated
// one-watcher-per-local-root behavior (review finding P2 #4, Codex). Falls
// back to the historical fixed name "sync.lock" when both roots are empty,
// for the same reason defaultManifestDBName does: existing callers that
// never set them keep today's behavior.
func defaultLockName(localRoot, remoteRoot string) string {
	if localRoot == "" && remoteRoot == "" {
		return "sync.lock"
	}
	sum := sha256.Sum256([]byte(localRoot + "\x00" + remoteRoot))
	return "sync-" + hex.EncodeToString(sum[:6]) + ".lock"
}

// defaultLogName gives the default log file the same per-root-pair name as
// defaultLockName/defaultManifestDBName. With one fixed "sync.log", two
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
