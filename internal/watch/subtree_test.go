package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"syncd/internal/app"
	"syncd/internal/db"
	"syncd/internal/fs"
	"syncd/internal/runtime"
)

// End to end through real fsnotify: renaming a synced directory reaches the
// remote via the event-driven scoped cycle, with the poll and deep scan an
// hour away (docs/follow-ups.md #26).
func TestRun_DirectoryRenamePropagatesWithoutPoll(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, ".synclock"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(local, "a", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "a", "sub", "f.txt"), []byte("f"), 0o644); err != nil {
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
	p := app.RobustParams{LocalRoot: local, RemoteRoot: remote, Workers: 2, Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger, Policy: fs.HashIfChanged}
	if _, _, err := app.RunRobustOnce(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, p, m, LoopConfig{RemotePoll: time.Hour, DeepScan: time.Hour, Debounce: 50 * time.Millisecond})
	}()
	defer func() { cancel(); <-done }()
	time.Sleep(200 * time.Millisecond) // let the watcher arm

	if err := os.Rename(filepath.Join(local, "a"), filepath.Join(local, "b")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		_, newErr := os.Stat(filepath.Join(remote, "b", "sub", "f.txt"))
		_, oldErr := os.Stat(filepath.Join(remote, "a"))
		if newErr == nil && os.IsNotExist(oldErr) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("rename did not propagate via the event path (new: %v, old still present: %v)", newErr, oldErr == nil)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
