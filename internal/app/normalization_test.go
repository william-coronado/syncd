package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"syncd/internal/reconcile"
)

// A local file whose name is stored in NFD (byte-exact filesystems such as
// Linux's keep it that way) is keyed by its NFC spelling everywhere in the
// engine. Every action on it must still reach the real entry.
const (
	nfdName = "cafe\u0301.txt"
	nfcName = "caf\u00e9.txt"
)

func syncedNFDPair(t *testing.T) (local, remote string, p RobustParams) {
	t.Helper()
	local, remote = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(local, nfdName), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	p = newTestRobustParams(t, local, remote)
	if _, failures, err := RunRobustOnce(context.Background(), p); err != nil || failures != 0 {
		t.Fatalf("initial cycle: failures=%d err=%v", failures, err)
	}
	if got := mustReadFile(t, filepath.Join(remote, nfcName)); got != "v1" {
		t.Fatalf("remote after initial cycle = %q", got)
	}
	return local, remote, p
}

func TestRunRobustOnce_RemoteEditReachesNFDNamedLocalFile(t *testing.T) {
	local, remote, p := syncedNFDPair(t)
	if err := os.WriteFile(filepath.Join(remote, nfcName), []byte("v2 from remote"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, failures, err := RunRobustOnce(context.Background(), p); err != nil || failures != 0 {
		t.Fatalf("cycle: failures=%d err=%v", failures, err)
	}
	if got := mustReadFile(t, filepath.Join(local, nfdName)); got != "v2 from remote" {
		t.Fatalf("local file = %q, want the remote edit", got)
	}
	assertOnlyEntry(t, local, nfdName)
}

func TestRunRobustOnce_RemoteDeleteRemovesNFDNamedLocalFile(t *testing.T) {
	local, remote, p := syncedNFDPair(t)
	if err := os.Remove(filepath.Join(remote, nfcName)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // the second cycle is where a resurrection showed up
		if _, failures, err := RunRobustOnce(context.Background(), p); err != nil || failures != 0 {
			t.Fatalf("cycle %d: failures=%d err=%v", i, failures, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(local, nfdName)); !os.IsNotExist(err) {
		t.Fatalf("local NFD file survived the remote delete (err=%v)", err)
	}
	if _, err := os.Lstat(filepath.Join(remote, nfcName)); !os.IsNotExist(err) {
		t.Fatalf("remote file was resurrected (err=%v)", err)
	}
}

// The scoped (fsnotify) cycle used to read an edited NFD file as deleted
// and delete the remote copy outright.
func TestRunRobustScoped_LocalEditOfNFDNamedFileUploads(t *testing.T) {
	local, remote, p := syncedNFDPair(t)
	if err := os.WriteFile(filepath.Join(local, nfdName), []byte("v2 local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, failures, err := RunRobustScoped(context.Background(), p, map[string]struct{}{nfcName: {}})
	if err != nil || failures != 0 {
		t.Fatalf("scoped cycle: failures=%d err=%v", failures, err)
	}
	for _, a := range plan.Actions {
		if a.Type == reconcile.ActionDeleteRemote {
			t.Fatalf("scoped cycle planned %s %q (%s)", a.Type, a.Path, a.Reason)
		}
	}
	if got := mustReadFile(t, filepath.Join(remote, nfcName)); got != "v2 local edit" {
		t.Fatalf("remote file = %q, want the local edit", got)
	}
}

func assertOnlyEntry(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("%s holds %q, want only %q", dir, names, name)
	}
}

// Fast mirror overwrites by path; at a destination that spells the name in
// NFD it must replace that file, not publish an NFC twin beside it (which
// every later scan then refuses as a normalization collision).
func TestSyncOnce_MirrorReplacesNFDNamedDestinationFile(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, nfcName), []byte("new and longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(right, nfdName), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, Mode: reconcile.FastMirror,
		Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyEntry(t, right, nfdName)
	if got := mustReadFile(t, filepath.Join(right, nfdName)); got != "new and longer" {
		t.Fatalf("destination = %q, want the mirrored content", got)
	}
}
