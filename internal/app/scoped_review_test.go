package app

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// failLstatFor makes scopedLstat return EACCES for exactly path, as if an
// ancestor had lost search permission (chmod can't simulate that when the
// tests run as root).
func failLstatFor(t *testing.T, path string) {
	t.Helper()
	orig := scopedLstat
	scopedLstat = func(p string) (os.FileInfo, error) {
		if p == path {
			return nil, &os.PathError{Op: "lstat", Path: p, Err: syscall.EACCES}
		}
		return orig(p)
	}
	t.Cleanup(func() { scopedLstat = orig })
}

// PR #4 review (P1): a batch directory that can't be stat'd on one side is
// UNKNOWN there, not absent. Expanding only the readable side's subtree
// used to make the unreadable side's tracked children read as deleted - and
// the intact copies on the readable side were deleted. Checked in both
// directions.
func TestRunRobustScoped_UnstattableSideNeverDrivesDeletes(t *testing.T) {
	for _, side := range []string{"local", "remote"} {
		t.Run(side, func(t *testing.T) {
			local, remote, p := baselineTree(t)
			unreadable, intact := local, remote
			if side == "remote" {
				unreadable, intact = remote, local
			}
			failLstatFor(t, filepath.Join(unreadable, "a"))

			_, failures, err := RunRobustScoped(context.Background(), p, map[string]struct{}{"a": {}})
			if err != nil || failures != 0 {
				t.Fatalf("scoped: failures=%d err=%v", failures, err)
			}
			for _, rel := range []string{"a/x.txt", "a/sub/y.txt"} {
				if !exists(filepath.Join(intact, filepath.FromSlash(rel))) {
					t.Errorf("%s deleted from the readable side because the %s side was unreadable", rel, side)
				}
			}
			rows, err := p.Repo.LoadAll(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := rows["a/x.txt"]; !ok {
				t.Error("manifest row for a/x.txt dropped for an unknown subtree")
			}
		})
	}
}

// PR #4 review (P2): once file actions have run, a cancellation must not
// stop their manifest updates from being committed. The directory baseline
// used to be loaded with ctx AFTER the actions, so a cancel landing there
// returned early and left completed copies unrecorded.
func TestRunRobustScoped_CancelAfterActionsStillCommitsManifest(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	p := newTestRobustParams(t, local, remote)
	mkfile(t, filepath.Join(local, "new", "f.txt"), "f")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orig := scopedActionsDone
	scopedActionsDone = cancel
	t.Cleanup(func() { scopedActionsDone = orig })

	_, _, _ = RunRobustScoped(ctx, p, map[string]struct{}{"new": {}})

	if got := mustReadFile(t, filepath.Join(remote, "new", "f.txt")); got != "f" {
		t.Fatalf("precondition: upload did not complete (%q)", got)
	}
	rows, err := p.Repo.LoadAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rows["new/f.txt"]; !ok {
		t.Fatal("completed upload missing from the manifest after cancellation")
	}
}
