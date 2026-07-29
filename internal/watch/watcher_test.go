package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"syncd/internal/fs"
)

func TestWatcherCoalescesEventsIntoBatch(t *testing.T) {
	root := t.TempDir()
	m, _ := fs.LoadIgnoreMatcher(root)
	w, err := NewWatcher(root, m, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	os.WriteFile(filepath.Join(root, "a.txt"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("2"), 0o644)

	select {
	case batch := <-w.Batches():
		if _, ok := batch["a.txt"]; !ok {
			t.Error("missing a.txt in batch")
		}
		if _, ok := batch["b.txt"]; !ok {
			t.Error("missing b.txt in batch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no batch emitted")
	}
}

func TestWatcherDropsIgnoredPaths(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".syncdignore"), []byte("*.tmp\n"), 0o644)
	m, _ := fs.LoadIgnoreMatcher(root)
	w, _ := NewWatcher(root, m, 80*time.Millisecond)
	defer w.Close()
	os.WriteFile(filepath.Join(root, "x.tmp"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "keep.txt"), []byte("k"), 0o644)
	batch := <-w.Batches()
	if _, ok := batch["x.tmp"]; ok {
		t.Error("ignored *.tmp leaked into batch")
	}
	if _, ok := batch["keep.txt"]; !ok {
		t.Error("keep.txt missing")
	}
}
