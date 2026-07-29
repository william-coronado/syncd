package fs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanPathsOnlyRequestedExistingFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("b"), 0o644)
	m, _ := LoadIgnoreMatcher(dir)
	// Request a.txt (exists), sub/b.txt (exists), gone.txt (missing), sub (a dir).
	req := map[string]struct{}{"a.txt": {}, "sub/b.txt": {}, "gone.txt": {}, "sub": {}}
	res, err := ScanPaths(dir, req, nil, HashAlways, false, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("want 2 files, got %d: %v", len(res.Files), res.Files)
	}
	if res.Files["a.txt"].Hash == "" || res.Files["sub/b.txt"].Hash == "" {
		t.Error("HashAlways should populate hashes for existing files")
	}
	if _, ok := res.Files["gone.txt"]; ok {
		t.Error("missing path must be absent from Files")
	}
	if _, ok := res.Files["sub"]; ok {
		t.Error("directory path must not be scanned as a file")
	}
}
