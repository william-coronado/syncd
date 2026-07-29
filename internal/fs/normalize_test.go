package fs

import (
	"path/filepath"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestNormalizeRelativePath_NFDInputBecomesNFC(t *testing.T) {
	// "café.txt" spelled with a combining acute accent (NFD form), as macOS/HFS+
	// would hand back from a directory walk.
	nfd := norm.NFD.String("café.txt")
	if nfd == norm.NFC.String("café.txt") {
		t.Fatal("test fixture is not actually NFD-distinct from NFC; adjust fixture")
	}

	root := "/sync/root"
	abs := filepath.Join(root, nfd)

	got, err := NormalizeRelativePath(root, abs)
	if err != nil {
		t.Fatalf("NormalizeRelativePath returned error: %v", err)
	}

	want := norm.NFC.String("café.txt")
	if got != want {
		t.Fatalf("NormalizeRelativePath() = %q (bytes %x), want %q (bytes %x)", got, got, want, want)
	}
}

func TestNormalizeRelativePath_NestedDirectories(t *testing.T) {
	root := "/sync/root"
	abs := filepath.Join(root, "a", "b", "c.txt")

	got, err := NormalizeRelativePath(root, abs)
	if err != nil {
		t.Fatalf("NormalizeRelativePath returned error: %v", err)
	}
	if got != "a/b/c.txt" {
		t.Fatalf("NormalizeRelativePath() = %q, want %q", got, "a/b/c.txt")
	}
}

func TestNormalizeRelativePath_NoDotSlashPrefix(t *testing.T) {
	root := "/sync/root"
	abs := filepath.Join(root, "top.txt")

	got, err := NormalizeRelativePath(root, abs)
	if err != nil {
		t.Fatalf("NormalizeRelativePath returned error: %v", err)
	}
	if got != "top.txt" {
		t.Fatalf("NormalizeRelativePath() = %q, want no leading './' prefix", got)
	}
}
