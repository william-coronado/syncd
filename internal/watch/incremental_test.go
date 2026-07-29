package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"syncd/internal/app"
	"syncd/internal/db"
	"syncd/internal/runtime"
)

func TestReconcilePathsSyncsOnlyGivenPaths(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(local, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(local, "b.txt"), []byte("b"), 0o644)
	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	p := app.RobustParams{LocalRoot: local, RemoteRoot: remote, Workers: 1, Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger}
	failures, err := ReconcilePaths(context.Background(), p, map[string]struct{}{"a.txt": {}})
	if err != nil || failures != 0 {
		t.Fatalf("err=%v failures=%d", err, failures)
	}
	if b, _ := os.ReadFile(filepath.Join(remote, "a.txt")); string(b) != "a" {
		t.Error("a.txt should have synced to remote")
	}
	if _, err := os.Stat(filepath.Join(remote, "b.txt")); !os.IsNotExist(err) {
		t.Error("b.txt was not in the batch and must not sync")
	}
}
