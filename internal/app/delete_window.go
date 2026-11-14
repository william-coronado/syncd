package app

import (
	"sync"
	"time"
)

// DefaultDeleteWindow is how far back the watch daemon counts deletions it
// has already made when judging a new cycle against --max-delete.
const DefaultDeleteWindow = 10 * time.Minute

// DeleteWindow remembers the deletions recent cycles carried out, so the
// delete limit is judged against a rolling total rather than one cycle at a
// time. Without it, a mass delete that reaches the daemon as several
// fsnotify batches - each under the limit on its own - goes through piece
// by piece.
//
// A nil *DeleteWindow remembers nothing (each cycle is judged alone), which
// is what a one-shot `syncd sync` run wants.
type DeleteWindow struct {
	span time.Duration
	now  func() time.Time

	mu     sync.Mutex
	events []deleteEvent
}

type deleteEvent struct {
	at            time.Time
	local, remote int
}

// NewDeleteWindow returns a window covering the last span.
func NewDeleteWindow(span time.Duration) *DeleteWindow {
	return &DeleteWindow{span: span, now: time.Now}
}

// Span is the window's length (0 for a nil window).
func (w *DeleteWindow) Span() time.Duration {
	if w == nil {
		return 0
	}
	return w.span
}

// Recent returns how many files were deleted from each side within the
// window.
func (w *DeleteWindow) Recent() (local, remote int) {
	if w == nil {
		return 0, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := w.now().Add(-w.span)
	kept := w.events[:0]
	for _, e := range w.events {
		if e.at.After(cutoff) {
			kept = append(kept, e)
			local += e.local
			remote += e.remote
		}
	}
	w.events = kept
	return local, remote
}

// Add records deletions a cycle just carried out.
func (w *DeleteWindow) Add(local, remote int) {
	if w == nil || (local == 0 && remote == 0) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, deleteEvent{at: w.now(), local: local, remote: remote})
}

// Reset forgets everything - after an approved mass delete, which must not
// count against the ordinary deletions that follow it.
func (w *DeleteWindow) Reset() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = nil
}
