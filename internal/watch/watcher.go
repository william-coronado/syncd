// Package watch provides an fsnotify-based recursive filesystem watcher
// that debounces and coalesces bursts of events into batches of changed,
// root-relative paths, for syncd's always-on watch mode.
package watch

import (
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"syncd/internal/fs"
)

// Watcher recursively watches root and emits coalesced batches of changed,
// root-relative, NFC-normalized paths. Events within `debounce` of each
// other are merged into one batch. matcher-ignored paths are dropped.
type Watcher struct {
	fsw      *fsnotify.Watcher
	root     string
	matcher  *fs.IgnoreMatcher
	debounce time.Duration

	batches chan map[string]struct{}
	done    chan struct{}
}

// NewWatcher creates a Watcher rooted at root. It walks root and adds every
// existing directory to the underlying fsnotify watcher (fsnotify is
// non-recursive, so newly created directories are added on the fly as
// events for them arrive). The returned Watcher must be closed with Close
// when no longer needed.
func NewWatcher(root string, matcher *fs.IgnoreMatcher, debounce time.Duration) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if addErr := fsw.Add(path); addErr != nil {
				return addErr
			}
		}
		return nil
	})
	if err != nil {
		fsw.Close()
		return nil, err
	}

	w := &Watcher{
		fsw:      fsw,
		root:     root,
		matcher:  matcher,
		debounce: debounce,
		batches:  make(chan map[string]struct{}),
		done:     make(chan struct{}),
	}

	go w.run()

	return w, nil
}

// Batches returns the receive-only channel of coalesced batches. Each value
// is a set of root-relative, NFC-normalized paths that changed since the
// previous batch.
func (w *Watcher) Batches() <-chan map[string]struct{} {
	return w.batches
}

// Close stops the watcher's goroutine, closes the underlying fsnotify
// watcher, and closes the batches channel. It is safe to call more than
// once.
func (w *Watcher) Close() error {
	select {
	case <-w.done:
		// already closed
		return nil
	default:
		close(w.done)
	}
	return w.fsw.Close()
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
	info, statErr := os.Stat(ev.Name)
	if statErr == nil {
		isDir = info.IsDir()
	}
	// A removed path can't be stat'd; best-effort treat as non-dir.

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
