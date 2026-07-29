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
