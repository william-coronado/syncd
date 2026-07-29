package fs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"syncd/internal/db"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newTestMatcher(t *testing.T, localRoot string) *IgnoreMatcher {
	t.Helper()
	matcher, err := LoadIgnoreMatcher(localRoot)
	if err != nil {
		t.Fatalf("LoadIgnoreMatcher: %v", err)
	}
	return matcher
}

func TestScanTree_RecordsDirectoriesIncludingEmptyOnes(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "empty", "nested"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "withfile"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(root, "withfile", "a.txt"), "content")

	result, err := ScanTree(context.Background(), root, nil, true, newTestMatcher(t, root), HashIfChanged, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error: %v", err)
	}

	for _, want := range []string{"empty", "empty/nested", "withfile"} {
		if _, ok := result.Dirs[want]; !ok {
			t.Errorf("expected directory %q to be recorded, got %+v", want, result.Dirs)
		}
	}
	if _, ok := result.Dirs["."]; ok {
		t.Fatal("root itself should not be recorded as a directory entry")
	}
}

func TestScanTree_SkipsSMBForbiddenCharacters(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "good.txt"), "ok")
	writeFile(t, filepath.Join(root, "bad?name.txt"), "not ok")

	result, err := ScanTree(context.Background(), root, nil, true, newTestMatcher(t, root), HashIfChanged, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error: %v", err)
	}

	if _, ok := result.Files["good.txt"]; !ok {
		t.Fatalf("expected good.txt to be scanned, got files %+v", result.Files)
	}
	if _, ok := result.Files["bad?name.txt"]; ok {
		t.Fatalf("expected bad?name.txt to be excluded from scanned files")
	}
	if _, ok := result.Skipped["bad?name.txt"]; !ok {
		t.Fatalf("expected bad?name.txt to be recorded as skipped")
	}
	if len(result.Issues) != 1 || result.Issues[0].Path != "bad?name.txt" {
		t.Fatalf("expected one issue for bad?name.txt, got %+v", result.Issues)
	}
}

func TestScanTree_DoesNotValidateSMBNamesWhenDisabled(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "bad?name.txt"), "content")

	result, err := ScanTree(context.Background(), root, nil, false, newTestMatcher(t, root), HashIfChanged, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error: %v", err)
	}
	if _, ok := result.Files["bad?name.txt"]; !ok {
		t.Fatalf("expected bad?name.txt to be scanned when validation disabled, got %+v", result.Files)
	}
	if len(result.Issues) != 0 {
		t.Fatalf("expected no issues when validation disabled, got %+v", result.Issues)
	}
}

func TestScanTree_ExcludesSynclockAndTmpFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".synclock"), "canary")
	writeFile(t, filepath.Join(root, "partial.tmp"), "leftover")
	writeFile(t, filepath.Join(root, "real.txt"), "content")

	result, err := ScanTree(context.Background(), root, nil, true, newTestMatcher(t, root), HashIfChanged, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error: %v", err)
	}
	if _, ok := result.Files[".synclock"]; ok {
		t.Fatalf(".synclock should never appear in scanned files")
	}
	if _, ok := result.Files["partial.tmp"]; ok {
		t.Fatalf("*.tmp files should never appear in scanned files")
	}
	if len(result.Files) != 1 {
		t.Fatalf("expected exactly one scanned file, got %+v", result.Files)
	}
	if len(result.Issues) != 0 {
		t.Fatalf(".synclock and *.tmp should be silently excluded, not reported as issues, got %+v", result.Issues)
	}
}

func TestScanTree_PrunesDefaultIgnoredDirectoriesEntirely(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep.txt"), "kept")

	if err := os.MkdirAll(filepath.Join(root, "#recycle", "deleted-item"), 0o755); err != nil {
		t.Fatalf("mkdir #recycle: %v", err)
	}
	writeFile(t, filepath.Join(root, "#recycle", "deleted-item", "gone.txt"), "must never be scanned")

	if err := os.MkdirAll(filepath.Join(root, ".venv", "lib"), 0o755); err != nil {
		t.Fatalf("mkdir .venv: %v", err)
	}
	writeFile(t, filepath.Join(root, ".venv", "lib", "site.py"), "must never be scanned")

	result, err := ScanTree(context.Background(), root, nil, true, newTestMatcher(t, root), HashIfChanged, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error: %v", err)
	}
	if len(result.Files) != 1 {
		t.Fatalf("expected only keep.txt to be scanned, got %+v", result.Files)
	}
	if _, ok := result.Files["keep.txt"]; !ok {
		t.Fatalf("expected keep.txt to be scanned, got %+v", result.Files)
	}
	for dir := range result.Dirs {
		t.Errorf("expected no directories recorded under a fully-ignored tree, found %q", dir)
	}
}

func TestScanTree_ReportsFinalProgressWithFileCount(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "a")
	writeFile(t, filepath.Join(root, "b.txt"), "b")
	writeFile(t, filepath.Join(root, "c.txt"), "c")

	var calls []struct {
		scanned int
		done    bool
	}
	progress := func(scanned int, done bool) {
		calls = append(calls, struct {
			scanned int
			done    bool
		}{scanned, done})
	}

	if _, err := ScanTree(context.Background(), root, nil, true, newTestMatcher(t, root), HashIfChanged, progress); err != nil {
		t.Fatalf("ScanTree returned error: %v", err)
	}

	if len(calls) == 0 {
		t.Fatal("expected at least one progress call, got none")
	}
	last := calls[len(calls)-1]
	if !last.done {
		t.Fatalf("expected final progress call to have done=true, got %+v", last)
	}
	if last.scanned != 3 {
		t.Fatalf("expected final progress call to report 3 files scanned, got %d", last.scanned)
	}
	for _, c := range calls[:len(calls)-1] {
		if c.done {
			t.Fatalf("expected done=true only on the final call, got it mid-stream in %+v", calls)
		}
	}
}

func TestScanTree_ReusesManifestHashWhenSizeAndMtimeMatch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	writeFile(t, path, "hello world")

	mtime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	manifest := map[string]db.ManifestRow{
		"file.txt": {
			Path:  "file.txt",
			Size:  info.Size(),
			Mtime: info.ModTime().Unix(),
			Hash:  "sentinel-hash-from-manifest",
		},
	}

	result, err := ScanTree(context.Background(), root, manifest, true, newTestMatcher(t, root), HashIfChanged, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error: %v", err)
	}
	got := result.Files["file.txt"].Hash
	if got != "sentinel-hash-from-manifest" {
		t.Fatalf("expected manifest hash to be reused when size/mtime match, got %q", got)
	}
}

func TestScanTree_RecomputesHashWhenMtimeDiffers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	writeFile(t, path, "hello world")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	manifest := map[string]db.ManifestRow{
		"file.txt": {
			Path:  "file.txt",
			Size:  info.Size(),
			Mtime: info.ModTime().Unix() - 1, // force a mismatch
			Hash:  "sentinel-hash-from-manifest",
		},
	}

	wantHash, err := FileBLAKE3(path)
	if err != nil {
		t.Fatalf("FileBLAKE3: %v", err)
	}

	result, err := ScanTree(context.Background(), root, manifest, true, newTestMatcher(t, root), HashIfChanged, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error: %v", err)
	}
	got := result.Files["file.txt"].Hash
	if got != wantHash {
		t.Fatalf("expected recomputed hash %q on mtime mismatch, got %q", wantHash, got)
	}
	if got == "sentinel-hash-from-manifest" {
		t.Fatalf("scanner incorrectly reused stale manifest hash despite mtime mismatch")
	}
}

func TestScanTreeHashNeverSkipsHashing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := LoadIgnoreMatcher(dir)
	res, err := ScanTree(context.Background(), dir, nil, false, m, HashNever, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := res.Files["a.txt"]
	if !ok {
		t.Fatal("a.txt missing from scan")
	}
	if e.Hash != "" {
		t.Errorf("HashNever must not hash; got %q", e.Hash)
	}
	if e.Size != 5 {
		t.Errorf("size = %d, want 5", e.Size)
	}
}

func TestScanTreeHashAlwaysHashes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := LoadIgnoreMatcher(dir)
	res, err := ScanTree(context.Background(), dir, nil, false, m, HashAlways, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files["a.txt"].Hash == "" {
		t.Error("HashAlways must populate Hash")
	}
}

func TestScanTree_RecomputesHashForSameSecondMtime_RacyMtimeGuard(t *testing.T) {
	// Regression test: a file whose manifest row already has the *current*
	// size and mtime recorded (e.g. because a second edit landed within the
	// same wall-clock second as the first) must still be rehashed. Trusting
	// the cache here would silently miss the second edit's content change.
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	writeFile(t, path, "hello world")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	manifest := map[string]db.ManifestRow{
		"file.txt": {
			Path:  "file.txt",
			Size:  info.Size(),           // matches exactly
			Mtime: info.ModTime().Unix(), // matches exactly, but is "now"
			Hash:  "sentinel-hash-from-manifest",
		},
	}

	wantHash, err := FileBLAKE3(path)
	if err != nil {
		t.Fatalf("FileBLAKE3: %v", err)
	}

	result, err := ScanTree(context.Background(), root, manifest, true, newTestMatcher(t, root), HashIfChanged, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error: %v", err)
	}
	got := result.Files["file.txt"].Hash
	if got != wantHash {
		t.Fatalf("expected recomputed hash %q for same-second mtime, got %q (stale cache reused)", wantHash, got)
	}
}

// --- Task 3a: one bad entry must not abort the whole scan ---

func assertIssueRecorded(t *testing.T, result ScanResult, path, wantReason string) {
	t.Helper()
	if _, ok := result.Skipped[path]; !ok {
		t.Errorf("expected %q to be recorded in Skipped, got %+v", path, result.Skipped)
	}
	for _, iss := range result.Issues {
		if iss.Path == path {
			if wantReason != "" && iss.Reason != wantReason {
				t.Errorf("issue for %q reason = %q, want %q", path, iss.Reason, wantReason)
			}
			return
		}
	}
	t.Errorf("expected an Issue recorded for %q, got %+v", path, result.Issues)
}

func TestScanTree_SkipsDirSymlinkAndRecordsIssue(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "good.txt"), "good")
	targetDir := filepath.Join(root, "realdir")
	if err := os.Mkdir(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(targetDir, "inside.txt"), "inside")
	linkPath := filepath.Join(root, "dlink")
	if err := os.Symlink(targetDir, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	result, err := ScanTree(context.Background(), root, nil, false, newTestMatcher(t, root), HashAlways, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error for a dir-symlink in the tree, want success: %v", err)
	}
	if _, ok := result.Files["good.txt"]; !ok {
		t.Errorf("expected good.txt to be scanned, got %+v", result.Files)
	}
	if _, ok := result.Files["realdir/inside.txt"]; !ok {
		t.Errorf("expected realdir/inside.txt to be scanned, got %+v", result.Files)
	}
	if _, ok := result.Files["dlink"]; ok {
		t.Errorf("dir-symlink must not appear in Files, got %+v", result.Files)
	}
	assertIssueRecorded(t, result, "dlink", "symlink not supported")
}

func TestScanTree_SkipsBrokenSymlinkAndRecordsIssue(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "good.txt"), "good")
	linkPath := filepath.Join(root, "blink")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	result, err := ScanTree(context.Background(), root, nil, false, newTestMatcher(t, root), HashAlways, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error for a broken symlink, want success: %v", err)
	}
	if _, ok := result.Files["good.txt"]; !ok {
		t.Errorf("expected good.txt to be scanned, got %+v", result.Files)
	}
	if _, ok := result.Files["blink"]; ok {
		t.Errorf("broken symlink must not appear in Files, got %+v", result.Files)
	}
	assertIssueRecorded(t, result, "blink", "symlink not supported")
}

func TestScanTree_SkipsUnreadableFileAndRecordsIssue(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses file permission checks")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "good.txt"), "good")
	badPath := filepath.Join(root, "unreadable.txt")
	writeFile(t, badPath, "secret")
	if err := os.Chmod(badPath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(badPath, 0o644) })

	result, err := ScanTree(context.Background(), root, nil, false, newTestMatcher(t, root), HashAlways, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error for an unreadable file, want success: %v", err)
	}
	if _, ok := result.Files["good.txt"]; !ok {
		t.Errorf("expected good.txt to be scanned, got %+v", result.Files)
	}
	if _, ok := result.Files["unreadable.txt"]; ok {
		t.Errorf("unreadable file must not appear in Files, got %+v", result.Files)
	}
	assertIssueRecorded(t, result, "unreadable.txt", "")
}

func TestScanTree_SkipsNonRegularFileAndRecordsIssue(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "good.txt"), "good")
	fifoPath := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifoPath, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	result, err := ScanTree(context.Background(), root, nil, false, newTestMatcher(t, root), HashAlways, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error for a FIFO in the tree, want success: %v", err)
	}
	if _, ok := result.Files["good.txt"]; !ok {
		t.Errorf("expected good.txt to be scanned, got %+v", result.Files)
	}
	if _, ok := result.Files["fifo"]; ok {
		t.Errorf("FIFO must not appear in Files, got %+v", result.Files)
	}
	assertIssueRecorded(t, result, "fifo", "")
}

func TestScanTree_SkipsUnreadableSubdirectoryAndRecordsIssue(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "good.txt"), "good")
	badDir := filepath.Join(root, "locked")
	if err := os.Mkdir(badDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(badDir, "hidden.txt"), "hidden")
	if err := os.Chmod(badDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(badDir, 0o755) })

	result, err := ScanTree(context.Background(), root, nil, false, newTestMatcher(t, root), HashAlways, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error for an unreadable subdirectory, want success: %v", err)
	}
	if _, ok := result.Files["good.txt"]; !ok {
		t.Errorf("expected good.txt to be scanned, got %+v", result.Files)
	}
	if _, ok := result.Files["locked/hidden.txt"]; ok {
		t.Errorf("file inside an unreadable directory must not be scanned, got %+v", result.Files)
	}
	assertIssueRecorded(t, result, "locked", "")
}

// TestScanTree_RootWalkErrorAborts pins the other side of the abort/record
// line: a failure on the root itself (not a per-entry problem somewhere
// inside the tree) must still be a hard error, not a silently-empty
// successful scan. Getting this backwards would hide a vanished SMB mount.
func TestScanTree_RootWalkErrorAborts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := ScanTree(context.Background(), root, nil, false, newTestMatcher(t, root), HashAlways, nil)
	if err == nil {
		t.Fatal("expected an error when the root itself cannot be walked, got nil")
	}
}

// TestScanTree_ContextCancellationAborts pins the same line from the
// cancellation side: ctx.Done() must still abort the whole scan with an
// error, never be swallowed as a per-entry issue.
func TestScanTree_ContextCancellationAborts(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "a")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ScanTree(ctx, root, nil, false, newTestMatcher(t, root), HashAlways, nil)
	if err == nil {
		t.Fatal("expected an error when the context is already cancelled, got nil")
	}
}
