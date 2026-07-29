package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"syncd/internal/cli"
)

func newTestConfig(t *testing.T, localRoot, remoteRoot string) cli.Config {
	t.Helper()
	// Canary file: with no --init support yet, tests bootstrap it manually,
	// mirroring what a real first-time setup must do by hand today.
	if err := os.WriteFile(filepath.Join(remoteRoot, ".synclock"), []byte("canary"), 0o644); err != nil {
		t.Fatalf("write canary: %v", err)
	}
	return cli.Config{
		LocalRoot:  localRoot,
		RemoteRoot: remoteRoot,
		Workers:    2,
		StateDir:   t.TempDir(),
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestRun_EndToEndSyncLifecycle(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	cfg := newTestConfig(t, localRoot, remoteRoot)
	ctx := context.Background()

	localPath := filepath.Join(localRoot, "a.txt")
	remotePath := filepath.Join(remoteRoot, "a.txt")

	// Run 1: new local file uploads to remote.
	if err := os.WriteFile(localPath, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write local v1: %v", err)
	}
	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("run 1 (upload new file): %v", err)
	}
	if got := mustReadFile(t, remotePath); got != "v1" {
		t.Fatalf("after run 1, remote content = %q, want %q", got, "v1")
	}

	// Run 2: local-only edit uploads the change.
	if err := os.WriteFile(localPath, []byte("v2"), 0o644); err != nil {
		t.Fatalf("write local v2: %v", err)
	}
	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("run 2 (local edit): %v", err)
	}
	if got := mustReadFile(t, remotePath); got != "v2" {
		t.Fatalf("after run 2, remote content = %q, want %q", got, "v2")
	}

	// Run 3: both sides edited since the last manifest -> conflict preserve-both.
	if err := os.WriteFile(localPath, []byte("v3-local"), 0o644); err != nil {
		t.Fatalf("write local v3: %v", err)
	}
	if err := os.WriteFile(remotePath, []byte("v3-remote"), 0o644); err != nil {
		t.Fatalf("write remote v3: %v", err)
	}
	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("run 3 (conflict): %v", err)
	}
	if got := mustReadFile(t, localPath); got != "v3-remote" {
		t.Fatalf("after run 3, local canonical content = %q, want %q (remote wins canonical slot)", got, "v3-remote")
	}
	if got := mustReadFile(t, remotePath); got != "v3-remote" {
		t.Fatalf("after run 3, remote canonical content = %q, want %q", got, "v3-remote")
	}
	localConflicts, err := filepath.Glob(filepath.Join(localRoot, "a_conflict_*.txt"))
	if err != nil {
		t.Fatalf("glob local conflicts: %v", err)
	}
	if len(localConflicts) != 1 {
		t.Fatalf("expected exactly one local conflict file, got %v", localConflicts)
	}
	if got := mustReadFile(t, localConflicts[0]); got != "v3-local" {
		t.Fatalf("local conflict file content = %q, want %q", got, "v3-local")
	}
	remoteConflicts, err := filepath.Glob(filepath.Join(remoteRoot, "a_conflict_*.txt"))
	if err != nil {
		t.Fatalf("glob remote conflicts: %v", err)
	}
	if len(remoteConflicts) != 1 {
		t.Fatalf("expected exactly one remote conflict file, got %v", remoteConflicts)
	}
	if got := mustReadFile(t, remoteConflicts[0]); got != "v3-local" {
		t.Fatalf("remote conflict file content = %q, want %q", got, "v3-local")
	}

	// Run 4: deleting the canonical local file propagates as a remote delete.
	if err := os.Remove(localPath); err != nil {
		t.Fatalf("remove local canonical: %v", err)
	}
	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("run 4 (delete propagation): %v", err)
	}
	if _, err := os.Stat(remotePath); !os.IsNotExist(err) {
		t.Fatalf("expected remote canonical file to be deleted after run 4, stat err = %v", err)
	}
}

func TestRun_DryRunDoesNotMutateFilesystem(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	cfg := newTestConfig(t, localRoot, remoteRoot)
	cfg.DryRun = true
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(localRoot, "new.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("dry-run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(remoteRoot, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not create files on remote, stat err = %v", err)
	}
}

func TestRun_FailedActionSurfacesAsError(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	cfg := newTestConfig(t, localRoot, remoteRoot)
	ctx := context.Background()

	// Block the upload deterministically: put a directory where the uploaded
	// file needs to land, so the final atomic rename fails. This avoids
	// relying on permission bits, which root/CI environments can bypass.
	if err := os.WriteFile(filepath.Join(localRoot, "blocked.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(remoteRoot, "blocked.txt"), 0o755); err != nil {
		t.Fatalf("create blocking directory: %v", err)
	}

	err := Run(ctx, cfg)
	if err == nil {
		t.Fatal("expected Run to return an error when an action fails, got nil")
	}
}

func TestRun_InitBootstrapsDirsAndCanaryWithoutSyncing(t *testing.T) {
	parent := t.TempDir()
	// Local/remote roots deliberately don't exist yet - --init must create them.
	localRoot := filepath.Join(parent, "local")
	remoteRoot := filepath.Join(parent, "remote")

	cfg := cli.Config{
		LocalRoot:  localRoot,
		RemoteRoot: remoteRoot,
		Workers:    2,
		StateDir:   t.TempDir(),
		Init:       true,
	}

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run(--init): %v", err)
	}

	if info, err := os.Stat(localRoot); err != nil || !info.IsDir() {
		t.Fatalf("expected local root to be created, stat err = %v", err)
	}
	if info, err := os.Stat(remoteRoot); err != nil || !info.IsDir() {
		t.Fatalf("expected remote root to be created, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, ".synclock")); err != nil {
		t.Fatalf("expected remote canary file to be created, stat err = %v", err)
	}
}

func TestRun_InitIsIdempotent(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	cfg := cli.Config{
		LocalRoot:  localRoot,
		RemoteRoot: remoteRoot,
		Workers:    2,
		StateDir:   t.TempDir(),
		Init:       true,
	}

	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("first Run(--init): %v", err)
	}
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("second Run(--init) should be a harmless no-op, got: %v", err)
	}
}

func TestRun_HonorsDriveSyncIgnoreFile(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	cfg := newTestConfig(t, localRoot, remoteRoot)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(localRoot, ".drivesyncignore"), []byte("*.scratch\nbuild/\n"), 0o644); err != nil {
		t.Fatalf("write .drivesyncignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "notes.scratch"), []byte("throwaway"), 0o644); err != nil {
		t.Fatalf("write notes.scratch: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(localRoot, "build"), 0o755); err != nil {
		t.Fatalf("mkdir build: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "build", "output.bin"), []byte("built artifact"), 0o644); err != nil {
		t.Fatalf("write build/output.bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "keep.txt"), []byte("real content"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}

	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(remoteRoot, "keep.txt")); err != nil {
		t.Fatalf("expected keep.txt to sync to remote, stat err = %v", err)
	}
	// .drivesyncignore itself is an ordinary file (like .gitignore in a repo)
	// and syncs like any other, so both machines end up with the same rules.
	if _, err := os.Stat(filepath.Join(remoteRoot, ".drivesyncignore")); err != nil {
		t.Fatalf("expected .drivesyncignore to sync to remote, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "notes.scratch")); !os.IsNotExist(err) {
		t.Fatalf("expected notes.scratch to be ignored, not synced to remote, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "build")); !os.IsNotExist(err) {
		t.Fatalf("expected ignored build/ directory not to be created on remote, stat err = %v", err)
	}
}

func TestRun_SyncsEmptyDirectoriesAndCleansUpAfterDelete(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	cfg := newTestConfig(t, localRoot, remoteRoot)
	ctx := context.Background()

	// An empty directory on the local side only should be mirrored to remote.
	if err := os.MkdirAll(filepath.Join(localRoot, "empty-folder"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A directory with one file, which will be deleted this run, leaving it
	// empty - it should then be cleaned up on both sides.
	if err := os.MkdirAll(filepath.Join(localRoot, "temp-folder"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tempFilePath := filepath.Join(localRoot, "temp-folder", "only.txt")
	if err := os.WriteFile(tempFilePath, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if info, err := os.Stat(filepath.Join(remoteRoot, "empty-folder")); err != nil || !info.IsDir() {
		t.Fatalf("expected empty-folder mirrored to remote, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "temp-folder", "only.txt")); err != nil {
		t.Fatalf("expected only.txt uploaded to remote, stat err = %v", err)
	}

	// Now delete the only file in temp-folder locally; the next run should
	// propagate the delete to remote and then clean up the now-empty
	// directory on both sides, while leaving empty-folder (never emptied by
	// a delete) alone.
	if err := os.Remove(tempFilePath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := Run(ctx, cfg); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	if _, err := os.Stat(filepath.Join(localRoot, "temp-folder")); !os.IsNotExist(err) {
		t.Fatalf("expected local temp-folder removed after emptying, err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "temp-folder")); !os.IsNotExist(err) {
		t.Fatalf("expected remote temp-folder removed after emptying, err = %v", err)
	}
	if info, err := os.Stat(filepath.Join(remoteRoot, "empty-folder")); err != nil || !info.IsDir() {
		t.Fatalf("expected empty-folder to remain untouched, stat err = %v", err)
	}
	if info, err := os.Stat(filepath.Join(localRoot, "empty-folder")); err != nil || !info.IsDir() {
		t.Fatalf("expected local empty-folder to remain untouched, stat err = %v", err)
	}
}
