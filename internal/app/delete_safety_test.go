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
	p.ApproveDeletes = func(string) bool { approvals--; return approvals >= 0 }
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
	trashed := filepath.Join(trashDir, time.Now().Format("2006-01-02"), fs.RootFolder(local), "sub", "keep.txt")
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
	trashed := filepath.Join(trashDir, time.Now().Format("2006-01-02"), fs.RootFolder(right), "extra.txt")
	if got := mustReadFile(t, trashed); got != "only here" {
		t.Fatalf("trashed copy = %q", got)
	}
}

func TestRunWatch_WaitsForRemote(t *testing.T) {
	prev := remoteWaitInterval
	remoteWaitInterval = 5 * time.Millisecond
	t.Cleanup(func() { remoteWaitInterval = prev })

	local := t.TempDir()
	remote := filepath.Join(t.TempDir(), "share") // "not mounted" yet
	cfg := WatchConfig{LocalRoot: local, RemoteRoot: remote, StateDir: t.TempDir(), WaitForRemote: true, NoTrash: true}

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(context.Background(), cfg, func(context.Context, RobustParams, *fs.IgnoreMatcher, WatchConfig) error {
			close(started)
			return nil
		})
	}()
	notStarted := func(when string) {
		t.Helper()
		select {
		case <-started:
			t.Fatalf("loop started %s", when)
		case <-time.After(60 * time.Millisecond): // a dozen wait intervals
		}
	}

	notStarted("with no remote at all")
	// The mountpoint appearing is not the share being ready: only its
	// canary is.
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	notStarted("when the remote directory existed but its canary did not")

	if _, err := runtime.EnsureRemoteCanary(remote); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("loop never started once the canary appeared")
	}
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
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

// approve-deletes must read the refusal, and write its token, exactly
// where the daemon for the same roots records and consumes them - and the
// approval must be good for that refused plan only.
func TestApproveDeletes_ApprovesTheRefusedPlanForTheSameRoots(t *testing.T) {
	local, remote, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := runtime.EnsureRemoteCanary(remote); err != nil {
		t.Fatal(err)
	}
	cfg := WatchConfig{LocalRoot: local, RemoteRoot: remote, StateDir: stateDir, NoTrash: true}

	if _, err := ApproveDeletes(cfg); !errors.Is(err, runtime.ErrNoDeleteRefusal) {
		t.Fatalf("nothing refused yet: got %v, want ErrNoDeleteRefusal", err)
	}

	var other, first, second bool
	var shown runtime.DeleteRefusal
	err := RunWatch(context.Background(), cfg, func(_ context.Context, p RobustParams, _ *fs.IgnoreMatcher, _ WatchConfig) error {
		p.OnDeleteRefused(runtime.DeleteRefusal{Fingerprint: "plan-a", Remote: 26, Tracked: 27})
		var err error
		if shown, err = ApproveDeletes(cfg); err != nil {
			return err
		}
		other = p.ApproveDeletes("plan-b")
		first, second = p.ApproveDeletes("plan-a"), p.ApproveDeletes("plan-a")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if shown.Remote != 26 || shown.Tracked != 27 {
		t.Fatalf("approve-deletes showed %+v, not the refused plan", shown)
	}
	if other {
		t.Fatal("approval of plan-a was honored for plan-b")
	}
	if !first || second {
		t.Fatalf("approval honored first=%v second=%v, want true then false", first, second)
	}
}

// A wipe that reaches the daemon as several fsnotify batches, each under
// the limit on its own, must be stopped by the rolling total.
func TestRunRobustScoped_SplitWipeIsStoppedByTheRollingLimit(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	writeFiles(t, local, 60)
	p := newTestRobustParams(t, local, remote)
	p.MaxDelete = mustLimit(t, "50%")
	p.DeleteWindow = NewDeleteWindow(DefaultDeleteWindow)
	var refused []runtime.DeleteRefusal
	p.OnDeleteRefused = func(r runtime.DeleteRefusal) { refused = append(refused, r) }
	ctx := context.Background()
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatal(err)
	}

	deleteBatch := func(from, to int) error {
		batch := make(map[string]struct{})
		for i := from; i < to; i++ {
			name := fmt.Sprintf("f%02d.txt", i)
			os.Remove(filepath.Join(local, name))
			batch[name] = struct{}{}
		}
		_, _, err := RunRobustScoped(ctx, p, batch)
		return err
	}
	// 12 of 60, then 12 of the remaining 48, 12 of 36: each batch is well
	// under half of what is left, and without the window all would pass.
	if err := deleteBatch(0, 12); err != nil {
		t.Fatalf("batch 1: %v", err)
	}
	if err := deleteBatch(12, 24); err != nil {
		t.Fatalf("batch 2: %v", err)
	}
	// Third batch: 36 of the original 60 would be gone - over half.
	if err := deleteBatch(24, 36); !errors.Is(err, reconcile.ErrDeleteLimit) {
		t.Fatalf("batch 3: got %v, want ErrDeleteLimit", err)
	}
	if got := countFiles(t, remote); got != 36 {
		t.Fatalf("remote has %d files, want 36 (two batches applied, the third refused)", got)
	}
	if len(refused) != 1 || refused[0].Remote != 12 || refused[0].Tracked != 60 || len(refused[0].SampleRemote) != 12 {
		t.Fatalf("refusal record: %+v", refused)
	}

	// Approving that plan lets it through and clears the window, so an
	// ordinary delete afterwards is not held against the approved one.
	p.ApproveDeletes = func(fp string) bool { return fp == refused[0].Fingerprint }
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatalf("approved cycle: %v", err)
	}
	p.ApproveDeletes = nil
	if got := countFiles(t, remote); got != 24 {
		t.Fatalf("remote has %d files after approval, want 24", got)
	}
	if err := deleteBatch(36, 40); err != nil {
		t.Fatalf("ordinary delete after an approved one: %v", err)
	}
}

func TestDeleteWindowForgetsOldDeletions(t *testing.T) {
	w := NewDeleteWindow(10 * time.Minute)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	w.Add(5, 0)
	now = now.Add(6 * time.Minute)
	w.Add(0, 7)
	if l, r := w.Recent(); l != 5 || r != 7 {
		t.Fatalf("Recent = %d, %d; want 5, 7", l, r)
	}
	now = now.Add(5 * time.Minute) // the first is now 11 minutes old
	if l, r := w.Recent(); l != 0 || r != 7 {
		t.Fatalf("Recent = %d, %d; want 0, 7", l, r)
	}
	w.Reset()
	if l, r := w.Recent(); l != 0 || r != 0 {
		t.Fatalf("after Reset: %d, %d", l, r)
	}
	var none *DeleteWindow
	none.Add(1, 1)
	none.Reset()
	if l, r := none.Recent(); l != 0 || r != 0 || none.Span() != 0 {
		t.Fatal("nil window must remember nothing")
	}
}
