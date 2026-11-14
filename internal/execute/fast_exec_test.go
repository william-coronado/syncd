package execute

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"syncd/internal/reconcile"
)

func TestRunFastTasksCopyAndDelete(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(right, "stale.txt"), []byte("x"), 0o644)
	tasks := []reconcile.FastTask{
		{Type: reconcile.CopyL2R, RelPath: "a.txt", Size: 2},
		{Type: reconcile.DeleteR, RelPath: "stale.txt"},
	}
	if err := RunFastTasks(context.Background(), 2, left, right, tasks, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(right, "a.txt")); string(b) != "hi" {
		t.Error("a.txt not copied to right")
	}
	if _, err := os.Stat(filepath.Join(right, "stale.txt")); !os.IsNotExist(err) {
		t.Error("stale.txt not deleted")
	}
}

// TestApplyFastTask_DeleteFailsSafelyWhenPathIsActuallyANonEmptyDirectory is
// a regression test for a review finding (Gemini 3.6): DeleteL/DeleteR used
// os.RemoveAll, whereas the robust engine's worker_pool.go uses a bare
// os.Remove (via removeIfExists) for the identical delete case. FastTask
// deletions are only ever queued for paths FastCompare saw as FILES during
// its scan - if that path was replaced by a directory before this task
// executes (a TOCTOU race: scan, then something else repopulates the path
// as a directory before the executor gets to it), os.RemoveAll would
// recursively wipe the entire subtree instead of failing safely. os.Remove
// refuses (ENOTEMPTY) instead, matching the project's existing safety
// convention.
func TestApplyFastTask_DeleteFailsSafelyWhenPathIsActuallyANonEmptyDirectory(t *testing.T) {
	left := t.TempDir()
	dir := filepath.Join(left, "was-a-file")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unexpected.txt"), []byte("do not delete me"), 0o644); err != nil {
		t.Fatalf("write unexpected.txt: %v", err)
	}

	err := applyFastTask(left, "", reconcile.FastTask{Type: reconcile.DeleteL, RelPath: "was-a-file"}, nil)
	if err == nil {
		t.Fatal("expected an error deleting a non-empty directory where a file was expected, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "unexpected.txt")); statErr != nil {
		t.Fatalf("expected unexpected.txt to survive - the whole subtree must not be wiped: stat err = %v", statErr)
	}
}

// TestApplyFastTask_MoveFallbackRemoveFailsSafelyWhenSourceIsNonEmptyDirectory
// is a regression test for the os.RemoveAll → os.Remove fix in moveFastFile.
// The copy+remove fallback (used when os.Rename fails across devices) must
// use os.Remove, not os.RemoveAll - the same safety convention enforced for
// DeleteL/DeleteR tasks (see TestApplyFastTask_DeleteFailsSafelyWhenPathIsActuallyANonEmptyDirectory).
//
// The real race this guards against: src is a regular file when FastCompare
// scans it, but gets replaced by a non-empty directory in the window between
// the fallback's copy succeeding and moveFastFile's os.Remove(src) call. A
// src that is already a directory at the top of moveFastFile can't reach
// Remove at all - AtomicCopyFile's own directory guard rejects it first - so
// this test uses copyFunc, a package-level indirection, to simulate the copy
// succeeding and *then* src being swapped for a directory, landing exactly in
// the window os.Remove (not os.RemoveAll) protects.
func TestApplyFastTask_MoveFallbackRemoveFailsSafelyWhenSourceIsNonEmptyDirectory(t *testing.T) {
	root := t.TempDir()

	// Source starts as the regular file FastCompare actually saw.
	src := filepath.Join(root, "old")
	if err := os.WriteFile(src, []byte("hi"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	// Force the Rename→fallback path by making the rename always fail.
	origRename := renameFunc
	renameFunc = func(_, _ string) error {
		return fmt.Errorf("simulated cross-device rename failure")
	}
	defer func() { renameFunc = origRename }()

	// Simulate the copy succeeding, then the TOCTOU race replacing src with a
	// non-empty directory before moveFastFile's os.Remove(src) runs.
	origCopy := copyFunc
	copyFunc = func(srcPath, dstPath string) error {
		if err := os.WriteFile(dstPath, []byte("hi"), 0o644); err != nil {
			return err
		}
		if err := os.Remove(srcPath); err != nil {
			return err
		}
		if err := os.MkdirAll(srcPath, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(srcPath, "precious.txt"), []byte("must survive"), 0o644)
	}
	defer func() { copyFunc = origCopy }()

	err := applyFastTask(root, root, reconcile.FastTask{
		Type: reconcile.MoveR, RelPath: "new/moved", OldRelPath: "old",
	}, nil)
	if err == nil {
		t.Fatal("expected an error removing the non-empty directory left behind by the TOCTOU race, got nil")
	}

	// The replaced source directory's contents must be intact - os.RemoveAll
	// would wipe the whole subtree; os.Remove refuses (ENOTEMPTY) instead.
	if _, statErr := os.Stat(filepath.Join(src, "precious.txt")); statErr != nil {
		t.Fatalf("source directory contents were wiped on failed move fallback: stat err = %v", statErr)
	}

	// The destination should hold the content the (simulated) copy step
	// wrote before the race window - a duplicate, not data loss, which is
	// the expected outcome once the copy itself has already succeeded.
	dst := filepath.Join(root, "new", "moved")
	if b, readErr := os.ReadFile(dst); readErr != nil || string(b) != "hi" {
		t.Fatalf("destination should contain the copied content: content=%q err=%v", b, readErr)
	}
}

// TestRunFastTasksWorkersClampedToMinimumOne is a regression test mirroring
// TestRunActions_WorkersClampedToMinimumOne in worker_pool_test.go: with
// workers == 0, no worker goroutines start, so RunFastTasks must clamp to a
// minimum of 1 instead of hanging (feeder blocked on the unbuffered jobs
// channel) or silently completing without doing the copy.
func TestRunFastTasksWorkersClampedToMinimumOne(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644)
	tasks := []reconcile.FastTask{{Type: reconcile.CopyL2R, RelPath: "a.txt", Size: 2}}

	done := make(chan error, 1)
	go func() {
		done <- RunFastTasks(context.Background(), 0, left, right, tasks, nil)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunFastTasks returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunFastTasks(workers=0) did not return within 5s - feeder goroutine leaked")
	}

	if b, err := os.ReadFile(filepath.Join(right, "a.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("copy did not happen with workers=0: err=%v content=%q", err, b)
	}
}

func TestRunFastTasksMove(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(right, "old"), 0o755)
	os.WriteFile(filepath.Join(right, "old", "f.txt"), []byte("z"), 0o644)
	tasks := []reconcile.FastTask{{Type: reconcile.MoveR, RelPath: "new/f.txt", OldRelPath: "old/f.txt"}}
	if err := RunFastTasks(context.Background(), 1, left, right, tasks, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(right, "new", "f.txt")); string(b) != "z" {
		t.Error("file not moved to new path")
	}
}

// TestRunFastTasksUnknownTaskTypeErrors guards against applyFastTask's
// default case silently succeeding (returning nil) on an unrecognized
// FastTaskType, mirroring applyAction's "unknown action type" handling in
// worker_pool.go - an unknown task should surface as a failure, not be
// dropped on the floor as a silent no-op.
func TestRunFastTasksUnknownTaskTypeErrors(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	tasks := []reconcile.FastTask{{Type: reconcile.FastTaskType("Bogus"), RelPath: "a.txt"}}
	err := RunFastTasks(context.Background(), 1, left, right, tasks, nil)
	if err == nil {
		t.Fatal("expected RunFastTasks to error on an unknown FastTaskType, got nil")
	}
}

// recordingSink captures ProgressSink calls so tests can verify RunFastTasks
// drives progress by task count (not raw bytes into a fixed-100 bar) and
// reports the correct cumulative byte total. Advance is called from multiple
// worker goroutines concurrently, so its state is mutex-guarded.
type recordingSink struct {
	mu         sync.Mutex
	startTotal int
	advances   []int64
	done       bool
}

func (r *recordingSink) Start(total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startTotal = total
}

func (r *recordingSink) Advance(bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.advances = append(r.advances, bytes)
}

func (r *recordingSink) Done() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
}

// TestRunFastTasksReportsProgressPerTask verifies the fix for the frozen
// "[000/100] 0%" bar: Start is called once with the total TASK count (so the
// bar has a real denominator), Advance fires exactly once per completed task
// (so the bar ticks per file instead of dumping raw bytes into a 100-unit
// bar), the cumulative bytes equal the sum of copied sizes (deletes add 0),
// and Done is called once.
func TestRunFastTasksReportsProgressPerTask(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(left, "a.txt"), []byte("hello"), 0o644) // 5 bytes
	os.WriteFile(filepath.Join(left, "b.txt"), []byte("hi"), 0o644)    // 2 bytes
	os.WriteFile(filepath.Join(right, "gone.txt"), []byte("x"), 0o644)
	tasks := []reconcile.FastTask{
		{Type: reconcile.CopyL2R, RelPath: "a.txt", Size: 5},
		{Type: reconcile.CopyL2R, RelPath: "b.txt", Size: 2},
		{Type: reconcile.DeleteR, RelPath: "gone.txt"},
	}
	sink := &recordingSink{}
	if err := RunFastTasks(context.Background(), 2, left, right, tasks, sink); err != nil {
		t.Fatal(err)
	}
	if sink.startTotal != 3 {
		t.Errorf("Start total = %d, want 3 (task count)", sink.startTotal)
	}
	if len(sink.advances) != 3 {
		t.Fatalf("Advance called %d times, want 3 (once per completed task)", len(sink.advances))
	}
	var sum int64
	for _, b := range sink.advances {
		sum += b
	}
	if sum != 7 { // 5 + 2 copied; delete contributes 0
		t.Errorf("cumulative advanced bytes = %d, want 7", sum)
	}
	if !sink.done {
		t.Error("Done was not called")
	}
}
