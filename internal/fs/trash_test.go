package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	trashed := filepath.Join(trashDir, "2026-11-14", RootFolder(root), "a", "b.txt")
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

// The trash is shared across processes: many concurrent deletes of the
// same relative path (from roots that even share a base name) must each
// keep their own copy.
func TestTrashRemoveConcurrentDeletesKeepEveryCopy(t *testing.T) {
	setNetworkFS(t, false)
	fixTrashClock(t, time.Date(2026, 11, 14, 9, 30, 15, 0, time.UTC))
	trashDir := filepath.Join(t.TempDir(), "trash")
	const n = 60
	base := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		// Half the deletes share one root; the rest use distinct roots with
		// the same base name. Each gets its own Trash, as separate processes would.
		root := filepath.Join(base, "shared", "proj")
		if i%2 == 1 {
			root = filepath.Join(base, fmt.Sprint(i), "proj")
		}
		writeTrashFixture(t, filepath.Join(root, fmt.Sprintf("src%d", i), "same.txt"), fmt.Sprint(i))
		// Move it to the one relative path every delete uses.
		if err := os.MkdirAll(filepath.Join(root, "d"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex // serializes only the fixture rename, not the Remove
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			root := filepath.Join(base, "shared", "proj")
			if i%2 == 1 {
				root = filepath.Join(base, fmt.Sprint(i), "proj")
			}
			rel := fmt.Sprintf("src%d/same.txt", i)
			if i%2 == 1 {
				rel = "d/same.txt"
				mu.Lock()
				err := os.Rename(filepath.Join(root, fmt.Sprintf("src%d", i), "same.txt"), filepath.Join(root, "d", "same.txt"))
				mu.Unlock()
				if err != nil {
					errs <- err
					return
				}
			}
			errs <- NewTrash(trashDir).Remove(root, rel)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]bool)
	_ = filepath.WalkDir(trashDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			seen[string(b)] = true
		}
		return nil
	})
	if len(seen) != n {
		t.Fatalf("trash holds %d distinct files, want %d", len(seen), n)
	}
}

// Same root, same path, same instant: the reservation, not luck, must keep
// the copies apart.
func TestTrashReserveIsExclusive(t *testing.T) {
	fixTrashClock(t, time.Date(2026, 11, 14, 9, 30, 15, 0, time.UTC))
	tr := NewTrash(filepath.Join(t.TempDir(), "trash"))
	const n = 50
	got := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := tr.reserve("/some/root", "a/b.txt")
			if err != nil {
				t.Error(err)
			}
			got <- p
		}()
	}
	wg.Wait()
	close(got)
	seen := make(map[string]bool)
	for p := range got {
		if seen[p] {
			t.Fatalf("destination %s handed out twice", p)
		}
		seen[p] = true
	}
}

func TestTrashIsPrivateAndPerRoot(t *testing.T) {
	setNetworkFS(t, false)
	trashDir := filepath.Join(t.TempDir(), "trash")
	rootA := filepath.Join(t.TempDir(), "dev")
	rootB := filepath.Join(t.TempDir(), "dev")
	// A file in one root, a directory of the same name in the other: with a
	// shared folder the second delete could not create its parent.
	writeTrashFixture(t, filepath.Join(rootA, "notes"), "a file")
	writeTrashFixture(t, filepath.Join(rootB, "notes", "inner.txt"), "in a dir")
	tr := NewTrash(trashDir)
	if err := tr.Remove(rootA, "notes"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Remove(rootB, "notes/inner.txt"); err != nil {
		t.Fatal(err)
	}
	if RootFolder(rootA) == RootFolder(rootB) {
		t.Fatal("two roots with the same base name share a trash folder")
	}
	_ = filepath.WalkDir(trashDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		info, _ := d.Info()
		if perm := info.Mode().Perm(); perm&0o077 != 0 && d.IsDir() {
			t.Errorf("%s is accessible to others (%o)", p, perm)
		}
		return nil
	})
}

func TestTrashPreserveKeepsOriginalAndCopies(t *testing.T) {
	setNetworkFS(t, true) // even on a network filesystem
	root := filepath.Join(t.TempDir(), "share")
	trashDir := filepath.Join(t.TempDir(), "trash")
	writeTrashFixture(t, filepath.Join(root, "a.txt"), "old version")
	tr := NewTrash(trashDir)
	if err := tr.Preserve(root, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(b) != "old version" {
		t.Fatal("Preserve disturbed the original")
	}
	kept := filepath.Join(trashDir, trashNow().Format(trashDayLayout), RootFolder(root), "a.txt")
	if b, err := os.ReadFile(kept); err != nil || string(b) != "old version" {
		t.Fatalf("kept copy: %q, %v", b, err)
	}
	if err := tr.Preserve(root, "missing.txt"); err != nil {
		t.Fatalf("missing source: %v", err)
	}
	var none *Trash
	if err := none.Preserve(root, "a.txt"); err != nil {
		t.Fatalf("nil trash: %v", err)
	}
}

// A root may contain a mount of another kind: classify the file's own
// directory.
func TestTrashClassifiesTheFilesDirectory(t *testing.T) {
	root := t.TempDir()
	prev := isNetworkFS
	isNetworkFS = func(dir string) bool { return filepath.Base(dir) == "nas" }
	t.Cleanup(func() { isNetworkFS = prev })
	trashDir := filepath.Join(t.TempDir(), "trash")
	writeTrashFixture(t, filepath.Join(root, "local.txt"), "l")
	writeTrashFixture(t, filepath.Join(root, "nas", "remote.txt"), "r")
	tr := NewTrash(trashDir)
	if err := tr.Remove(root, "local.txt"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Remove(root, "nas/remote.txt"); err != nil {
		t.Fatal(err)
	}
	day := filepath.Join(trashDir, trashNow().Format(trashDayLayout), RootFolder(root))
	if !lexists(filepath.Join(day, "local.txt")) {
		t.Error("file on the local part of the root was not trashed")
	}
	if lexists(filepath.Join(day, "nas", "remote.txt")) {
		t.Error("file on the nested network mount was trashed")
	}
}
