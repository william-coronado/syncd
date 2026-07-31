package execute

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"syncd/internal/db"
	"syncd/internal/fs"
	"syncd/internal/reconcile"
)

// timeNow is time.Now, indirected through a package-level variable so tests
// can force two conflict resolutions onto the exact same timestamp -
// deterministically reproducing conflictPath's 1-second collision window
// without relying on both calls landing in the same wall-clock second by
// chance. Same pattern as watch/loop.go's newTicker.
var timeNow = time.Now

func runConflict(state SyncState, action reconcile.Action) Result {
	local, localOK := state.LocalFiles[action.Path]
	remote, remoteOK := state.RemoteFiles[action.Path]
	if !localOK || !remoteOK {
		return Result{Action: action, Error: fmt.Errorf("conflict requires local and remote versions: %s", action.Path)}
	}

	localCanonicalPath := filepath.Join(state.LocalRoot, filepath.FromSlash(action.Path))
	remoteCanonicalPath := filepath.Join(state.RemoteRoot, filepath.FromSlash(action.Path))

	conflictRel := findAvailableConflictPath(state.LocalRoot, state.RemoteRoot, action.Path, timeNow().UTC())
	localConflictPath := filepath.Join(state.LocalRoot, filepath.FromSlash(conflictRel))
	remoteConflictPath := filepath.Join(state.RemoteRoot, filepath.FromSlash(conflictRel))

	if err := fs.EnsureParents(localConflictPath); err != nil {
		return Result{Action: action, Error: fmt.Errorf("prepare local conflict parent: %w", err)}
	}
	if err := renameFile(localCanonicalPath, localConflictPath); err != nil {
		return Result{Action: action, Error: fmt.Errorf("rename local conflict file: %w", err)}
	}
	if err := fs.AtomicCopyFile(remoteCanonicalPath, localCanonicalPath); err != nil {
		return Result{Action: action, Error: fmt.Errorf("download remote canonical after conflict rename: %w", err)}
	}
	if err := fs.AtomicCopyFile(localConflictPath, remoteConflictPath); err != nil {
		return Result{Action: action, Error: fmt.Errorf("upload conflict copy to remote: %w", err)}
	}

	return Result{
		Action: action,
		Updates: []ManifestUpdate{
			{
				Path: action.Path,
				Row: db.ManifestRow{
					Path:       action.Path,
					Size:       remote.Size,
					Mtime:      remote.Mtime,
					Hash:       remote.Hash,
					LastSynced: time.Now().Unix(),
				},
			},
			{
				Path: conflictRel,
				Row: db.ManifestRow{
					Path:       conflictRel,
					Size:       local.Size,
					Mtime:      local.Mtime,
					Hash:       local.Hash,
					LastSynced: time.Now().Unix(),
				},
			},
		},
	}
}

func renameFile(src, dst string) error {
	return os.Rename(src, dst)
}

// findAvailableConflictPath returns conflictPath(relPath, ts), or - if that
// name is already occupied on either side - a disambiguated variant that
// exists on neither, by trying successive attempt numbers. conflictPath's
// timestamp has only 1-second resolution, and renameFile is a bare
// os.Rename, which silently REPLACES an existing destination on Unix - so
// without this, two conflicts on the same relPath within the same
// wall-clock second would have the second one clobber the first call's
// already-renamed-aside conflict copy (review finding "Medium 6"). A single
// process handles one RunActions batch (one action per path) at a time and
// watch/loop.go serializes cycles through a mutex, so the check-then-rename
// here has no concurrent-writer race to guard against in practice.
func findAvailableConflictPath(localRoot, remoteRoot, relPath string, ts time.Time) string {
	for attempt := 0; ; attempt++ {
		candidate := conflictPathAttempt(relPath, ts, attempt)
		localP := filepath.Join(localRoot, filepath.FromSlash(candidate))
		remoteP := filepath.Join(remoteRoot, filepath.FromSlash(candidate))
		if !pathExists(localP) && !pathExists(remoteP) {
			return candidate
		}
	}
}

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func conflictPath(relPath string, ts time.Time) string {
	return conflictPathAttempt(relPath, ts, 0)
}

// conflictPathAttempt is conflictPath, plus a disambiguating "-N" appended
// to the suffix when attempt > 0 (see findAvailableConflictPath).
func conflictPathAttempt(relPath string, ts time.Time, attempt int) string {
	dir := filepath.Dir(relPath)
	base := filepath.Base(relPath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	suffix := "_conflict_" + ts.Format("20060102_150405")
	if attempt > 0 {
		suffix += fmt.Sprintf("-%d", attempt+1)
	}
	newName := name + suffix + ext
	newName = trimComponentBytes(newName, 255, suffix, ext)
	if dir == "." {
		return filepath.ToSlash(newName)
	}
	return filepath.ToSlash(filepath.Join(dir, newName))
}

func trimComponentBytes(component string, maxBytes int, suffix, ext string) string {
	if len(component) <= maxBytes {
		return component
	}
	base := strings.TrimSuffix(component, ext)
	base = strings.TrimSuffix(base, suffix)
	allowed := maxBytes - len(suffix) - len(ext)
	if allowed < 1 {
		allowed = 1
	}
	for len(base) > allowed {
		_, size := utf8.DecodeLastRuneInString(base)
		if size <= 0 {
			break
		}
		base = base[:len(base)-size]
	}
	return base + suffix + ext
}
