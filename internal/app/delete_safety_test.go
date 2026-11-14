package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"syncd/internal/fs"
	"syncd/internal/reconcile"
	"syncd/internal/runtime"
)

func mustLimit(t *testing.T, s string) reconcile.DeleteLimit {
	t.Helper()
	l, err := reconcile.ParseDeleteLimit(s)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func writeFiles(t *testing.T, root string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.txt", i)), []byte(fmt.Sprint(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// emptyDir removes root's contents but keeps root itself - what a local
// tree looks like after an accidental wipe.
func emptyDir(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

// The catastrophe the guard exists for: the local tree is emptied, and the
// next cycle wants to delete everything tracked on the remote.
func TestRunRobustOnce_DeleteLimitRefusesMassDelete(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	writeFiles(t, local, 20)
	p := newTestRobustParams(t, local, remote)
	p.MaxDelete = mustLimit(t, "50%")
	ctx := context.Background()

	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	emptyDir(t, local)

	_, _, err := RunRobustOnce(ctx, p)
	if !errors.Is(err, reconcile.ErrDeleteLimit) {
		t.Fatalf("got %v, want ErrDeleteLimit", err)
	}
	if got := countFiles(t, remote); got != 20 {
		t.Fatalf("remote has %d files after a refused cycle, want 20", got)
	}

	// Still refused next cycle: a refusal must not have advanced the manifest.
	if _, _, err := RunRobustOnce(ctx, p); !errors.Is(err, reconcile.ErrDeleteLimit) {
		t.Fatalf("second cycle: got %v, want ErrDeleteLimit", err)
	}

	// One approval lets exactly one cycle through.
	approvals := 1
	p.ApproveDeletes = func() bool { approvals--; return approvals >= 0 }
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatalf("approved cycle: %v", err)
	}
	if got := countFiles(t, remote); got != 0 {
		t.Fatalf("remote has %d files after an approved cycle, want 0", got)
	}
}

func TestRunRobustOnce_DeleteLimitAllowsOrdinaryDeletes(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	writeFiles(t, local, 20)
	p := newTestRobustParams(t, local, remote)
	p.MaxDelete = mustLimit(t, "50%")
	ctx := context.Background()
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		os.Remove(filepath.Join(local, fmt.Sprintf("f%02d.txt", i)))
	}
	if _, failures, err := RunRobustOnce(ctx, p); err != nil || failures != 0 {
		t.Fatalf("cycle: failures=%d err=%v", failures, err)
	}
	if got := countFiles(t, remote); got != 15 {
		t.Fatalf("remote has %d files, want 15", got)
	}
}

// The scoped (fsnotify) cycle only loads the batch's manifest rows, so it
// must measure a percentage against the whole manifest - not conclude that
// deleting 100% of a 12-file batch is a mass delete.
func TestRunRobustScoped_DeleteLimitUsesWholeManifest(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	writeFiles(t, local, 40)
	p := newTestRobustParams(t, local, remote)
	p.MaxDelete = mustLimit(t, "50%")
	ctx := context.Background()
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatal(err)
	}

	batch := make(map[string]struct{})
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("f%02d.txt", i)
		os.Remove(filepath.Join(local, name))
		batch[name] = struct{}{}
	}
	if _, failures, err := RunRobustScoped(ctx, p, batch); err != nil || failures != 0 {
		t.Fatalf("12 of 40: failures=%d err=%v", failures, err)
	}
	if got := countFiles(t, remote); got != 28 {
		t.Fatalf("remote has %d files, want 28", got)
	}

	// 20 of the remaining 28 is over half: refused.
	batch = make(map[string]struct{})
	for i := 12; i < 32; i++ {
		name := fmt.Sprintf("f%02d.txt", i)
		os.Remove(filepath.Join(local, name))
		batch[name] = struct{}{}
	}
	if _, _, err := RunRobustScoped(ctx, p, batch); !errors.Is(err, reconcile.ErrDeleteLimit) {
		t.Fatalf("20 of 28: got %v, want ErrDeleteLimit", err)
	}
	if got := countFiles(t, remote); got != 28 {
		t.Fatalf("remote has %d files after a refused scoped cycle, want 28", got)
	}
}

// A delete propagated to the local side lands in the trash, not in the void.
func TestRunRobustOnce_LocalDeleteGoesToTrash(t *testing.T) {
	local := filepath.Join(t.TempDir(), "proj")
	remote := t.TempDir()
	if err := os.MkdirAll(filepath.Join(local, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "sub", "keep.txt"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	trashDir := filepath.Join(t.TempDir(), "trash")
	p := newTestRobustParams(t, local, remote)
	p.Trash = fs.NewTrash(trashDir)
	ctx := context.Background()
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatal(err)
	}

	// Another machine deletes it on the remote.
	if err := os.Remove(filepath.Join(remote, "sub", "keep.txt")); err != nil {
		t.Fatal(err)
	}
	if _, failures, err := RunRobustOnce(ctx, p); err != nil || failures != 0 {
		t.Fatalf("cycle: failures=%d err=%v", failures, err)
	}
	if _, err := os.Stat(filepath.Join(local, "sub", "keep.txt")); !os.IsNotExist(err) {
		t.Fatal("local file was not deleted")
	}
	trashed := filepath.Join(trashDir, time.Now().Format("2006-01-02"), "proj", "sub", "keep.txt")
	if got := mustReadFile(t, trashed); got != "precious" {
		t.Fatalf("trashed copy = %q", got)
	}
}

func TestSyncOnce_MaxDeleteGuardsUnattendedRunsOnly(t *testing.T) {
	for _, useHash := range []bool{false, true} {
		t.Run(fmt.Sprintf("hash=%v", useHash), func(t *testing.T) {
			left, right := t.TempDir(), t.TempDir()
			writeFiles(t, left, 20)
			opts := SyncOptions{
				Name: "t", Left: left, Right: right, Mode: reconcile.FastMirror, UseHash: useHash,
				Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
				MaxDelete: mustLimit(t, "50%"),
			}
			ctx := context.Background()
			if err := SyncOnce(ctx, opts); err != nil {
				t.Fatalf("initial: %v", err)
			}
			emptyDir(t, left)

			if err := SyncOnce(ctx, opts); !errors.Is(err, reconcile.ErrDeleteLimit) {
				t.Fatalf("--yes run: got %v, want ErrDeleteLimit", err)
			}
			if got := countFiles(t, right); got != 20 {
				t.Fatalf("right has %d files after a refused run, want 20", got)
			}

			// Interactive: the confirm prompt is the gate, not the limit.
			opts.Assume = false
			asked := false
			opts.Confirm = func(string) bool { asked = true; return true }
			if err := SyncOnce(ctx, opts); err != nil {
				t.Fatalf("confirmed run: %v", err)
			}
			if !asked {
				t.Fatal("confirm was not consulted")
			}
			if got := countFiles(t, right); got != 0 {
				t.Fatalf("right has %d files after a confirmed run, want 0", got)
			}
		})
	}
}

func TestSyncOnce_MirrorDeleteGoesToTrash(t *testing.T) {
	left := t.TempDir()
	right := filepath.Join(t.TempDir(), "dest")
	if err := os.MkdirAll(right, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(right, "extra.txt"), []byte("only here"), 0o644); err != nil {
		t.Fatal(err)
	}
	trashDir := filepath.Join(t.TempDir(), "trash")
	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, Mode: reconcile.FastMirror,
		Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
		Trash: fs.NewTrash(trashDir),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(right, "extra.txt")); !os.IsNotExist(err) {
		t.Fatal("mirror did not delete the extra file")
	}
	trashed := filepath.Join(trashDir, time.Now().Format("2006-01-02"), "dest", "extra.txt")
	if got := mustReadFile(t, trashed); got != "only here" {
		t.Fatalf("trashed copy = %q", got)
	}
}

func TestRunWatch_WaitsForRemote(t *testing.T) {
	prev := remoteWaitInterval
	remoteWaitInterval = 10 * time.Millisecond
	t.Cleanup(func() { remoteWaitInterval = prev })

	local := t.TempDir()
	remote := filepath.Join(t.TempDir(), "share") // "not mounted" yet
	cfg := WatchConfig{LocalRoot: local, RemoteRoot: remote, StateDir: t.TempDir(), WaitForRemote: true, NoTrash: true}

	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = os.MkdirAll(remote, 0o755)
		time.Sleep(60 * time.Millisecond)
		_, _ = runtime.EnsureRemoteCanary(remote)
	}()

	loopCalled := false
	err := RunWatch(context.Background(), cfg, func(context.Context, RobustParams, *fs.IgnoreMatcher, WatchConfig) error {
		loopCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
	if !loopCalled {
		t.Fatal("loop never started once the remote appeared")
	}
}

func TestRunWatch_WaitForRemoteStopsCleanlyOnCancel(t *testing.T) {
	prev := remoteWaitInterval
	remoteWaitInterval = 10 * time.Millisecond
	t.Cleanup(func() { remoteWaitInterval = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	cfg := WatchConfig{LocalRoot: t.TempDir(), RemoteRoot: filepath.Join(t.TempDir(), "never"), StateDir: t.TempDir(), WaitForRemote: true}
	err := RunWatch(ctx, cfg, func(context.Context, RobustParams, *fs.IgnoreMatcher, WatchConfig) error {
		t.Error("loop started without a remote")
		return nil
	})
	if err != nil {
		t.Fatalf("cancelled wait should be a clean exit, got %v", err)
	}
}

// approve-deletes must write its token exactly where the daemon for the
// same roots consumes it.
func TestApproveDeletes_ReachesTheDaemonForTheSameRoots(t *testing.T) {
	local, remote, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := runtime.EnsureRemoteCanary(remote); err != nil {
		t.Fatal(err)
	}
	cfg := WatchConfig{LocalRoot: local, RemoteRoot: remote, StateDir: stateDir, NoTrash: true}

	tokenPath, err := ApproveDeletes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("token not written: %v", err)
	}

	var first, second bool
	err = RunWatch(context.Background(), cfg, func(_ context.Context, p RobustParams, _ *fs.IgnoreMatcher, _ WatchConfig) error {
		first, second = p.ApproveDeletes(), p.ApproveDeletes()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first || second {
		t.Fatalf("approval honored first=%v second=%v, want true then false", first, second)
	}
}
