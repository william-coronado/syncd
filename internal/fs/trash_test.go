package fs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixTrashClock(t *testing.T, now time.Time) {
	t.Helper()
	prev := trashNow
	trashNow = func() time.Time { return now }
	t.Cleanup(func() { trashNow = prev })
}

func setNetworkFS(t *testing.T, network bool) {
	t.Helper()
	prev := isNetworkFS
	isNetworkFS = func(string) bool { return network }
	t.Cleanup(func() { isNetworkFS = prev })
}

func writeTrashFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTrashRemoveMovesLocalFile(t *testing.T) {
	setNetworkFS(t, false)
	fixTrashClock(t, time.Date(2026, 11, 14, 9, 30, 15, 0, time.UTC))
	root := filepath.Join(t.TempDir(), "proj")
	trashDir := filepath.Join(t.TempDir(), "trash")
	writeTrashFixture(t, filepath.Join(root, "a/b.txt"), "one")

	tr := NewTrash(trashDir)
	if err := tr.Remove(root, "a/b.txt"); err != nil {
		t.Fatal(err)
	}
	if lexists(filepath.Join(root, "a/b.txt")) {
		t.Fatal("original still present")
	}
	trashed := filepath.Join(trashDir, "2026-11-14", "proj", "a", "b.txt")
	if got, err := os.ReadFile(trashed); err != nil || string(got) != "one" {
		t.Fatalf("trashed copy: %q, %v", got, err)
	}

	// The same path deleted again that day must not replace the first copy.
	writeTrashFixture(t, filepath.Join(root, "a/b.txt"), "two")
	if err := tr.Remove(root, "a/b.txt"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(trashed); string(got) != "one" {
		t.Fatalf("first trashed copy was replaced: %q", got)
	}
	if got, err := os.ReadFile(trashed + ".093015"); err != nil || string(got) != "two" {
		t.Fatalf("second trashed copy: %q, %v", got, err)
	}
}

func TestTrashRemoveDeletesOutrightOnNetworkFS(t *testing.T) {
	setNetworkFS(t, true)
	root := t.TempDir()
	trashDir := filepath.Join(t.TempDir(), "trash")
	writeTrashFixture(t, filepath.Join(root, "x.txt"), "x")

	if err := NewTrash(trashDir).Remove(root, "x.txt"); err != nil {
		t.Fatal(err)
	}
	if lexists(filepath.Join(root, "x.txt")) {
		t.Fatal("file not removed")
	}
	if lexists(trashDir) {
		t.Fatal("trash was used for a network filesystem")
	}
}

func TestTrashRemoveNilAndMissing(t *testing.T) {
	setNetworkFS(t, false)
	root := t.TempDir()
	writeTrashFixture(t, filepath.Join(root, "x.txt"), "x")

	var none *Trash
	if err := none.Remove(root, "x.txt"); err != nil {
		t.Fatal(err)
	}
	if lexists(filepath.Join(root, "x.txt")) {
		t.Fatal("nil Trash did not remove the file")
	}
	if err := none.Remove(root, "x.txt"); err != nil {
		t.Fatalf("already-missing path (nil trash): %v", err)
	}
	if err := NewTrash(t.TempDir()).Remove(root, "x.txt"); err != nil {
		t.Fatalf("already-missing path: %v", err)
	}
}

// A path that became a non-empty directory since the scan must fail, not be
// moved (or removed) wholesale.
func TestTrashRemoveRefusesNonEmptyDirectory(t *testing.T) {
	setNetworkFS(t, false)
	root := t.TempDir()
	writeTrashFixture(t, filepath.Join(root, "d/inner.txt"), "keep")

	if err := NewTrash(t.TempDir()).Remove(root, "d"); err == nil {
		t.Fatal("expected an error removing a non-empty directory")
	}
	if !lexists(filepath.Join(root, "d/inner.txt")) {
		t.Fatal("directory contents were removed")
	}
}

func TestTrashRemoveLeavesFileWhenTrashUnwritable(t *testing.T) {
	setNetworkFS(t, false)
	root := t.TempDir()
	writeTrashFixture(t, filepath.Join(root, "x.txt"), "x")
	// A regular file where the trash directory should be.
	blocker := filepath.Join(t.TempDir(), "trash")
	writeTrashFixture(t, blocker, "")

	if err := NewTrash(blocker).Remove(root, "x.txt"); err == nil {
		t.Fatal("expected an error when the trash cannot be written")
	}
	if !lexists(filepath.Join(root, "x.txt")) {
		t.Fatal("file was deleted even though it could not be trashed")
	}
}

func TestPruneTrash(t *testing.T) {
	fixTrashClock(t, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	dir := t.TempDir()
	for _, name := range []string{"2026-10-01", "2026-10-15", "2026-11-13", "not-a-day"} {
		writeTrashFixture(t, filepath.Join(dir, name, "f"), "x")
	}
	writeTrashFixture(t, filepath.Join(dir, "2026-01-01"), "a file, not a day directory")

	removed, err := PruneTrash(dir, 30)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d, want 1", removed)
	}
	for name, want := range map[string]bool{
		"2026-10-01": false, // 44 days old
		"2026-10-15": true,  // exactly 30 days old: kept
		"2026-11-13": true,
		"not-a-day":  true,
		"2026-01-01": true,
	} {
		if got := lexists(filepath.Join(dir, name)); got != want {
			t.Errorf("%s: exists=%v, want %v", name, got, want)
		}
	}

	if n, err := PruneTrash(dir, 0); n != 0 || err != nil {
		t.Fatalf("keepDays 0: %d, %v", n, err)
	}
	if n, err := PruneTrash(filepath.Join(dir, "missing"), 30); n != 0 || err != nil {
		t.Fatalf("missing dir: %d, %v", n, err)
	}
}
