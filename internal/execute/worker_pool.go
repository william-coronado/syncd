package execute

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"syncd/internal/db"
	"syncd/internal/fs"
	"syncd/internal/reconcile"
)

type SyncState struct {
	LocalRoot   string
	RemoteRoot  string
	LocalFiles  map[string]fs.FileEntry
	RemoteFiles map[string]fs.FileEntry
	// Trash, if set, receives deleted files that live on a local disk
	// instead of their being removed outright (see fs.Trash). nil removes.
	Trash *fs.Trash
}

type ManifestUpdate struct {
	Path   string
	Delete bool
	Row    db.ManifestRow
}

type Result struct {
	Action  reconcile.Action
	Error   error
	Updates []ManifestUpdate
}

func RunActions(ctx context.Context, workers int, state SyncState, actions []reconcile.Action) ([]Result, error) {
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan reconcile.Action)
	results := make(chan Result, len(actions))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for action := range jobs {
				if err := ctx.Err(); err != nil {
					results <- Result{Action: action, Error: err}
					continue
				}
				results <- applyAction(state, action)
			}
		}()
	}

	// Feeding and draining run as independent goroutines so that an early
	// return here (on cancellation) still always closes jobs, and workers
	// finishing still always closes results - neither side can leave the
	// other blocked forever.
	go func() {
		defer close(jobs)
		for _, action := range actions {
			select {
			case <-ctx.Done():
				return
			case jobs <- action:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([]Result, 0, len(actions))
	cancelled := false
	for res := range results {
		if res.Error != nil && ctx.Err() != nil && errors.Is(res.Error, ctx.Err()) {
			cancelled = true
		}
		out = append(out, res)
	}
	// Report cancellation to the caller even though we still return whatever
	// partial results completed - the caller decides what to do with them
	// (e.g. still commit manifest updates for actions that finished). But
	// only when cancellation actually cut the batch short: a SIGINT landing
	// after the last action already finished must not make a fully
	// successful run report "did not complete" (docs/follow-ups.md #16).
	if cancelled || len(out) < len(actions) {
		return out, ctx.Err()
	}
	return out, nil
}

// ErrDestinationChanged reports that an action's destination no longer
// matches what this cycle's scan saw (it was created, modified, replaced,
// or - for an overwrite - removed in between). The action is skipped rather
// than applied, with no manifest update, so the next cycle re-plans it from
// fresh state. Without this guard, a slow full-tree cycle over SMB leaves a
// wide window in which a user's (or another machine's) write to the
// destination is silently overwritten by a copy, or destroyed by a delete,
// planned against a now-stale scan.
var ErrDestinationChanged = errors.New("destination changed since scan; deferring to next cycle")

// verifyDestinationUnchanged checks absPath against the scan's view of it:
// scanned/present is the entry (if any) the scan recorded for that path on
// the destination side. A path the scan saw absent must still be absent; a
// path the scan saw must still be a regular file with the same size and
// mtime. allowVanished tolerates a scanned path that has since disappeared
// (correct for deletes, whose goal state is "absent" anyway).
func verifyDestinationUnchanged(absPath string, scanned fs.FileEntry, present, allowVanished bool) error {
	info, err := os.Lstat(absPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat destination: %w", err)
		}
		if !present || allowVanished {
			return nil
		}
		return fmt.Errorf("%w (scanned present, now missing)", ErrDestinationChanged)
	}
	if !present {
		return fmt.Errorf("%w (scanned absent, now exists)", ErrDestinationChanged)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w (no longer a regular file)", ErrDestinationChanged)
	}
	if info.Size() != scanned.Size || info.ModTime().Unix() != scanned.Mtime {
		return fmt.Errorf("%w (size/mtime %d/%d, scanned %d/%d)", ErrDestinationChanged, info.Size(), info.ModTime().Unix(), scanned.Size, scanned.Mtime)
	}
	return nil
}

func applyAction(state SyncState, action reconcile.Action) Result {
	switch action.Type {
	case reconcile.ActionNoOp:
		return Result{Action: action}
	case reconcile.ActionUpload:
		return runUpload(state, action)
	case reconcile.ActionDownload:
		return runDownload(state, action)
	case reconcile.ActionDeleteRemote:
		return runDelete(state, action, false)
	case reconcile.ActionDeleteLocal:
		return runDelete(state, action, true)
	case reconcile.ActionConflict:
		return runConflict(state, action)
	default:
		return Result{
			Action: action,
			Error:  fmt.Errorf("unknown action type %s", action.Type),
		}
	}
}

func runUpload(state SyncState, action reconcile.Action) Result {
	local, ok := state.LocalFiles[action.Path]
	if !ok {
		return Result{Action: action, Error: fmt.Errorf("upload source missing: %s", action.Path)}
	}
	dst := filepath.Join(state.RemoteRoot, filepath.FromSlash(action.Path))
	scanned, present := state.RemoteFiles[action.Path]
	if err := verifyDestinationUnchanged(dst, scanned, present, false); err != nil {
		return Result{Action: action, Error: fmt.Errorf("upload %s: %w", action.Path, err)}
	}
	if err := fs.AtomicCopyFile(local.AbsPath, dst); err != nil {
		return Result{Action: action, Error: fmt.Errorf("upload %s: %w", action.Path, err)}
	}
	return Result{
		Action: action,
		Updates: []ManifestUpdate{
			{Path: action.Path, Row: db.ManifestRow{
				Path: action.Path, Size: local.Size, Mtime: local.Mtime, Hash: local.Hash, LastSynced: time.Now().Unix(),
			}},
		},
	}
}

func runDownload(state SyncState, action reconcile.Action) Result {
	remote, ok := state.RemoteFiles[action.Path]
	if !ok {
		return Result{Action: action, Error: fmt.Errorf("download source missing: %s", action.Path)}
	}
	dst := filepath.Join(state.LocalRoot, filepath.FromSlash(action.Path))
	scanned, present := state.LocalFiles[action.Path]
	if err := verifyDestinationUnchanged(dst, scanned, present, false); err != nil {
		return Result{Action: action, Error: fmt.Errorf("download %s: %w", action.Path, err)}
	}
	if err := fs.AtomicCopyFile(remote.AbsPath, dst); err != nil {
		return Result{Action: action, Error: fmt.Errorf("download %s: %w", action.Path, err)}
	}
	return Result{
		Action: action,
		Updates: []ManifestUpdate{
			{Path: action.Path, Row: db.ManifestRow{
				Path: action.Path, Size: remote.Size, Mtime: remote.Mtime, Hash: remote.Hash, LastSynced: time.Now().Unix(),
			}},
		},
	}
}

func runDelete(state SyncState, action reconcile.Action, deleteLocal bool) Result {
	targetRoot, targetFiles := state.RemoteRoot, state.RemoteFiles
	if deleteLocal {
		targetRoot, targetFiles = state.LocalRoot, state.LocalFiles
	}
	targetPath := filepath.Join(targetRoot, filepath.FromSlash(action.Path))
	// BuildPlan only deletes a path it saw, unchanged since the manifest, on
	// the target side - so the target must still be exactly that file. If it
	// was edited in the meantime, deleting it would destroy the edit.
	scanned, present := targetFiles[action.Path]
	if err := verifyDestinationUnchanged(targetPath, scanned, present, true); err != nil {
		return Result{Action: action, Error: fmt.Errorf("delete %s: %w", action.Path, err)}
	}
	if err := state.Trash.Remove(targetRoot, action.Path); err != nil {
		return Result{Action: action, Error: fmt.Errorf("delete %s: %w", action.Path, err)}
	}
	return Result{
		Action: action,
		Updates: []ManifestUpdate{
			{Path: action.Path, Delete: true},
		},
	}
}
