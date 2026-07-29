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
