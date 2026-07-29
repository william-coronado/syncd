package app

import (
	"os"
	"path"
	"path/filepath"

	"drivesync/internal/runtime"
)

// createMissingEmptyDirs mirrors directories that exist on one side but not
// the other. Directories that contain files are already created as a
// side-effect of file copy operations (see fs.EnsureParents), so this only
// matters for directories with no files in them at all - a user organizing
// future files ahead of time. Failures are logged and tallied rather than
// aborting the run: a missing empty directory never loses data.
func createMissingEmptyDirs(logger *runtime.Logger, localRoot, remoteRoot string, localDirs, remoteDirs map[string]struct{}) int {
	failures := 0
	for relDir := range localDirs {
		if _, ok := remoteDirs[relDir]; ok {
			continue
		}
		if err := os.MkdirAll(filepath.Join(remoteRoot, filepath.FromSlash(relDir)), 0o755); err != nil {
			failures++
			logger.Error("failed to create remote directory", map[string]any{"path": relDir, "error": err.Error()})
		}
	}
	for relDir := range remoteDirs {
		if _, ok := localDirs[relDir]; ok {
			continue
		}
		if err := os.MkdirAll(filepath.Join(localRoot, filepath.FromSlash(relDir)), 0o755); err != nil {
			failures++
			logger.Error("failed to create local directory", map[string]any{"path": relDir, "error": err.Error()})
		}
	}
	return failures
}

// cleanupEmptiedDirs removes directories, on both local and remote, that
// held one of this run's deleted files and are now empty on that side. A
// directory that was already empty before this run - and that this run
// didn't touch - is never considered, so pre-existing empty directories the
// user keeps intentionally are left alone.
func cleanupEmptiedDirs(localRoot, remoteRoot string, deletedRelPaths []string) {
	touched := make(map[string]struct{})
	for _, relPath := range deletedRelPaths {
		dir := path.Dir(relPath)
		if dir == "." {
			continue // never remove the sync root itself
		}
		touched[dir] = struct{}{}
	}
	for relDir := range touched {
		removeIfEmpty(filepath.Join(localRoot, filepath.FromSlash(relDir)))
		removeIfEmpty(filepath.Join(remoteRoot, filepath.FromSlash(relDir)))
	}
}

func removeIfEmpty(absDir string) {
	entries, err := os.ReadDir(absDir)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(absDir)
}
