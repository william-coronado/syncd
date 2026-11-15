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
	LocalRoot             string
	RemoteRoot            string
	LocalTree, RemoteTree *fs.Tree
	LocalFiles            map[string]fs.FileEntry
	RemoteFiles           map[string]fs.FileEntry
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
	if len(actions) > 0 && state.LocalTree == nil {
		local, err := fs.OpenTree(state.LocalRoot)
		if err != nil {
			return nil, err
		}
		defer local.Close()
		remote, err := fs.OpenTree(state.RemoteRoot)
		if err != nil {
			return nil, err
		}
		defer remote.Close()
		state.LocalTree, state.RemoteTree = local, remote
	}

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

// checkObserved validates the entry after it has been detached from the
// destination name, so a concurrent pathname replacement cannot pass the
// check and then be removed in its place.
func checkObserved(scanned fs.FileEntry) fs.EntryCheck {
	return func(tree *fs.Tree, rel string) error {
		info, err := tree.Lstat(rel)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrDestinationChanged, err)
		}
		if !info.Mode().IsRegular() || (scanned.Info != nil && !fs.SameEntry(scanned.Info, info)) ||
			info.Size() != scanned.Size || (!scanned.ModTime.IsZero() && !info.ModTime().Equal(scanned.ModTime)) ||
			(scanned.Mtime != 0 && info.ModTime().Unix() != scanned.Mtime) {
			return ErrDestinationChanged
		}
		if scanned.Hash != "" {
			hash, err := tree.Hash(rel)
			if err != nil {
				return err
			}
			if hash != scanned.Hash {
				return ErrDestinationChanged
			}
		}
		return nil
	}
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
	rel := filepath.FromSlash(action.Path)
	scanned, present := state.RemoteFiles[action.Path]
	var preserve func(*fs.Tree, string) error
	if action.PreserveDest {
		preserve = func(held *fs.Tree, leaf string) error {
			return state.Trash.PreserveFrom(held, leaf, state.RemoteRoot, action.Path)
		}
	}
	if err := fs.CopyChecked(state.LocalTree, relativeSource(state.LocalTree.Name(), local), state.RemoteTree, rel, present, checkObserved(scanned), preserve); err != nil {
		if os.IsExist(err) || os.IsNotExist(err) {
			err = fmt.Errorf("%w: %v", ErrDestinationChanged, err)
		}
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
	rel := filepath.FromSlash(action.Path)
	scanned, present := state.LocalFiles[action.Path]
	var preserve func(*fs.Tree, string) error
	if action.PreserveDest {
		preserve = func(held *fs.Tree, leaf string) error {
			return state.Trash.PreserveFrom(held, leaf, state.LocalRoot, action.Path)
		}
	}
	if err := fs.CopyChecked(state.RemoteTree, relativeSource(state.RemoteTree.Name(), remote), state.LocalTree, rel, present, checkObserved(scanned), preserve); err != nil {
		if os.IsExist(err) || os.IsNotExist(err) {
			err = fmt.Errorf("%w: %v", ErrDestinationChanged, err)
		}
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
	targetRoot, targetFiles := state.RemoteTree, state.RemoteFiles
	if deleteLocal {
		targetRoot, targetFiles = state.LocalTree, state.LocalFiles
	}
	// BuildPlan only deletes a path it saw, unchanged since the manifest, on
	// the target side - so the target must still be exactly that file. If it
	// was edited in the meantime, deleting it would destroy the edit.
	scanned, present := targetFiles[action.Path]
	if !present {
		return Result{Action: action, Error: ErrDestinationChanged}
	}
	if err := state.Trash.RemoveFrom(targetRoot, action.Path, checkObserved(scanned)); err != nil {
		return Result{Action: action, Error: fmt.Errorf("delete %s: %w", action.Path, err)}
	}

	return Result{
		Action: action,
		Updates: []ManifestUpdate{
			{Path: action.Path, Delete: true},
		},
	}
}

func relativeSource(root string, entry fs.FileEntry) string {
	rel, err := filepath.Rel(root, entry.AbsPath)
	if err != nil {
		return entry.AbsPath
	}
	return rel
}
