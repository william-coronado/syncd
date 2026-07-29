package execute

import (
	"context"
	"os"
	"path/filepath"
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
