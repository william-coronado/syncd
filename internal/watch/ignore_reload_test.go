package watch

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"syncd/internal/app"
	"syncd/internal/db"
	"syncd/internal/fs"
	"syncd/internal/runtime"
)

// Editing .syncdignore mid-run must change what the watcher watches, not
// just what scans include: a directory un-ignored while watch runs must
// then sync its new files through events, with the poll an hour away
// (docs/follow-ups.md #28 - previously it was never watched at all).
func TestRun_SyncdignoreEditRebuildsWatcher(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, ".synclock"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ignorePath := filepath.Join(local, fs.IgnoreFileName)
	if err := os.WriteFile(ignorePath, []byte("skip/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(local, "skip"), 0o755); err != nil {
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

	var built atomic.Int32
	orig := newWatcher
	newWatcher = func(root string, matcher *fs.IgnoreMatcher, debounce time.Duration) (*Watcher, error) {
		w, err := orig(root, matcher, debounce)
		if err == nil {
			built.Add(1)
		}
		return w, err
	}
	t.Cleanup(func() { newWatcher = orig })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, p, m, LoopConfig{RemotePoll: time.Hour, DeepScan: time.Hour, Debounce: 40 * time.Millisecond})
	}()
	defer func() { cancel(); <-done }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	waitFor("initial watcher", func() bool { return built.Load() == 1 })
	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(ignorePath, []byte("# nothing ignored now\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor("watcher rebuild", func() bool { return built.Load() == 2 })
	// The reload's rescan mirrors the now-unignored (empty) directory.
	waitFor("rescan to create remote skip/", func() bool {
		_, err := os.Stat(filepath.Join(remote, "skip"))
		return err == nil
	})

	if err := os.WriteFile(filepath.Join(local, "skip", "now-synced.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor("event-driven sync of a file in the un-ignored dir", func() bool {
		b, _ := os.ReadFile(filepath.Join(remote, "skip", "now-synced.txt"))
		return string(b) == "hi"
	})
}
