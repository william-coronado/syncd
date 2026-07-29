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

func TestLoopSyncsOnLocalEvent(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	// canary so CheckRemoteCanary passes
	os.WriteFile(filepath.Join(remote, ".synclock"), []byte("x"), 0o644)
	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	m, _ := fs.LoadIgnoreMatcher(local)
	p := app.RobustParams{LocalRoot: local, RemoteRoot: remote, Workers: 2, Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, p, m, LoopConfig{RemotePoll: time.Hour, DeepScan: time.Hour, Debounce: 80 * time.Millisecond})

	time.Sleep(150 * time.Millisecond) // let the watcher arm
	os.WriteFile(filepath.Join(local, "a.txt"), []byte("hi"), 0o644)

	deadline := time.After(3 * time.Second)
	for {
		if b, _ := os.ReadFile(filepath.Join(remote, "a.txt")); string(b) == "hi" {
			return // success
		}
		select {
		case <-deadline:
			t.Fatal("local event did not propagate to remote")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestRunClampsNonPositiveDurationsToDefaults is a regression test: Run
// passes cfg.RemotePoll/cfg.DeepScan straight to time.NewTicker, which
// panics on a non-positive duration ("non-positive interval for
// NewTicker"). A user-supplied --remote-poll 0 (or a negative value) must
// not be able to crash the always-on daemon - Run must fall back to the
// documented defaults (30s / 1h) instead.
func TestRunClampsNonPositiveDurationsToDefaults(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(remote, ".synclock"), []byte("x"), 0o644)
	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	m, _ := fs.LoadIgnoreMatcher(local)
	p := app.RobustParams{LocalRoot: local, RemoteRoot: remote, Workers: 1, Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, p, m, LoopConfig{RemotePoll: 0, DeepScan: -1, Debounce: 10 * time.Millisecond})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancellation - possibly panicked or hung on non-positive ticker duration")
	}
}
