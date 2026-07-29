package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"syncd/internal/db"
	"syncd/internal/reconcile"
)

func TestSyncOnceMirrorEndToEnd(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, Mode: reconcile.FastMirror,
		Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(right, "a.txt")); string(b) != "hi" {
		t.Error("mirror did not copy file")
	}
}

func TestSyncOnceTwoWayPersistsState(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	dbp := filepath.Join(t.TempDir(), "m.db")
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	opts := SyncOptions{Name: "j", Left: left, Right: right, Mode: reconcile.FastTwoWay, Assume: true, Workers: 1, DBPath: dbp}
	if err := SyncOnce(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	// Delete on left; second run should propagate delete to right (state proves prior sync).
	if err := os.Remove(filepath.Join(left, "a.txt")); err != nil {
		t.Fatalf("remove a.txt: %v", err)
	}
	if err := SyncOnce(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(right, "a.txt")); !os.IsNotExist(err) {
		t.Error("two-way delete not propagated using persisted state")
	}
}

// TestSyncOnceMirrorMissingSourceErrorsWithoutTouchingDest guards against a
// data-loss footgun: if a mirror's --left (source) is mistyped or missing,
// SyncOnce must refuse rather than silently treating a fabricated empty
// source as "the source has nothing, so delete everything on the
// destination" - which, under --yes, would wipe the destination outright.
func TestSyncOnceMirrorMissingSourceErrorsWithoutTouchingDest(t *testing.T) {
	right := t.TempDir()
	if err := os.WriteFile(filepath.Join(right, "keep.txt"), []byte("precious"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}
	missingLeft := filepath.Join(t.TempDir(), "does-not-exist")

	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: missingLeft, Right: right, Mode: reconcile.FastMirror,
		Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err == nil {
		t.Fatal("expected SyncOnce to error when the mirror source does not exist, got nil")
	}

	if b, readErr := os.ReadFile(filepath.Join(right, "keep.txt")); readErr != nil || string(b) != "precious" {
		t.Fatalf("destination file was touched: read err=%v content=%q", readErr, b)
	}
	if _, statErr := os.Stat(missingLeft); !os.IsNotExist(statErr) {
		t.Fatalf("expected source path to remain nonexistent (must not be auto-created), stat err = %v", statErr)
	}
}

// TestSyncOnceMirror_SkippedSymlinkDoesNotDeleteRealCounterpart is a
// regression test for Critical 1: left has a symlink at "link.txt" (skipped
// during scan - syncd never follows symlinks), while right has a REAL
// regular file at the same path. Before this fix, runFastOnce discarded
// scans.Local.Skipped/scans.Remote.Skipped entirely, so FastCompare's mirror
// mode saw "link.txt" as present nowhere in `left` and concluded it had been
// deleted from the source, emitting (and, under --yes, executing) a DeleteR
// that destroyed the real file on the right.
func TestSyncOnceMirror_SkippedSymlinkDoesNotDeleteRealCounterpart(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "target.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write target.txt: %v", err)
	}
	if err := os.Symlink(filepath.Join(left, "target.txt"), filepath.Join(left, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(right, "link.txt"), []byte("precious real file"), 0o644); err != nil {
		t.Fatalf("write right link.txt: %v", err)
	}

	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, Mode: reconcile.FastMirror,
		Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}

	b, readErr := os.ReadFile(filepath.Join(right, "link.txt"))
	if readErr != nil {
		t.Fatalf("expected right's real link.txt to survive mirror, got stat/read error: %v", readErr)
	}
	if string(b) != "precious real file" {
		t.Errorf("right's link.txt content = %q, want unchanged %q", b, "precious real file")
	}
}

// TestSyncOnceMirror_SkippedSubdirectoryDoesNotDeleteRemoteSubtree is a
// regression test for Critical 2: an unreadable local subdirectory
// ("secret") means its descendants are never enumerated locally at all -
// they appear in neither Files nor (before this fix) anywhere a reconciler
// could see them as "known but unreadable" rather than "gone". Right already
// has real files under "secret/" (the steady state after a prior successful
// sync). Mirror must leave them alone, not mass-delete the subtree.
func TestSyncOnceMirror_SkippedSubdirectoryDoesNotDeleteRemoteSubtree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "keep.txt"), []byte("k"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}
	secretDir := filepath.Join(left, "secret")
	if err := os.Mkdir(secretDir, 0o755); err != nil {
		t.Fatalf("mkdir secret: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write secret/a.txt: %v", err)
	}
	if err := os.Chmod(secretDir, 0o000); err != nil {
		t.Fatalf("chmod secret: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(secretDir, 0o755) })

	if err := os.WriteFile(filepath.Join(right, "keep.txt"), []byte("k"), 0o644); err != nil {
		t.Fatalf("write right keep.txt: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(right, "secret"), 0o755); err != nil {
		t.Fatalf("mkdir right secret: %v", err)
	}
	if err := os.WriteFile(filepath.Join(right, "secret", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write right secret/a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(right, "secret", "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatalf("write right secret/b.txt: %v", err)
	}

	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, Mode: reconcile.FastMirror,
		Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(right, "secret", "a.txt")); statErr != nil {
		t.Errorf("expected right's secret/a.txt to survive mirror, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(right, "secret", "b.txt")); statErr != nil {
		t.Errorf("expected right's secret/b.txt to survive mirror, stat err = %v", statErr)
	}
}

// TestSyncOnceHashFailedActionSurfacesAsError covers, for the --hash/robust
// path, the same "a failed action must surface as a non-nil error" behavior
// the old TestRun_FailedActionSurfacesAsError checked via the now-removed
// app.Run. That behavior still lives in runRobustOnce's failures>0 check
// (sync_once.go), reached here through the exported SyncOnce entry point,
// same as the real `syncd sync --hash` CLI path.
func TestSyncOnceHashFailedActionSurfacesAsError(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()

	// Block the upload deterministically: put a directory where the
	// uploaded file needs to land, so the final atomic rename fails. This
	// avoids relying on permission bits, which root/CI environments can
	// bypass.
	if err := os.WriteFile(filepath.Join(left, "blocked.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(right, "blocked.txt"), 0o755); err != nil {
		t.Fatalf("create blocking directory: %v", err)
	}

	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, UseHash: true,
		Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err == nil {
		t.Fatal("expected SyncOnce to return an error when an action fails, got nil")
	}
}

// TestFastSummary_ListsPathsDestructiveFirstWithCap covers Task 5e's fix to
// fastSummary: the old summary showed only counts ("DeleteR: 412") with no
// indication of which files - for a destructive mirror run, the confirm
// prompt is the only safety gate, and it must show what's being approved.
// This checks the fix's three required properties: destructive types
// (DeleteL/DeleteR) are listed before copies, paths are capped at 20 per
// type with a "... and N more" tail, and paths are sorted (so the shown/cut
// boundary is deterministic).
func TestFastSummary_ListsPathsDestructiveFirstWithCap(t *testing.T) {
	var tasks []reconcile.FastTask
	for i := 0; i < 25; i++ {
		tasks = append(tasks, reconcile.FastTask{Type: reconcile.DeleteR, RelPath: fmt.Sprintf("del-%02d.txt", i)})
	}
	tasks = append(tasks, reconcile.FastTask{Type: reconcile.CopyL2R, RelPath: "new.txt"})

	out := fastSummary("job", reconcile.FastMirror, tasks)

	delIdx := strings.Index(out, "DeleteR")
	copyIdx := strings.Index(out, "CopyL2R")
	if delIdx == -1 || copyIdx == -1 {
		t.Fatalf("expected both DeleteR and CopyL2R sections, got:\n%s", out)
	}
	if delIdx > copyIdx {
		t.Errorf("expected DeleteR (destructive) section before CopyL2R, got:\n%s", out)
	}
	if !strings.Contains(out, "... and 5 more") {
		t.Errorf("expected a cap tail '... and 5 more' (25 deletes, cap 20), got:\n%s", out)
	}
	if !strings.Contains(out, "del-00.txt") || !strings.Contains(out, "del-19.txt") {
		t.Errorf("expected the first 20 sorted paths (del-00..del-19) shown, got:\n%s", out)
	}
	if strings.Contains(out, "del-24.txt") {
		t.Errorf("expected paths beyond the cap not to be listed, got:\n%s", out)
	}
	if !strings.Contains(out, "new.txt") {
		t.Errorf("expected new.txt (CopyL2R) listed, got:\n%s", out)
	}
}

// TestRobustSummary_ListsPathsDestructiveFirstWithCap mirrors
// TestFastSummary_ListsPathsDestructiveFirstWithCap for the --hash path's
// robustSummary.
func TestRobustSummary_ListsPathsDestructiveFirstWithCap(t *testing.T) {
	var actions []reconcile.Action
	for i := 0; i < 25; i++ {
		actions = append(actions, reconcile.Action{Type: reconcile.ActionDeleteRemote, Path: fmt.Sprintf("del-%02d.txt", i)})
	}
	actions = append(actions, reconcile.Action{Type: reconcile.ActionUpload, Path: "new.txt"})

	out := robustSummary("job", actions)

	delIdx := strings.Index(out, "DELETE_REMOTE")
	uploadIdx := strings.Index(out, "UPLOAD")
	if delIdx == -1 || uploadIdx == -1 {
		t.Fatalf("expected both DELETE_REMOTE and UPLOAD sections, got:\n%s", out)
	}
	if delIdx > uploadIdx {
		t.Errorf("expected DELETE_REMOTE (destructive) section before UPLOAD, got:\n%s", out)
	}
	if !strings.Contains(out, "... and 5 more") {
		t.Errorf("expected a cap tail '... and 5 more' (25 deletes, cap 20), got:\n%s", out)
	}
	if strings.Contains(out, "del-24.txt") {
		t.Errorf("expected paths beyond the cap not to be listed, got:\n%s", out)
	}
	if !strings.Contains(out, "new.txt") {
		t.Errorf("expected new.txt (UPLOAD) listed, got:\n%s", out)
	}
}

// TestSyncOnceDryRun_FastMirrorLeavesTreeUntouched is the fast-path half of
// Task 5e's --dry-run requirement: a dry run must print the summary and
// exit without mutating anything.
func TestSyncOnceDryRun_FastMirrorLeavesTreeUntouched(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(right, "stale.txt"), []byte("old"), 0o644); err != nil {
		t.Fatalf("write stale.txt: %v", err)
	}

	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, Mode: reconcile.FastMirror,
		DryRun: true, Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err != nil {
		t.Fatalf("dry-run SyncOnce returned error: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(right, "a.txt")); !os.IsNotExist(statErr) {
		t.Error("dry-run copied a.txt to right - tree was mutated")
	}
	if b, readErr := os.ReadFile(filepath.Join(right, "stale.txt")); readErr != nil || string(b) != "old" {
		t.Errorf("dry-run deleted or modified stale.txt: err=%v content=%q", readErr, b)
	}
}

// TestSyncOnceDryRun_TwoWayDoesNotPersistJobState is the subtle part of
// Task 5e: for two-way, runFastOnce calls repo.Replace after executing to
// persist job_state - a dry run must skip both the execution AND that
// write, or the next real run would compute its diff against a baseline
// that never actually happened.
func TestSyncOnceDryRun_TwoWayDoesNotPersistJobState(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	dbp := filepath.Join(t.TempDir(), "m.db")
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	opts := SyncOptions{Name: "j", Left: left, Right: right, Mode: reconcile.FastTwoWay, DryRun: true, Assume: true, Workers: 1, DBPath: dbp}
	if err := SyncOnce(context.Background(), opts); err != nil {
		t.Fatalf("dry-run SyncOnce returned error: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(right, "a.txt")); !os.IsNotExist(statErr) {
		t.Fatal("dry-run copied a.txt to right - tree was mutated")
	}

	sqlDB, err := db.Open(dbp)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close()
	repo := db.NewJobStateRepo(sqlDB)
	state, err := repo.Load(context.Background(), "j")
	if err != nil {
		t.Fatalf("load job state: %v", err)
	}
	if len(state) != 0 {
		t.Errorf("expected no persisted job state after a dry-run, got %d entries", len(state))
	}
}

// TestSyncOnceDryRun_HashDoesNotMutate is the --hash-path half of the
// --dry-run requirement.
func TestSyncOnceDryRun_HashDoesNotMutate(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, UseHash: true,
		DryRun: true, Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err != nil {
		t.Fatalf("dry-run SyncOnce (hash) returned error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(right, "a.txt")); !os.IsNotExist(statErr) {
		t.Error("dry-run (hash) copied a.txt to right - tree was mutated")
	}
}

// TestSyncOnceDryRun_DoesNotRequireYes checks that --dry-run works without
// --yes: a dry run must never invoke the interactive Confirm callback (it
// only prints and exits), even when opts.Assume is false.
func TestSyncOnceDryRun_DoesNotRequireYes(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	confirmCalled := false
	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, Mode: reconcile.FastMirror,
		DryRun: true, Assume: false, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
		Confirm: func(string) bool { confirmCalled = true; return true },
	})
	if err != nil {
		t.Fatalf("dry-run SyncOnce returned error: %v", err)
	}
	if confirmCalled {
		t.Error("dry-run must not invoke the interactive Confirm callback")
	}
	if _, statErr := os.Stat(filepath.Join(right, "a.txt")); !os.IsNotExist(statErr) {
		t.Error("dry-run copied a.txt to right - tree was mutated")
	}
}

// TestSyncOnceDryRun_MirrorDoesNotCreateMissingDestination is a regression
// test for a gap the earlier --dry-run fix missed: SyncOnce called
// prepareRoots unconditionally, and prepareRoots os.MkdirAll's the
// destination for mirror/update/--hash - so a dry run against a
// not-yet-existing destination silently created a real, empty directory on
// disk (including, in production, on the mounted SMB share). "exits without
// mutating anything" must mean the destination is never created either, not
// just that no files are copied/deleted into it.
func TestSyncOnceDryRun_MirrorDoesNotCreateMissingDestination(t *testing.T) {
	left := t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	// A destination path that does NOT exist yet and is never created by
	// the test - t.TempDir() only creates its own directory, not this
	// child path.
	right := filepath.Join(t.TempDir(), "does-not-exist-yet")

	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, Mode: reconcile.FastMirror,
		DryRun: true, Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err != nil {
		t.Fatalf("dry-run SyncOnce returned error: %v", err)
	}
	if _, statErr := os.Stat(right); !os.IsNotExist(statErr) {
		t.Errorf("dry-run created the destination directory %s - stat err = %v", right, statErr)
	}
}

// TestSyncOnceDryRun_HashDoesNotCreateMissingDestination is the --hash-path
// counterpart to TestSyncOnceDryRun_MirrorDoesNotCreateMissingDestination.
func TestSyncOnceDryRun_HashDoesNotCreateMissingDestination(t *testing.T) {
	left := t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	right := filepath.Join(t.TempDir(), "does-not-exist-yet")

	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, UseHash: true,
		DryRun: true, Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err != nil {
		t.Fatalf("dry-run SyncOnce (hash) returned error: %v", err)
	}
	if _, statErr := os.Stat(right); !os.IsNotExist(statErr) {
		t.Errorf("dry-run (hash) created the destination directory %s - stat err = %v", right, statErr)
	}
}

// TestSyncOnceDryRun_TwoWayDoesNotCreateEitherMissingRoot is the two-way
// half of the same fix, and the strictly worse case the reviewer flagged:
// prepareRoots creates BOTH roots for two-way, so a dry run must leave
// BOTH untouched when neither pre-exists.
func TestSyncOnceDryRun_TwoWayDoesNotCreateEitherMissingRoot(t *testing.T) {
	parent := t.TempDir()
	left := filepath.Join(parent, "left-does-not-exist-yet")
	right := filepath.Join(parent, "right-does-not-exist-yet")

	err := SyncOnce(context.Background(), SyncOptions{
		Name: "j", Left: left, Right: right, Mode: reconcile.FastTwoWay,
		DryRun: true, Assume: true, Workers: 1, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err != nil {
		t.Fatalf("dry-run SyncOnce (two-way) returned error: %v", err)
	}
	if _, statErr := os.Stat(left); !os.IsNotExist(statErr) {
		t.Errorf("dry-run created the left root %s - stat err = %v", left, statErr)
	}
	if _, statErr := os.Stat(right); !os.IsNotExist(statErr) {
		t.Errorf("dry-run created the right root %s - stat err = %v", right, statErr)
	}
}

// TestSyncOnceDryRun_MirrorMissingSourceStillErrors checks that skipping
// destination creation under --dry-run did not also weaken the data-loss
// guard: a dry run against a genuinely missing mirror SOURCE must still
// report the same error as a real run - the guard is a validation, not a
// creation, and only the creation half may be skipped for --dry-run.
func TestSyncOnceDryRun_MirrorMissingSourceStillErrors(t *testing.T) {
	right := t.TempDir()
	missingLeft := filepath.Join(t.TempDir(), "does-not-exist")

	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: missingLeft, Right: right, Mode: reconcile.FastMirror,
		DryRun: true, Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err == nil {
		t.Fatal("expected a dry-run mirror with a missing source to error, got nil")
	}
	if _, statErr := os.Stat(missingLeft); !os.IsNotExist(statErr) {
		t.Fatalf("expected source path to remain nonexistent, stat err = %v", statErr)
	}
}
