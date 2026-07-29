package fs

import (
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
