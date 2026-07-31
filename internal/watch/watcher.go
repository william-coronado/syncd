// Package watch provides an fsnotify-based recursive filesystem watcher
// that debounces and coalesces bursts of events into batches of changed,
// root-relative paths, for syncd's always-on watch mode.
package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"syncd/internal/fs"
)

// fsAdd is fsw.Add, indirected through a package-level variable purely so
// tests can simulate a per-directory Add failure (e.g. Linux's
// max_user_watches exhaustion) deterministically, without needing to
// actually exhaust OS-level watch resources.
var fsAdd = func(fsw *fsnotify.Watcher, path string) error {
	return fsw.Add(path)
}

// Watcher recursively watches root and emits coalesced batches of changed,
// root-relative, NFC-normalized paths. Events within `debounce` of each
// other are merged into one batch. matcher-ignored paths are dropped.
type Watcher struct {
	fsw      *fsnotify.Watcher
	root     string
	matcher  *fs.IgnoreMatcher
	debounce time.Duration

	batches   chan map[string]struct{}
	done      chan struct{}
	closeOnce sync.Once

	// addErrors records every fsw.Add failure hit while walking root in
	// NewWatcher. It is only ever written during that synchronous walk,
	// before w.run's goroutine starts, so it needs no lock: any later
	// caller of AddErrors is reading after that write has already
	// happened-before it (NewWatcher returning is the synchronization
	// point).
	addErrors []error
}

// NewWatcher creates a Watcher rooted at root. It walks root and adds every
// existing directory to the underlying fsnotify watcher (fsnotify is
// non-recursive, so newly created directories are added on the fly as
// events for them arrive). The returned Watcher must be closed with Close
// when no longer needed.
//
// A directory that fails to be added (e.g. Linux's max_user_watches
// exhausted partway through the walk) is skipped best-effort rather than
// failing the whole walk - the resulting partial coverage is degraded, not
// broken, and the hourly deep scan already backstops paths the watcher
// missed. Each such failure is recorded and available via AddErrors(); if
// literally zero directories could be watched, that IS a real failure and
// NewWatcher returns an error.
func NewWatcher(root string, matcher *fs.IgnoreMatcher, debounce time.Duration) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	// filepath.Abs (used to resolve --local before this is ever called)
	// does not resolve symlinks, so root may itself be a symlink to a
	// directory. filepath.WalkDir lstats its root argument and never
	// follows a symlink there, so d.IsDir() is false for the root entry and
	// the walk below visits nothing at all (not even an attempt to fsAdd
	// it) - resolving here makes watching a symlinked root actually work,
	// instead of silently watching zero directories.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		fsw.Close()
		return nil, fmt.Errorf("watcher: resolve root %s: %w", root, err)
	}
	if info, statErr := os.Stat(resolvedRoot); statErr != nil {
		fsw.Close()
		return nil, fmt.Errorf("watcher: stat root %s: %w", root, statErr)
	} else if !info.IsDir() {
		fsw.Close()
		return nil, fmt.Errorf("watcher: root %s is not a directory", root)
	}
	root = resolvedRoot

	var addErrors []error
	watched := 0

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root {
			rel, relErr := fs.NormalizeRelativePath(root, path)
			if relErr == nil && matcher.Match(rel, true) {
				// Don't watch ignored directory subtrees at all (e.g. a
				// pre-existing .git, node_modules, .venv): on Linux this
				// avoids burning inotify watch descriptors (and ENOSPC)
				// on large ignored trees, and per-event Match() already
				// keeps their contents out of batches anyway.
				return filepath.SkipDir
			}
		}
		if addErr := fsAdd(fsw, path); addErr != nil {
			addErrors = append(addErrors, fmt.Errorf("watch %s: %w", path, addErr))
			return nil // best-effort: skip this directory, keep walking
		}
		watched++
		return nil
	})
	if err != nil {
		fsw.Close()
		return nil, err
	}
	if watched == 0 {
		fsw.Close()
		// A previous reviewer argued indexing addErrors[0] here was safe
		// because "root is always attempted, so watched==0 implies >=1 add
		// error" - and was wrong (see TestNewWatcher_SymlinkedRootDoesNotPanic,
		// which panicked here with a symlinked root before the resolve+IsDir
		// check above was added). Even with that check in place, never index
		// addErrors without first confirming it's non-empty - the whole
		// point of this fix is that "reasoning" is exactly what got this
		// wrong the first time.
		if len(addErrors) == 0 {
			return nil, fmt.Errorf("watcher: no directories could be watched under %s", root)
		}
		return nil, fmt.Errorf("watcher: no directories could be watched under %s (%d add error(s), first: %v)", root, len(addErrors), addErrors[0])
	}

	w := &Watcher{
		fsw:       fsw,
		root:      root,
		matcher:   matcher,
		debounce:  debounce,
		batches:   make(chan map[string]struct{}),
		done:      make(chan struct{}),
		addErrors: addErrors,
	}

	go w.run()

	return w, nil
}

// AddErrors returns every fsw.Add failure hit while walking root in
// NewWatcher (nil if none). It reflects best-effort, degraded watch
// coverage - not a startup failure, since NewWatcher only errors outright
// when zero directories could be watched - so a caller (watch.Run) can log
// it once and let the periodic deep scan backstop the gap.
func (w *Watcher) AddErrors() []error {
	return w.addErrors
}

// Batches returns the receive-only channel of coalesced batches. Each value
// is a set of root-relative, NFC-normalized paths that changed since the
// previous batch.
func (w *Watcher) Batches() <-chan map[string]struct{} {
	return w.batches
}

// Close stops the watcher's goroutine, closes the underlying fsnotify
// watcher, and closes the batches channel. It is safe to call more than
// once, including concurrently from multiple goroutines.
func (w *Watcher) Close() error {
	var err error
	w.closeOnce.Do(func() {
		close(w.done)
		err = w.fsw.Close()
	})
	return err
}

// run is the single goroutine that owns all mutable state (pending set and
// debounce timer), so no locking is needed: fsnotify events, timer fires,
// and shutdown are all handled via select on this one goroutine, and it is
// the only goroutine that ever sends on w.batches or closes it.
func (w *Watcher) run() {
	defer close(w.batches)

	pending := make(map[string]struct{})
	timer := time.NewTimer(w.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	timerArmed := false

	for {
		select {
		case <-w.done:
			return

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handleEvent(ev, pending)
			if len(pending) > 0 {
				if timerArmed && !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(w.debounce)
				timerArmed = true
			}

		case _, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			// Errors from fsnotify are not fatal to the watch loop; drop
			// and continue watching.

		case <-timer.C:
			timerArmed = false
			if len(pending) == 0 {
				continue
			}
			batch := pending
			pending = make(map[string]struct{})
			select {
			case w.batches <- batch:
			case <-w.done:
				return
			}
		}
	}
}

// handleEvent computes the root-relative path for ev, drops it if the
// matcher ignores it, adds newly created directories to the underlying
// fsnotify watcher so nested files are seen, and otherwise accumulates the
// surviving relative path into pending.
func (w *Watcher) handleEvent(ev fsnotify.Event, pending map[string]struct{}) {
	rel, err := fs.NormalizeRelativePath(w.root, ev.Name)
	if err != nil {
		return
	}
	if rel == "" || rel == "." {
		return
	}

	isDir := false
	info, statErr := os.Lstat(ev.Name)
	if statErr == nil {
		isDir = info.IsDir()
	}
	// A removed path can't be lstat'd; best-effort treat as non-dir.
	// Lstat (not Stat) is deliberate: a symlink-to-directory must not be
	// treated as a real directory. os.Stat follows symlinks and would make
	// isDir=true for a symlink-to-dir, causing fsw.Add to watch the symlink
	// target (potentially outside the sync root) and the ignore-matcher to
	// receive isDir=true, both of which are incorrect for a symlink.
	// The scanner (ScanTree/ScanPaths) also uses Lstat and rejects symlinks
	// explicitly, so this keeps the two consistent.

	if w.matcher.Match(rel, isDir) {
		return
	}

	if isDir && ev.Op&fsnotify.Create != 0 {
		// Best-effort: if Add fails (e.g. the directory has already
		// vanished by the time we get here), just skip adding it; the
		// event itself is still recorded below.
		_ = w.fsw.Add(ev.Name)
	}

	pending[rel] = struct{}{}
}
