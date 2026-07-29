package execute

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
