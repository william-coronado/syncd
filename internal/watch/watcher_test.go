package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"syncd/internal/fs"
)

func TestWatcherCoalescesEventsIntoBatch(t *testing.T) {
	root := t.TempDir()
	m, _ := fs.LoadIgnoreMatcher(root)
	w, err := NewWatcher(root, m, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	os.WriteFile(filepath.Join(root, "a.txt"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("2"), 0o644)

	select {
	case batch := <-w.Batches():
		if _, ok := batch["a.txt"]; !ok {
			t.Error("missing a.txt in batch")
		}
		if _, ok := batch["b.txt"]; !ok {
			t.Error("missing b.txt in batch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no batch emitted")
	}
}

func TestWatcherDropsIgnoredPaths(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".syncdignore"), []byte("*.tmp\n"), 0o644)
	m, _ := fs.LoadIgnoreMatcher(root)
	w, _ := NewWatcher(root, m, 80*time.Millisecond)
	defer w.Close()
	os.WriteFile(filepath.Join(root, "x.tmp"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "keep.txt"), []byte("k"), 0o644)
	batch := <-w.Batches()
	if _, ok := batch["x.tmp"]; ok {
		t.Error("ignored *.tmp leaked into batch")
	}
	if _, ok := batch["keep.txt"]; !ok {
		t.Error("keep.txt missing")
	}
}

// TestWatcherConcurrentCloseNoPanic is a regression test: Close() must be
// safe to call from multiple goroutines at once (e.g. a signal handler
// racing the watch loop's own teardown), not just sequentially.
func TestWatcherConcurrentCloseNoPanic(t *testing.T) {
	root := t.TempDir()
	m, _ := fs.LoadIgnoreMatcher(root)
	w, err := NewWatcher(root, m, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = w.Close()
		}(i)
	}
	wg.Wait() // no panic reaching here is the main assertion

	for i, err := range errs {
		if err != nil {
			t.Logf("goroutine %d Close() returned benign error: %v", i, err)
		}
	}

	select {
	case _, ok := <-w.Batches():
		if ok {
			t.Error("expected closed batches channel after concurrent Close")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("batches channel never closed after concurrent Close")
	}
}

// TestNewWatcherDegradesOnPartialAddFailure is a regression test: NewWatcher
// used to return the first fsw.Add error straight out of WalkDir, so on
// Linux, exhausting max_user_watches partway through the walk meant the
// watcher didn't start at ALL - rather than degrading to partial coverage
// (which the hourly deep scan already backstops). A per-directory Add
// failure must be best-effort: skip that directory, keep walking, and
// record the error so the caller can observe it via AddErrors().
//
// fsAdd is swapped for a fake so this is deterministic and cross-platform,
// rather than relying on actually exhausting OS-level watch resources.
func TestNewWatcherDegradesOnPartialAddFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "good"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bad"), 0o755); err != nil {
		t.Fatal(err)
	}

	orig := fsAdd
	fsAdd = func(fsw *fsnotify.Watcher, path string) error {
		if filepath.Base(path) == "bad" {
			return fmt.Errorf("simulated add failure")
		}
		return orig(fsw, path)
	}
	defer func() { fsAdd = orig }()

	m, _ := fs.LoadIgnoreMatcher(root)
	w, err := NewWatcher(root, m, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("expected NewWatcher to degrade gracefully on a partial Add failure, got error: %v", err)
	}
	defer w.Close()

	errs := w.AddErrors()
	if len(errs) != 1 {
		t.Fatalf("expected 1 recorded add error, got %d: %v", len(errs), errs)
	}
}

// TestNewWatcherErrorsWhenNoDirectoryCouldBeWatched checks the other half
// of the same fix: degrading to partial coverage is fine, but if literally
// zero directories could be watched, that's a real failure (not
// degradation) and NewWatcher must still return an error.
func TestNewWatcherErrorsWhenNoDirectoryCouldBeWatched(t *testing.T) {
	root := t.TempDir()

	orig := fsAdd
	fsAdd = func(fsw *fsnotify.Watcher, path string) error {
		return fmt.Errorf("simulated add failure")
	}
	defer func() { fsAdd = orig }()

	m, _ := fs.LoadIgnoreMatcher(root)
	_, err := NewWatcher(root, m, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected NewWatcher to error when zero directories could be watched")
	}
}

// TestWatcherSkipsIgnoredDirSubtree is a light check that a pre-existing
// ignored directory (matched as dirOnly, e.g. "node_modules/") is never
// added to the underlying fsnotify watcher: NewWatcher must not error on
// such a tree, and a file written inside the ignored subtree afterward
// must never surface in a batch (since fsnotify was never told to watch
// it), even though a concurrent, non-ignored write does produce a batch.
func TestWatcherSkipsIgnoredDirSubtree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "node_modules", "pkg", "existing.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, ".syncdignore"), []byte("node_modules/\n"), 0o644)

	m, _ := fs.LoadIgnoreMatcher(root)
	w, err := NewWatcher(root, m, 80*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	os.WriteFile(filepath.Join(root, "node_modules", "pkg", "new.js"), []byte("y"), 0o644)
	os.WriteFile(filepath.Join(root, "keep.txt"), []byte("k"), 0o644)

	select {
	case batch := <-w.Batches():
		if _, ok := batch["node_modules/pkg/new.js"]; ok {
			t.Error("ignored dir subtree leaked into batch")
		}
		if _, ok := batch["keep.txt"]; !ok {
			t.Error("keep.txt missing")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no batch emitted")
	}
}
