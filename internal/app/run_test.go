package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// TestRun_HardScanFailureSkipsRunSummaryInSilentMode is a regression test
// for a behavior-preservation bug introduced when RunRobustOnce was
// extracted from Run: a hard, pre-execution failure (here, fs.ScanPair
// erroring because the local root doesn't exist) must be reported
// immediately, with NO run-summary log event - exactly like the original
// (pre-extraction) Run, which returned such errors before ever reaching its
// printRunSummary call. Driving it in --silent mode, where the run summary
// (and everything else) is a structured JSON log line, makes this directly
// observable: a spurious "sync run summary" event in the log file would
// mean Run started treating a hard failure like a completed-with-errors
// run, which it must not.
func TestRun_HardScanFailureSkipsRunSummaryInSilentMode(t *testing.T) {
	remoteRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(remoteRoot, ".synclock"), []byte("canary"), 0o644); err != nil {
		t.Fatalf("write canary: %v", err)
	}
	stateDir := t.TempDir()
	// Deliberately never created - fs.ScanPair's local-side walk will fail
	// on this before any plan/execute/manifest work happens.
	localRoot := filepath.Join(t.TempDir(), "does-not-exist")

	cfg := runtime.Config{
		LocalRoot:  localRoot,
		RemoteRoot: remoteRoot,
		Workers:    2,
		StateDir:   stateDir,
		Silent:     true,
	}

	err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected Run to return an error when the local root does not exist")
	}

	logBytes, readErr := os.ReadFile(filepath.Join(stateDir, "sync.log"))
	if readErr != nil {
		t.Fatalf("read sync.log: %v", readErr)
	}
	if strings.Contains(string(logBytes), "sync run summary") {
		t.Fatalf("expected no run-summary event on a hard scan failure, got log:\n%s", logBytes)
	}
}
