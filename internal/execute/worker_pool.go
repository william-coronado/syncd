package execute

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"drivesync/internal/db"
	"drivesync/internal/fs"
	"drivesync/internal/reconcile"
)

type SyncState struct {
	LocalRoot   string
	RemoteRoot  string
	LocalFiles  map[string]fs.FileEntry
	RemoteFiles map[string]fs.FileEntry
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
	for res := range results {
		out = append(out, res)
	}
	// Report cancellation to the caller even though we still return whatever
	// partial results completed - the caller decides what to do with them
	// (e.g. still commit manifest updates for actions that finished).
	return out, ctx.Err()
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
	targetRoot := state.RemoteRoot
	if deleteLocal {
		targetRoot = state.LocalRoot
	}
	targetPath := filepath.Join(targetRoot, filepath.FromSlash(action.Path))
	if err := removeIfExists(targetPath); err != nil {
		return Result{Action: action, Error: fmt.Errorf("delete %s: %w", action.Path, err)}
	}
	return Result{
		Action: action,
		Updates: []ManifestUpdate{
			{Path: action.Path, Delete: true},
		},
	}
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
