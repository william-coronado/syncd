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

// newTestRobustParams opens a fresh manifest db and a discard-friendly
// logger and returns RobustParams wired to sync local<->remote, ready to
// pass to RunRobustOnce/RunRobustScoped. The db and logger are closed
// automatically via t.Cleanup.
func newTestRobustParams(t *testing.T, local, remote string) RobustParams {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	logger, err := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	if err != nil {
		t.Fatalf("runtime.NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	return RobustParams{
		LocalRoot:  local,
		RemoteRoot: remote,
		Workers:    2,
		Repo:       db.NewManifestRepo(sqlDB),
		DB:         sqlDB,
		Logger:     logger,
		Policy:     fs.HashIfChanged,
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

// TestRunRobustOnce_EndToEndSyncLifecycle exercises the full scan -> plan ->
// execute -> manifest-update cycle across several runs: new-file upload,
// local-only edit propagation, a same-file conflict on both sides (preserve-
// both), and delete propagation. This is the same scenario the old
// TestRun_EndToEndSyncLifecycle drove through app.Run before app.Run (the
// superseded always-on-daemon entrypoint) was removed as dead code - the
// behavior itself is unchanged and still lives entirely in RunRobustOnce.
func TestRunRobustOnce_EndToEndSyncLifecycle(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	ctx := context.Background()
	p := newTestRobustParams(t, localRoot, remoteRoot)

	localPath := filepath.Join(localRoot, "a.txt")
	remotePath := filepath.Join(remoteRoot, "a.txt")

	// Run 1: new local file uploads to remote.
	if err := os.WriteFile(localPath, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write local v1: %v", err)
	}
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatalf("run 1 (upload new file): %v", err)
	}
	if got := mustReadFile(t, remotePath); got != "v1" {
		t.Fatalf("after run 1, remote content = %q, want %q", got, "v1")
	}

	// Run 2: local-only edit uploads the change.
	if err := os.WriteFile(localPath, []byte("v2"), 0o644); err != nil {
		t.Fatalf("write local v2: %v", err)
	}
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
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
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
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
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatalf("run 4 (delete propagation): %v", err)
	}
	if _, err := os.Stat(remotePath); !os.IsNotExist(err) {
		t.Fatalf("expected remote canonical file to be deleted after run 4, stat err = %v", err)
	}
}

// TestRunRobustOnce_SkippedSubdirectoryDoesNotDeleteRemoteSubtree is a
// regression test for Critical 2 on the robust (hash-based) path: a local
// subdirectory becomes unreadable AFTER a prior successful sync (the
// manifest already has hash rows for its descendants that match remote
// exactly - the "healthy steady state, nothing changed" case called out in
// the review evidence, which is the MORE likely trigger in practice since a
// stale manifest instead takes the "preserving changed file" download
// branch). Before this fix, BuildPlan's exact-match-only skipped set covered
// only the directory's own relpath, so descendants read as "in manifest, not
// in local, not changed on remote" -> ActionDeleteRemote, propagating a
// delete that destroys real, unrelated remote files.
func TestRunRobustOnce_SkippedSubdirectoryDoesNotDeleteRemoteSubtree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	ctx := context.Background()
	p := newTestRobustParams(t, localRoot, remoteRoot)

	secretDir := filepath.Join(localRoot, "secret")
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		t.Fatalf("mkdir secret: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "a.txt"), []byte("a-content"), 0o644); err != nil {
		t.Fatalf("write secret/a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "keep.txt"), []byte("keep-content"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}

	// Run 1: a normal sync establishes the manifest baseline and mirrors
	// secret/a.txt and keep.txt to remote - the "healthy, already synced"
	// steady state.
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatalf("run 1 (establish baseline): %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "secret", "a.txt")); err != nil {
		t.Fatalf("expected secret/a.txt mirrored to remote after run 1, stat err = %v", err)
	}

	// Now "secret" becomes unreadable locally (e.g. a permission change) -
	// nothing has actually changed content-wise on either side.
	if err := os.Chmod(secretDir, 0o000); err != nil {
		t.Fatalf("chmod secret: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(secretDir, 0o755) })

	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatalf("run 2 (secret unreadable): %v", err)
	}

	if _, err := os.Stat(filepath.Join(remoteRoot, "secret", "a.txt")); err != nil {
		t.Errorf("expected remote secret/a.txt to survive an unreadable local subdirectory, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "keep.txt")); err != nil {
		t.Errorf("expected unrelated keep.txt to be unaffected, stat err = %v", err)
	}
}

func TestRunInit_BootstrapsDirsAndCanaryWithoutSyncing(t *testing.T) {
	parent := t.TempDir()
	// Local/remote roots deliberately don't exist yet - RunInit must create them.
	localRoot := filepath.Join(parent, "local")
	remoteRoot := filepath.Join(parent, "remote")

	logger, err := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	if err != nil {
		t.Fatalf("runtime.NewLogger: %v", err)
	}
	defer logger.Close()

	if err := RunInit(localRoot, remoteRoot, logger); err != nil {
		t.Fatalf("RunInit: %v", err)
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

func TestRunInit_IsIdempotent(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()

	logger, err := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	if err != nil {
		t.Fatalf("runtime.NewLogger: %v", err)
	}
	defer logger.Close()

	if err := RunInit(localRoot, remoteRoot, logger); err != nil {
		t.Fatalf("first RunInit: %v", err)
	}
	if err := RunInit(localRoot, remoteRoot, logger); err != nil {
		t.Fatalf("second RunInit should be a harmless no-op, got: %v", err)
	}
}

func TestRunRobustOnce_HonorsSyncdignoreFile(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	ctx := context.Background()
	p := newTestRobustParams(t, localRoot, remoteRoot)

	if err := os.WriteFile(filepath.Join(localRoot, ".syncdignore"), []byte("*.scratch\nbuild/\n"), 0o644); err != nil {
		t.Fatalf("write .syncdignore: %v", err)
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

	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatalf("run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(remoteRoot, "keep.txt")); err != nil {
		t.Fatalf("expected keep.txt to sync to remote, stat err = %v", err)
	}
	// .syncdignore itself is an ordinary file (like .gitignore in a repo)
	// and syncs like any other, so both machines end up with the same rules.
	if _, err := os.Stat(filepath.Join(remoteRoot, ".syncdignore")); err != nil {
		t.Fatalf("expected .syncdignore to sync to remote, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "notes.scratch")); !os.IsNotExist(err) {
		t.Fatalf("expected notes.scratch to be ignored, not synced to remote, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "build")); !os.IsNotExist(err) {
		t.Fatalf("expected ignored build/ directory not to be created on remote, stat err = %v", err)
	}
}

func TestRunRobustOnce_SyncsEmptyDirectoriesAndCleansUpAfterDelete(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	ctx := context.Background()
	p := newTestRobustParams(t, localRoot, remoteRoot)

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

	if _, _, err := RunRobustOnce(ctx, p); err != nil {
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
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
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
