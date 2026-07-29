package execute

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"drivesync/internal/db"
	"drivesync/internal/fs"
	"drivesync/internal/reconcile"
)

func runConflict(state SyncState, action reconcile.Action) Result {
	local, localOK := state.LocalFiles[action.Path]
	remote, remoteOK := state.RemoteFiles[action.Path]
	if !localOK || !remoteOK {
		return Result{Action: action, Error: fmt.Errorf("conflict requires local and remote versions: %s", action.Path)}
	}

	localCanonicalPath := filepath.Join(state.LocalRoot, filepath.FromSlash(action.Path))
	remoteCanonicalPath := filepath.Join(state.RemoteRoot, filepath.FromSlash(action.Path))

	conflictRel := conflictPath(action.Path, time.Now().UTC())
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

func conflictPath(relPath string, ts time.Time) string {
	dir := filepath.Dir(relPath)
	base := filepath.Base(relPath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	suffix := "_conflict_" + ts.Format("20060102_150405")
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
