package execute

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"syncd/internal/fs"
)

// checkObserved verifies content on its first call only; later calls (the
// re-checks fs.CopyChecked makes on the same held entry) compare metadata.
func TestCheckObservedHashesOnlyOnFirstCall(t *testing.T) {
	root := t.TempDir()
	mtime := time.Unix(1700000000, 0)
	writeAt(t, filepath.Join(root, "f"), "aaaa", mtime)
	scanned := scannedEntry(t, root, "f")
	scanned.Hash, _ = fs.FileBLAKE3(filepath.Join(root, "f"))
	tree, err := fs.OpenTree(root)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if scanned.Info, err = tree.Lstat("f"); err != nil { // as a scan records it
		t.Fatal(err)
	}

	// A same-size, same-mtime content edit is caught by the first call.
	edit := func(body string, at time.Time) {
		t.Helper()
		f, err := os.OpenFile(filepath.Join(root, "f"), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if err := os.Chtimes(filepath.Join(root, "f"), at, at); err != nil {
			t.Fatal(err)
		}
	}
	edit("bbbb", mtime)
	if err := checkObserved(scanned)(tree, "f"); !errors.Is(err, ErrDestinationChanged) {
		t.Fatalf("first call missed a content edit: %v", err)
	}

	edit("aaaa", mtime)
	check := checkObserved(scanned)
	if err := check(tree, "f"); err != nil {
		t.Fatalf("first call on the scanned content: %v", err)
	}
	// Later calls: metadata only, so a write that moves mtime is caught...
	edit("cccc", mtime.Add(time.Second))
	if err := check(tree, "f"); !errors.Is(err, ErrDestinationChanged) {
		t.Fatalf("re-check missed an mtime change: %v", err)
	}
	// ...and the content is not read again.
	edit("dddd", mtime)
	if err := check(tree, "f"); err != nil {
		t.Fatalf("re-check re-read content: %v", err)
	}
}
