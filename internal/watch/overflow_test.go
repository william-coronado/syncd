package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"syncd/internal/app"
	"syncd/internal/db"
	"syncd/internal/fs"
	"syncd/internal/runtime"
)

// fsnotify errors used to be dropped on the floor; they must now reach
// Errors().
func TestWatcher_ForwardsFsnotifyErrors(t *testing.T) {
	root := t.TempDir()
	m, _ := fs.LoadIgnoreMatcher(root)
	w, err := NewWatcher(root, m, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.fsw.Errors <- fsnotify.ErrEventOverflow
	select {
	case got := <-w.Errors():
		if got != fsnotify.ErrEventOverflow {
			t.Fatalf("got %v, want ErrEventOverflow", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fsnotify error was not forwarded to Errors()")
	}
}

// A tree that never goes quiet must still flush a batch within maxLatency,
// instead of the debounce timer being reset forever.
func TestWatcher_SustainedActivityStillFlushesWithinMaxLatency(t *testing.T) {
	root := t.TempDir()
	m, _ := fs.LoadIgnoreMatcher(root)
	debounce := 60 * time.Millisecond // maxLatency = 600ms
	w, err := NewWatcher(root, m, debounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		// Event every 15ms: always well inside the 60ms debounce window.
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(15 * time.Millisecond):
				_ = os.WriteFile(filepath.Join(root, "busy.txt"), []byte{byte(i)}, 0o644)
			}
		}
	}()

	select {
	case batch := <-w.Batches():
		if _, ok := batch["busy.txt"]; !ok {
			t.Fatalf("unexpected batch %v", batch)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no batch flushed during sustained activity: debounce has no latency cap")
	}
}

// An overflow means the kernel dropped events, so Run must reconcile the
// full tree right away. Here a local file exists that the watcher never
// reported (written before Run started); with hour-long poll/deep-scan
// tickers, only the overflow-triggered rescan can sync it.
func TestRun_OverflowTriggersFullRescan(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, ".synclock"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "missed.txt"), []byte("missed"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbh, err := db.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	m, _ := fs.LoadIgnoreMatcher(local)
	p := app.RobustParams{LocalRoot: local, RemoteRoot: remote, Workers: 1, Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger}

	created := make(chan *Watcher, 1)
	orig := newWatcher
	newWatcher = func(root string, matcher *fs.IgnoreMatcher, debounce time.Duration) (*Watcher, error) {
		w, err := orig(root, matcher, debounce)
		if err == nil {
			created <- w
		}
		return w, err
	}
	t.Cleanup(func() { newWatcher = orig })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, p, m, LoopConfig{RemotePoll: time.Hour, DeepScan: time.Hour, Debounce: 20 * time.Millisecond})
	}()
	defer func() {
		cancel()
		<-done
	}()

	var w *Watcher
	select {
	case w = <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not create a watcher")
	}
	w.fsw.Errors <- fsnotify.ErrEventOverflow

	deadline := time.After(3 * time.Second)
	for {
		if b, _ := os.ReadFile(filepath.Join(remote, "missed.txt")); string(b) == "missed" {
			return
		}
		select {
		case <-deadline:
			t.Fatal("overflow did not trigger a full rescan")
		case <-time.After(25 * time.Millisecond):
		}
	}
}
