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

	rel := filepath.FromSlash(action.Path)
	conflictRel, err := findAvailableConflictInTrees(state.LocalTree, state.RemoteTree, action.Path, timeNow().UTC())
	if err != nil {
		return Result{Action: action, Error: err}
	}
	conflictName := filepath.FromSlash(conflictRel)
	preserve := func(held *fs.Tree, leaf string) error {
		return fs.CopyChecked(held, leaf, state.LocalTree, conflictName, false, nil, nil)
	}
	if err := fs.CopyChecked(state.RemoteTree, rel, state.LocalTree, rel, true, checkObserved(local), preserve); err != nil {
		return Result{Action: action, Error: fmt.Errorf("download remote canonical for conflict: %w", err)}
	}
	if err := fs.CopyChecked(state.LocalTree, conflictName, state.RemoteTree, conflictName, false, nil, nil); err != nil {
		return Result{Action: action, Error: fmt.Errorf("upload conflict copy to remote: %w", err)}
	}

	// Capture once so both manifest updates share the same LastSynced
	// timestamp - they represent one logical operation.
	now := time.Now().Unix()
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
					LastSynced: now,
				},
			},
			{
				Path: conflictRel,
				Row: db.ManifestRow{
					Path:       conflictRel,
					Size:       local.Size,
					Mtime:      local.Mtime,
					Hash:       local.Hash,
					LastSynced: now,
				},
			},
		},
	}
}

// findAvailableConflictPath picks a name not occupied on either side.
// The timestamp alone has only one-second resolution, so repeated conflicts
// need a suffix. The search is bounded to fail rather than spin forever.
// The pair lock serializes syncd processes; other writers can still race
// with this check, so the actual filesystem operations remain confined.
func findAvailableConflictPath(localRoot, remoteRoot, relPath string, ts time.Time) (string, error) {
	local, err := fs.OpenTree(localRoot)
	if err != nil {
		return "", err
	}
	defer local.Close()
	remote, err := fs.OpenTree(remoteRoot)
	if err != nil {
		return "", err
	}
	defer remote.Close()
	return findAvailableConflictInTrees(local, remote, relPath, ts)
}

func findAvailableConflictInTrees(local, remote *fs.Tree, relPath string, ts time.Time) (string, error) {
	for attempt := 0; attempt < maxConflictPathAttempts; attempt++ {
		candidate := conflictPathAttempt(relPath, ts, attempt)
		_, lerr := local.Stat(filepath.FromSlash(candidate))
		_, rerr := remote.Stat(filepath.FromSlash(candidate))
		if lerr != nil && !os.IsNotExist(lerr) {
			return "", lerr
		}
		if rerr != nil && !os.IsNotExist(rerr) {
			return "", rerr
		}
		if os.IsNotExist(lerr) && os.IsNotExist(rerr) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no available conflict filename for %s after %d attempts", relPath, maxConflictPathAttempts)
}

const maxConflictPathAttempts = 1000

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

// trimComponentBytes shrinks component to at most maxBytes, giving up base
// first, then ext, then (only in the pathological case suffix alone doesn't
// fit) suffix itself - in that priority order, since suffix is the conflict
// marker that has to survive for the file to be identifiable as a conflict
// copy at all, ext is a nicety, and base (the original name) is the most
// expendable. Every trim happens at a UTF-8 rune boundary. Unlike the
// earlier version, this never floors the base budget at 1 byte and always
// appends the full ext regardless of size - that could let the result
// exceed maxBytes when suffix+ext alone were already over budget (review
// finding, Gemini 3.6); this version guarantees len(result) <= maxBytes
// whenever maxBytes >= 0.
func trimComponentBytes(component string, maxBytes int, suffix, ext string) string {
	if len(component) <= maxBytes {
		return component
	}
	base := strings.TrimSuffix(component, ext)
	base = strings.TrimSuffix(base, suffix)

	if len(suffix) > maxBytes {
		return trimToRuneBoundary(suffix, maxBytes)
	}
	if len(suffix)+len(ext) > maxBytes {
		ext = trimToRuneBoundary(ext, maxBytes-len(suffix))
	}
	allowed := maxBytes - len(suffix) - len(ext)
	if allowed < 0 {
		allowed = 0
	}
	base = trimToRuneBoundary(base, allowed)
	return base + suffix + ext
}

// trimToRuneBoundary returns the longest prefix of s that is at most
// maxBytes bytes long and ends on a valid UTF-8 rune boundary.
func trimToRuneBoundary(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	for len(s) > maxBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		if size <= 0 {
			break
		}
		s = s[:len(s)-size]
	}
	return s
}
