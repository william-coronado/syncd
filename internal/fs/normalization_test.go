package fs

import (
	"os"
	"path/filepath"
	"testing"
)

// Names in the scan's NFC form must reach entries the filesystem stores in
// another Unicode normalization form. Linux (and Samba on Linux) store names
// byte for byte, so an NFD name - common in files that came from a Mac - is
// only reachable by its own bytes. The scanner keys every path in NFC; when
// later lookups used that key verbatim, a tracked NFD file read as deleted.
const (
	nfcCafe = "caf\u00e9"  // e-acute, precomposed
	nfdCafe = "cafe\u0301" // e + combining acute
)

func TestTree_ResolvesFileStoredInOtherNormalizationForm(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, nfdCafe+".txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, err := OpenTree(root)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()

	if _, err := tree.Lstat(nfcCafe + ".txt"); err != nil {
		t.Fatalf("Lstat of NFC spelling: %v", err)
	}
	if _, err := tree.Hash(nfcCafe + ".txt"); err != nil {
		t.Fatalf("Hash of NFC spelling: %v", err)
	}
	if err := tree.Remove(nfcCafe+".txt", false); err != nil {
		t.Fatalf("Remove of NFC spelling: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("file still present after Remove: %v", entries)
	}
}

func TestTree_ResolvesDirectoryStoredInOtherNormalizationForm(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, nfdCafe, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, nfdCafe, "sub", "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, err := OpenTree(root)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()

	if _, err := tree.Lstat(filepath.Join(nfcCafe, "sub", "x.txt")); err != nil {
		t.Fatalf("Lstat through NFC directory spelling: %v", err)
	}
	// Creating beneath the NFC spelling must reuse the existing directory,
	// not create a second one that differs only in normalization.
	if err := tree.MkdirAll(filepath.Join(nfcCafe, "new")); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("want the one existing directory, got %q", names)
	}
	if _, err := os.Stat(filepath.Join(root, entries[0].Name(), "new")); err != nil {
		t.Fatalf("new directory not created inside the existing one: %v", err)
	}
}

func TestTree_SubtreeScanThroughOtherNormalizationForm(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, nfdCafe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, nfdCafe, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ScanSubtree(t.Context(), root, nfcCafe, nil, true, mustMatcher(t, root), HashAlways)
	if err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}
	if _, ok := res.Files[nfcCafe+"/x.txt"]; !ok {
		t.Fatalf("file under NFD directory missing from subtree scan: %v", res.Files)
	}
}

func TestScanPaths_FindsFileStoredInOtherNormalizationForm(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, nfdCafe+".txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ScanPaths(root, map[string]struct{}{nfcCafe + ".txt": {}}, nil, HashAlways, true, mustMatcher(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Files[nfcCafe+".txt"]; !ok {
		t.Fatalf("NFD file not found by its NFC path (would read as deleted): %+v", res)
	}
}

// A few compatibility characters normalize to plain ASCII (KELVIN SIGN to
// "K"), so even an ASCII path can be stored under another spelling.
func TestTree_ResolvesASCIIPathStoredAsCompatibilityCharacter(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "\u212a.txt"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, err := OpenTree(root)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if _, err := tree.Lstat("K.txt"); err != nil {
		t.Fatalf("Lstat of NFC spelling K.txt: %v", err)
	}
}

// Two entries that both normalize to the requested name are the collision
// the scanner refuses; a lookup must not silently pick one of them.
func TestTree_AmbiguousNormalizationVariantsAreNotResolved(t *testing.T) {
	root := t.TempDir()
	nfc := "\u00c5"      // A-ring, precomposed (the NFC form, deliberately absent)
	nfd := "A\u030a"     // A + combining ring
	angstrom := "\u212b" // ANGSTROM SIGN, NFC-normalizes to U+00C5
	for _, name := range []string{nfd, angstrom} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Skipf("filesystem cannot hold both variants: %v", err)
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 2 {
		t.Skip("filesystem folds normalization variants together")
	}
	tree, err := OpenTree(root)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if _, err := tree.Lstat(nfc); !os.IsNotExist(err) {
		t.Fatalf("want not-exist for an ambiguous name, got %v", err)
	}
}

func mustMatcher(t *testing.T, root string) *IgnoreMatcher {
	t.Helper()
	m, err := LoadIgnoreMatcher(root)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
