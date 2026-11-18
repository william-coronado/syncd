package app

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	syncfs "syncd/internal/fs"
)

// syncedPhotosPair syncs a local "photos" directory (one file) to the remote
// and runs a second cycle so the directory is in the baseline on both sides,
// then has the other side delete it - as another machine would.
func syncedPhotosPair(t *testing.T, ignoreRules string) (local, remote string, p RobustParams) {
	t.Helper()
	local, remote = t.TempDir(), t.TempDir()
	if ignoreRules != "" {
		if err := os.WriteFile(filepath.Join(local, syncfs.IgnoreFileName), []byte(ignoreRules), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(local, "photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "photos", "a.jpg"), []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	p = newTestRobustParams(t, local, remote)
	p.Trash = syncfs.NewTrash(filepath.Join(t.TempDir(), "state", syncfs.TrashDirName))
	for i := 0; i < 2; i++ {
		if _, failures, err := RunRobustOnce(context.Background(), p); err != nil || failures != 0 {
			t.Fatalf("setup cycle %d: failures=%d err=%v", i, failures, err)
		}
	}
	return local, remote, p
}

func runCyclesExpectingNoFailures(t *testing.T, p RobustParams, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, failures, err := RunRobustOnce(context.Background(), p); err != nil || failures != 0 {
			t.Fatalf("cycle %d: failures=%d err=%v", i, failures, err)
		}
	}
}

// Finder leaves .DS_Store in every folder it opens. A folder deleted on the
// other side that held one (ignored, so never synced) failed to be removed
// with "directory not empty" - an ERROR and a failed action every cycle,
// forever.
func TestRunRobustOnce_RemovesDeletedFolderHoldingOnlyJunk(t *testing.T) {
	local, remote, p := syncedPhotosPair(t, "")
	for _, junk := range []string{".DS_Store", "._a.jpg"} {
		if err := os.WriteFile(filepath.Join(local, "photos", junk), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(filepath.Join(remote, "photos")); err != nil {
		t.Fatal(err)
	}

	runCyclesExpectingNoFailures(t, p, 2)

	if _, err := os.Lstat(filepath.Join(local, "photos")); !os.IsNotExist(err) {
		t.Fatalf("local photos/ still present (err=%v)", err)
	}
	if _, err := os.Lstat(filepath.Join(remote, "photos")); !os.IsNotExist(err) {
		t.Fatalf("remote photos/ was recreated (err=%v)", err)
	}
	// The junk went through the trash like any other sync-driven delete.
	trashed := filesUnder(t, p.Trash.Dir)
	for _, junk := range []string{".DS_Store", "._a.jpg"} {
		if !trashed[junk] {
			t.Errorf("%s not in the trash; trash holds %v", junk, trashed)
		}
	}
}

// A user-ignored file is local-only data: it must never be deleted to make
// room for a directory removal. The folder stays, quietly - no failure
// every cycle, and no recreating it on the side that deleted it.
func TestRunRobustOnce_KeepsDeletedFolderHoldingUserIgnoredFile(t *testing.T) {
	local, remote, p := syncedPhotosPair(t, "*.secret\n")
	if err := os.WriteFile(filepath.Join(local, "photos", "keep.secret"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "photos", ".DS_Store"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(remote, "photos")); err != nil {
		t.Fatal(err)
	}

	runCyclesExpectingNoFailures(t, p, 3)

	if got := mustReadFile(t, filepath.Join(local, "photos", "keep.secret")); got != "mine" {
		t.Fatalf("ignored file changed: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(local, "photos", ".DS_Store")); err != nil {
		t.Fatalf("junk beside a kept file should be left alone: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(remote, "photos")); !os.IsNotExist(err) {
		t.Fatalf("remote photos/ was recreated (err=%v)", err)
	}
}

func filesUnder(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out[d.Name()] = true
		}
		return nil
	})
	return out
}
