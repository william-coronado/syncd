package watch

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"syncd/internal/fs"
)

// A directory that arrives already populated (moved in, mkdir -p, unzipped,
// cloned) produces one Create event for its top level only. Its
// subdirectories must be watched too, or later changes inside them wait for
// the next remote poll.
func TestHandleEvent_NewDirectoryWatchesWholeSubtree(t *testing.T) {
	root := t.TempDir()
	m, _ := fs.LoadIgnoreMatcher(root)
	w, err := NewWatcher(root, m, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Built outside and renamed in, so the tree arrives complete: its
	// subdirectories never produce events of their own.
	outside := t.TempDir()
	for _, d := range []string{"a/b/c", "a/d", ".venv/lib"} { // .venv is ignored by default
		if err := os.MkdirAll(filepath.Join(outside, "proj", filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	proj := filepath.Join(w.root, "proj")
	if err := os.Rename(filepath.Join(outside, "proj"), proj); err != nil {
		t.Fatal(err)
	}
	w.handleEvent(fsnotify.Event{Name: proj, Op: fsnotify.Create}, map[string]struct{}{})

	watched := w.fsw.WatchList()
	for _, d := range []string{"", "a", "a/b", "a/b/c", "a/d"} {
		want := filepath.Join(proj, filepath.FromSlash(d))
		if !slices.Contains(watched, want) {
			t.Errorf("%s not watched", want)
		}
	}
	for _, p := range watched {
		if rel, _ := filepath.Rel(proj, p); rel == ".venv" || filepath.Dir(rel) == ".venv" {
			t.Errorf("ignored directory %s was watched", p)
		}
	}
}

func TestWatcher_ReportsChangeInsideMovedInDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	m, _ := fs.LoadIgnoreMatcher(root)
	w, err := NewWatcher(root, m, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := os.MkdirAll(filepath.Join(outside, "proj", "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(outside, "proj"), filepath.Join(root, "proj")); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, w, "proj")

	if err := os.WriteFile(filepath.Join(root, "proj", "a", "b", "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, w, "proj/a/b/new.txt")
}

func waitForPath(t *testing.T, w *Watcher, rel string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case batch := <-w.Batches():
			if _, ok := batch[rel]; ok {
				return
			}
		case <-deadline:
			t.Fatalf("no batch reported %s", rel)
		}
	}
}

// A subdirectory that cannot be watched (inotify's max_user_watches, say)
// is reported on Errors(), which watch.Run logs, rather than dropped.
//
// The Watcher here has no run goroutine, so swapping the fsAdd seam cannot
// race with event handling.
func TestHandleEvent_ReportsSubtreeWatchFailure(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "proj", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer fsw.Close()
	m, _ := fs.LoadIgnoreMatcher(root)
	w := &Watcher{fsw: fsw, root: root, matcher: m, errs: make(chan error, errorBuffer)}

	orig := fsAdd
	t.Cleanup(func() { fsAdd = orig })
	fsAdd = func(fsw *fsnotify.Watcher, path string) error {
		if filepath.Base(path) == "deep" {
			return errors.New("no space left on device")
		}
		return fsw.Add(path)
	}

	w.handleEvent(fsnotify.Event{Name: filepath.Join(root, "proj"), Op: fsnotify.Create}, map[string]struct{}{})

	select {
	case err := <-w.Errors():
		if !strings.Contains(err.Error(), "deep") {
			t.Fatalf("error does not name the directory: %v", err)
		}
	default:
		t.Fatal("subtree watch failure was not reported")
	}
}
