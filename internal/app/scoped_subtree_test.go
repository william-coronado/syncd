package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mkfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// baselineTree syncs a small tree with a full cycle so the manifest and the
// directory baseline both know about a/ and a/sub/.
func baselineTree(t *testing.T) (local, remote string, p RobustParams) {
	t.Helper()
	local, remote = t.TempDir(), t.TempDir()
	mkfile(t, filepath.Join(local, "a", "x.txt"), "x")
	mkfile(t, filepath.Join(local, "a", "sub", "y.txt"), "y")
	if err := os.MkdirAll(filepath.Join(local, "a", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	p = newTestRobustParams(t, local, remote)
	if _, f, err := RunRobustOnce(context.Background(), p); err != nil || f != 0 {
		t.Fatalf("baseline: failures=%d err=%v", f, err)
	}
	// Two full cycles: the first creates a/empty on the remote, the second
	// sees it on both sides and baselines it.
	if _, f, err := RunRobustOnce(context.Background(), p); err != nil || f != 0 {
		t.Fatalf("baseline 2: failures=%d err=%v", f, err)
	}
	if !exists(filepath.Join(remote, "a", "sub", "y.txt")) || !exists(filepath.Join(remote, "a", "empty")) {
		t.Fatal("precondition: baseline did not reach the remote")
	}
	return local, remote, p
}

// fsnotify reports a directory rename as just its old and new top-level
// paths. The scoped cycle must still move the whole subtree - files and
// empty directories - in that one cycle (docs/follow-ups.md #26).
func TestRunRobustScoped_DirectoryRenamePropagatesWholeSubtree(t *testing.T) {
	local, remote, p := baselineTree(t)
	if err := os.Rename(filepath.Join(local, "a"), filepath.Join(local, "b")); err != nil {
		t.Fatal(err)
	}
	if _, f, err := RunRobustScoped(context.Background(), p, map[string]struct{}{"a": {}, "b": {}}); err != nil || f != 0 {
		t.Fatalf("scoped: failures=%d err=%v", f, err)
	}
	for _, rel := range []string{"b/x.txt", "b/sub/y.txt", "b/empty"} {
		if !exists(filepath.Join(remote, filepath.FromSlash(rel))) {
			t.Errorf("remote missing %s after rename", rel)
		}
	}
	if exists(filepath.Join(remote, "a")) {
		t.Error("remote still has the old directory a/ after rename")
	}
}

func TestRunRobustScoped_RecursiveDeletePropagates(t *testing.T) {
	local, remote, p := baselineTree(t)
	if err := os.RemoveAll(filepath.Join(local, "a")); err != nil {
		t.Fatal(err)
	}
	if _, f, err := RunRobustScoped(context.Background(), p, map[string]struct{}{"a": {}}); err != nil || f != 0 {
		t.Fatalf("scoped: failures=%d err=%v", f, err)
	}
	if exists(filepath.Join(remote, "a")) {
		t.Fatal("recursive local delete did not propagate to the remote")
	}
	// The next full cycle must agree there is nothing left to do.
	plan, f, err := RunRobustOnce(context.Background(), p)
	if err != nil || f != 0 {
		t.Fatalf("follow-up full cycle: failures=%d err=%v", f, err)
	}
	if acts := nonNoOpActions(plan); len(acts) != 0 {
		t.Fatalf("full cycle found leftover work: %+v", acts)
	}
}

// Three-way safety still holds inside an expanded subtree: a remote file
// edited since the baseline survives a recursive local delete (it is
// downloaded back), exactly as a full cycle would decide.
func TestRunRobustScoped_RecursiveDeleteKeepsRemoteEdit(t *testing.T) {
	local, remote, p := baselineTree(t)
	edited := filepath.Join(remote, "a", "sub", "y.txt")
	mkfile(t, edited, "edited on another machine")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(edited, future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(local, "a")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunRobustScoped(context.Background(), p, map[string]struct{}{"a": {}}); err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if got := mustReadFile(t, edited); got != "edited on another machine" {
		t.Fatalf("remote edit lost: %q", got)
	}
	if got := mustReadFile(t, filepath.Join(local, "a", "sub", "y.txt")); got != "edited on another machine" {
		t.Fatalf("remote edit not restored locally: %q", got)
	}
	if exists(filepath.Join(remote, "a", "x.txt")) {
		t.Fatal("unchanged a/x.txt should have been deleted on the remote")
	}
}

// A directory created locally with contents (e.g. extracted, or moved in
// from outside the tree) arrives as one Create event for the directory.
func TestRunRobustScoped_NewDirectoryWithContentsUploads(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	p := newTestRobustParams(t, local, remote)
	mkfile(t, filepath.Join(local, "new", "deep", "f.txt"), "f")
	if _, f, err := RunRobustScoped(context.Background(), p, map[string]struct{}{"new": {}}); err != nil || f != 0 {
		t.Fatalf("scoped: failures=%d err=%v", f, err)
	}
	if got := mustReadFile(t, filepath.Join(remote, "new", "deep", "f.txt")); got != "f" {
		t.Fatalf("got %q", got)
	}
}

// An ignored directory in the batch is not expanded.
func TestRunRobustScoped_IgnoredDirectoryNotExpanded(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	mkfile(t, filepath.Join(local, ".syncdignore"), "build/\n")
	mkfile(t, filepath.Join(local, "build", "out.bin"), "artifact")
	p := newTestRobustParams(t, local, remote)
	if _, _, err := RunRobustScoped(context.Background(), p, map[string]struct{}{"build": {}}); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(remote, "build")) {
		t.Fatal("ignored directory was synced")
	}
}
