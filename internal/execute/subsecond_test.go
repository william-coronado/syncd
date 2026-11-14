package execute

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"syncd/internal/fs"
	"syncd/internal/reconcile"
)

func TestRunUpload_SubsecondEditDeferred(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	writeAt(t, filepath.Join(local, "a"), "left", tNew)
	writeAt(t, filepath.Join(remote, "a"), "base", tOld.Add(100*time.Millisecond))
	state := SyncState{LocalRoot: local, RemoteRoot: remote, LocalFiles: map[string]fs.FileEntry{"a": scannedEntry(t, local, "a")}, RemoteFiles: map[string]fs.FileEntry{"a": scannedEntry(t, remote, "a")}}
	writeAt(t, filepath.Join(remote, "a"), "edit", tOld.Add(900*time.Millisecond))
	res := runOne(t, state, reconcile.Action{Type: reconcile.ActionUpload, Path: "a"})
	if !errors.Is(res.Error, ErrDestinationChanged) {
		t.Fatalf("same-second edit overwritten: error=%v content=%q", res.Error, readString(t, filepath.Join(remote, "a")))
	}
}

func TestDestructiveActionsDetectTimestampPreservingEdits(t *testing.T) {
	for _, action := range []reconcile.ActionType{reconcile.ActionUpload, reconcile.ActionDownload, reconcile.ActionDeleteLocal, reconcile.ActionDeleteRemote} {
		t.Run(string(action), func(t *testing.T) {
			local, remote := t.TempDir(), t.TempDir()
			for _, root := range []string{local, remote} {
				writeAt(t, filepath.Join(root, "a"), "base", tOld)
			}
			l, r := scannedEntry(t, local, "a"), scannedEntry(t, remote, "a")
			hash, err := fs.FileBLAKE3(filepath.Join(local, "a"))
			if err != nil {
				t.Fatal(err)
			}
			l.Hash, r.Hash = hash, hash
			state := SyncState{LocalRoot: local, RemoteRoot: remote, LocalFiles: map[string]fs.FileEntry{"a": l}, RemoteFiles: map[string]fs.FileEntry{"a": r}}
			target := remote
			if action == reconcile.ActionDownload || action == reconcile.ActionDeleteLocal {
				target = local
			}
			writeAt(t, filepath.Join(target, "a"), "edit", tOld)
			res := runOne(t, state, reconcile.Action{Type: action, Path: "a"})
			if !errors.Is(res.Error, ErrDestinationChanged) {
				t.Fatalf("edit not detected: %v", res.Error)
			}
			if len(res.Updates) != 0 {
				t.Fatal("deferred action updated manifest")
			}
			if got := readString(t, filepath.Join(target, "a")); got != "edit" {
				t.Fatalf("edit lost: %q", got)
			}
		})
	}
}
