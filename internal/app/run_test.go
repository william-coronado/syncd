package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"syncd/internal/db"
	"syncd/internal/fs"
	"syncd/internal/runtime"
)

func TestRunRobustOnceUploadsNewLocalFile(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(local, "a.txt"), []byte("hi"), 0o644)
	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	_, failures, err := RunRobustOnce(context.Background(), RobustParams{
		LocalRoot: local, RemoteRoot: remote, Workers: 2,
		Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger, Policy: fs.HashIfChanged,
	})
	if err != nil || failures != 0 {
		t.Fatalf("err=%v failures=%d", err, failures)
	}
	if b, _ := os.ReadFile(filepath.Join(remote, "a.txt")); string(b) != "hi" {
		t.Error("file not uploaded to remote")
	}
}

func TestRunRobustOnceOnlyPathsRestrictsScope(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(local, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(local, "b.txt"), []byte("b"), 0o644)
	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	_, _, err := RunRobustOnce(context.Background(), RobustParams{
		LocalRoot: local, RemoteRoot: remote, Workers: 1,
		Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger, Policy: fs.HashIfChanged,
		OnlyPaths: map[string]struct{}{"a.txt": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(remote, "a.txt")); err != nil {
		t.Error("a.txt should have synced")
	}
	if _, err := os.Stat(filepath.Join(remote, "b.txt")); !os.IsNotExist(err) {
		t.Error("b.txt was out of scope and must not sync")
	}
}
