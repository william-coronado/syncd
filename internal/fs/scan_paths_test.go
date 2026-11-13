package fs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
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

// --- Task 3a: ScanPaths gets the same per-entry treatment as ScanTree ---

func TestScanPaths_SkipsDirSymlinkAndRecordsIssue(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "good.txt"), []byte("good"), 0o644)
	os.MkdirAll(filepath.Join(dir, "realdir"), 0o755)
	if err := os.Symlink(filepath.Join(dir, "realdir"), filepath.Join(dir, "dlink")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	m, _ := LoadIgnoreMatcher(dir)
	req := map[string]struct{}{"good.txt": {}, "dlink": {}}

	res, err := ScanPaths(dir, req, nil, HashAlways, false, m)
	if err != nil {
		t.Fatalf("ScanPaths returned error for a dir-symlink, want success: %v", err)
	}
	if _, ok := res.Files["good.txt"]; !ok {
		t.Errorf("expected good.txt to be scanned, got %+v", res.Files)
	}
	if _, ok := res.Files["dlink"]; ok {
		t.Errorf("dir-symlink must not appear in Files, got %+v", res.Files)
	}
	if _, ok := res.Skipped["dlink"]; !ok {
		t.Errorf("expected dlink to be recorded as skipped, got %+v", res.Skipped)
	}
	if len(res.Issues) != 1 || res.Issues[0].Path != "dlink" || res.Issues[0].Reason != "symlink not supported" {
		t.Errorf("expected one issue for dlink with reason %q, got %+v", "symlink not supported", res.Issues)
	}
}

func TestScanPaths_SkipsBrokenSymlinkAndRecordsIssue(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "good.txt"), []byte("good"), 0o644)
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), filepath.Join(dir, "blink")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	m, _ := LoadIgnoreMatcher(dir)
	req := map[string]struct{}{"good.txt": {}, "blink": {}}

	res, err := ScanPaths(dir, req, nil, HashAlways, false, m)
	if err != nil {
		t.Fatalf("ScanPaths returned error for a broken symlink, want success: %v", err)
	}
	if _, ok := res.Files["blink"]; ok {
		t.Errorf("broken symlink must not appear in Files, got %+v", res.Files)
	}
	if _, ok := res.Skipped["blink"]; !ok {
		t.Errorf("expected blink to be recorded as skipped, got %+v", res.Skipped)
	}
}

func TestScanPaths_SkipsNonRegularFileAndRecordsIssue(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "good.txt"), []byte("good"), 0o644)
	fifoPath := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifoPath, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	m, _ := LoadIgnoreMatcher(dir)
	req := map[string]struct{}{"good.txt": {}, "fifo": {}}

	res, err := ScanPaths(dir, req, nil, HashAlways, false, m)
	if err != nil {
		t.Fatalf("ScanPaths returned error for a FIFO, want success: %v", err)
	}
	if _, ok := res.Files["fifo"]; ok {
		t.Errorf("FIFO must not appear in Files, got %+v", res.Files)
	}
	if _, ok := res.Skipped["fifo"]; !ok {
		t.Errorf("expected fifo to be recorded as skipped, got %+v", res.Skipped)
	}
}

func TestScanPaths_SkipsUnreadableFileAndRecordsIssue(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses file permission checks")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "good.txt"), []byte("good"), 0o644)
	badPath := filepath.Join(dir, "unreadable.txt")
	os.WriteFile(badPath, []byte("secret"), 0o644)
	if err := os.Chmod(badPath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(badPath, 0o644) })
	m, _ := LoadIgnoreMatcher(dir)
	req := map[string]struct{}{"good.txt": {}, "unreadable.txt": {}}

	res, err := ScanPaths(dir, req, nil, HashAlways, false, m)
	if err != nil {
		t.Fatalf("ScanPaths returned error for an unreadable file, want success: %v", err)
	}
	if _, ok := res.Files["unreadable.txt"]; ok {
		t.Errorf("unreadable file must not appear in Files, got %+v", res.Files)
	}
	if _, ok := res.Skipped["unreadable.txt"]; !ok {
		t.Errorf("expected unreadable.txt to be recorded as skipped, got %+v", res.Skipped)
	}
}

// A directory-only ignore rule ("build/") must exclude files beneath that
// directory from a scoped scan exactly as it does from a full ScanTree walk
// (which never descends into the ignored directory at all).
func TestScanPaths_HonorsIgnoredAncestorDirectory(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"build", "src"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, ignoreFileName), "build/\n")
	writeFile(t, filepath.Join(root, "build", "out.bin"), "artifact")
	writeFile(t, filepath.Join(root, "src", "main.go"), "package main")
	matcher := newTestMatcher(t, root)

	full, err := ScanTree(context.Background(), root, nil, true, matcher, HashNever, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := full.Files["build/out.bin"]; ok {
		t.Fatal("precondition: full scan should ignore build/out.bin")
	}

	scoped, err := ScanPaths(root, map[string]struct{}{"build/out.bin": {}, "src/main.go": {}}, nil, HashNever, true, matcher)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := scoped.Files["build/out.bin"]; ok {
		t.Fatal("scoped scan synced a file under an ignored directory")
	}
	if _, ok := scoped.Files["src/main.go"]; !ok {
		t.Fatal("scoped scan dropped a non-ignored file")
	}
}

// ScanSubtree reports root-relative paths and applies anchored ignore rules
// against them, so its result merges with a full scan of the same root.
func TestScanSubtree_RootRelativePathsAndAnchoredIgnores(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"proj/src", "proj/out", "other"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, ignoreFileName), "/proj/out\n")
	writeFile(t, filepath.Join(root, "proj", "src", "a.go"), "a")
	writeFile(t, filepath.Join(root, "proj", "out", "bin"), "b")
	writeFile(t, filepath.Join(root, "other", "c.txt"), "c")
	matcher := newTestMatcher(t, root)

	res, err := ScanSubtree(context.Background(), root, "proj", nil, true, matcher, HashNever)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Files["proj/src/a.go"]; !ok {
		t.Fatalf("want root-relative proj/src/a.go, got %v", res.Files)
	}
	if _, ok := res.Files["proj/out/bin"]; ok {
		t.Fatal("anchored ignore /proj/out not applied in subtree scan")
	}
	if _, ok := res.Files["other/c.txt"]; ok {
		t.Fatal("subtree scan escaped its subtree")
	}
	if _, ok := res.Dirs["proj"]; !ok {
		t.Fatalf("subtree root itself should be in Dirs: %v", res.Dirs)
	}
}
