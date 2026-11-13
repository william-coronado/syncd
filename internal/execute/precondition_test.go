package execute

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"syncd/internal/fs"
	"syncd/internal/reconcile"
)

// scannedEntry stats path and returns the FileEntry a scan would have
// recorded for it (size + unix-second mtime), so a test can simulate "the
// scan saw this file" and then mutate it before the executor runs.
func scannedEntry(t *testing.T, root, rel string) fs.FileEntry {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stat %s: %v", abs, err)
	}
	return fs.FileEntry{Path: rel, AbsPath: abs, Size: info.Size(), Mtime: info.ModTime().Unix()}
}

func writeAt(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func runOne(t *testing.T, state SyncState, action reconcile.Action) Result {
	t.Helper()
	results, err := RunActions(context.Background(), 1, state, []reconcile.Action{action})
	if err != nil {
		t.Fatalf("RunActions: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	return results[0]
}

var (
	tOld = time.Unix(1_700_000_000, 0)
	tNew = time.Unix(1_700_000_500, 0)
)

// A remote file edited after the scan (by another machine, over SMB) must
// not be overwritten by an upload planned against the stale scan.
func TestRunUpload_RemoteEditedAfterScan_Deferred(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	writeAt(t, filepath.Join(local, "a.txt"), "local edit", tNew)
	writeAt(t, filepath.Join(remote, "a.txt"), "base", tOld)
	state := SyncState{
		LocalRoot: local, RemoteRoot: remote,
		LocalFiles:  map[string]fs.FileEntry{"a.txt": scannedEntry(t, local, "a.txt")},
		RemoteFiles: map[string]fs.FileEntry{"a.txt": scannedEntry(t, remote, "a.txt")},
	}
	writeAt(t, filepath.Join(remote, "a.txt"), "remote edit after scan", tNew)

	res := runOne(t, state, reconcile.Action{Type: reconcile.ActionUpload, Path: "a.txt"})
	if !errors.Is(res.Error, ErrDestinationChanged) {
		t.Fatalf("want ErrDestinationChanged, got %v", res.Error)
	}
	if len(res.Updates) != 0 {
		t.Fatalf("deferred action must not update the manifest: %+v", res.Updates)
	}
	if got := readString(t, filepath.Join(remote, "a.txt")); got != "remote edit after scan" {
		t.Fatalf("remote edit was clobbered: %q", got)
	}
}

// A local file created after the scan saw the path absent must not be
// overwritten by a download of the remote's new file.
func TestRunDownload_LocalCreatedAfterScan_Deferred(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	writeAt(t, filepath.Join(remote, "n.txt"), "remote new", tOld)
	state := SyncState{
		LocalRoot: local, RemoteRoot: remote,
		LocalFiles:  map[string]fs.FileEntry{},
		RemoteFiles: map[string]fs.FileEntry{"n.txt": scannedEntry(t, remote, "n.txt")},
	}
	writeAt(t, filepath.Join(local, "n.txt"), "user just saved this", tNew)

	res := runOne(t, state, reconcile.Action{Type: reconcile.ActionDownload, Path: "n.txt"})
	if !errors.Is(res.Error, ErrDestinationChanged) {
		t.Fatalf("want ErrDestinationChanged, got %v", res.Error)
	}
	if got := readString(t, filepath.Join(local, "n.txt")); got != "user just saved this" {
		t.Fatalf("local file was clobbered: %q", got)
	}
}

// The most dangerous race: a delete planned because "local deleted, remote
// unchanged" must not destroy a remote edit made after the scan.
func TestRunDelete_TargetEditedAfterScan_Deferred(t *testing.T) {
	for _, deleteLocal := range []bool{false, true} {
		local, remote := t.TempDir(), t.TempDir()
		target := remote
		actionType := reconcile.ActionDeleteRemote
		if deleteLocal {
			target = local
			actionType = reconcile.ActionDeleteLocal
		}
		writeAt(t, filepath.Join(target, "d.txt"), "base", tOld)
		files := map[string]fs.FileEntry{"d.txt": scannedEntry(t, target, "d.txt")}
		state := SyncState{LocalRoot: local, RemoteRoot: remote, LocalFiles: map[string]fs.FileEntry{}, RemoteFiles: map[string]fs.FileEntry{}}
		if deleteLocal {
			state.LocalFiles = files
		} else {
			state.RemoteFiles = files
		}
		writeAt(t, filepath.Join(target, "d.txt"), "edited after scan", tNew)

		res := runOne(t, state, reconcile.Action{Type: actionType, Path: "d.txt"})
		if !errors.Is(res.Error, ErrDestinationChanged) {
			t.Fatalf("%s: want ErrDestinationChanged, got %v", actionType, res.Error)
		}
		if got := readString(t, filepath.Join(target, "d.txt")); got != "edited after scan" {
			t.Fatalf("%s: edited file was destroyed or altered: %q", actionType, got)
		}
	}
}

// A delete whose target already vanished is still a success: the goal
// state ("absent") holds, and the manifest row must be dropped.
func TestRunDelete_TargetAlreadyGone_Succeeds(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	writeAt(t, filepath.Join(remote, "d.txt"), "base", tOld)
	state := SyncState{
		LocalRoot: local, RemoteRoot: remote,
		RemoteFiles: map[string]fs.FileEntry{"d.txt": scannedEntry(t, remote, "d.txt")},
	}
	if err := os.Remove(filepath.Join(remote, "d.txt")); err != nil {
		t.Fatal(err)
	}
	res := runOne(t, state, reconcile.Action{Type: reconcile.ActionDeleteRemote, Path: "d.txt"})
	if res.Error != nil {
		t.Fatalf("delete of already-gone target should succeed: %v", res.Error)
	}
	if len(res.Updates) != 1 || !res.Updates[0].Delete {
		t.Fatalf("want one manifest delete, got %+v", res.Updates)
	}
}

// The unchanged-destination happy path still overwrites.
func TestRunUpload_DestinationUnchanged_Proceeds(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	writeAt(t, filepath.Join(local, "a.txt"), "local edit", tNew)
	writeAt(t, filepath.Join(remote, "a.txt"), "base", tOld)
	state := SyncState{
		LocalRoot: local, RemoteRoot: remote,
		LocalFiles:  map[string]fs.FileEntry{"a.txt": scannedEntry(t, local, "a.txt")},
		RemoteFiles: map[string]fs.FileEntry{"a.txt": scannedEntry(t, remote, "a.txt")},
	}
	res := runOne(t, state, reconcile.Action{Type: reconcile.ActionUpload, Path: "a.txt"})
	if res.Error != nil {
		t.Fatalf("upload: %v", res.Error)
	}
	if got := readString(t, filepath.Join(remote, "a.txt")); got != "local edit" {
		t.Fatalf("remote not updated: %q", got)
	}
}

// RunActions must not report ctx.Err() for a batch that nothing was cut
// short of (docs/follow-ups.md #16). An empty batch under an
// already-cancelled ctx is the deterministic form of "cancellation landed
// after the last action finished": every action (all zero of them)
// completed, so the run succeeded. The pre-cancelled non-empty case is
// covered by TestRunActions_CancelledContextDoesNotHang.
func TestRunActions_CancelledButNothingCutShort_ReportsSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, err := RunActions(ctx, 2, SyncState{}, nil)
	if err != nil {
		t.Fatalf("batch with nothing cut short reported %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("want 0 results, got %d", len(results))
	}
}

func TestFindAvailableConflictPath_Bounded(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	ts := time.Unix(1_700_000_000, 0).UTC()
	for attempt := 0; attempt < maxConflictPathAttempts; attempt++ {
		writeAt(t, filepath.Join(local, filepath.FromSlash(conflictPathAttempt("f.txt", ts, attempt))), "x", tOld)
	}
	if _, err := findAvailableConflictPath(local, remote, "f.txt", ts); err == nil {
		t.Fatal("want an error once every candidate name is taken, got nil")
	}
}
