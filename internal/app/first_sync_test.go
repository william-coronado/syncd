package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"syncd/internal/fs"
)

// firstSyncFixture builds a pair with no sync history where local has the
// newer work.txt, remote has the newer notes.txt, same.txt is identical,
// and each side has one file the other lacks.
func firstSyncFixture(t *testing.T) (local, remote string) {
	t.Helper()
	local = filepath.Join(t.TempDir(), "dev")
	remote = filepath.Join(t.TempDir(), "share")
	older := time.Now().Add(-48 * time.Hour)
	newer := time.Now().Add(-1 * time.Hour)
	put := func(root, name, content string, mtime time.Time) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	put(local, "work.txt", "local new work", newer)
	put(remote, "work.txt", "remote stale work", older)
	put(local, "notes.txt", "local stale notes", older)
	put(remote, "notes.txt", "remote new notes", newer)
	put(local, "same.txt", "same", older)
	put(remote, "same.txt", "same", older)
	put(local, "only-local.txt", "L", older)
	put(remote, "only-remote.txt", "R", older)
	return local, remote
}

func conflictCopies(t *testing.T, roots ...string) []string {
	t.Helper()
	var out []string
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), "_conflict_") {
				out = append(out, filepath.Join(root, e.Name()))
			}
		}
	}
	return out
}

func assertBoth(t *testing.T, local, remote, name, want string) {
	t.Helper()
	for _, root := range []string{local, remote} {
		if got := mustReadFile(t, filepath.Join(root, name)); got != want {
			t.Errorf("%s in %s = %q, want %q", name, filepath.Base(root), got, want)
		}
	}
}

// The first run over two existing trees: the newer version of each
// differing file wins on both sides, the older one is in the trash, and the
// trees are left free of conflict copies.
func TestFirstSyncNewer_NewerWinsOlderGoesToTrash(t *testing.T) {
	local, remote := firstSyncFixture(t)
	trashDir := filepath.Join(t.TempDir(), "trash")
	p := newTestRobustParams(t, local, remote)
	p.Trash = fs.NewTrash(trashDir)
	p.FirstSync = FirstSyncNewer
	ctx := context.Background()

	if _, failures, err := RunRobustOnce(ctx, p); err != nil || failures != 0 {
		t.Fatalf("first cycle: failures=%d err=%v", failures, err)
	}
	assertBoth(t, local, remote, "work.txt", "local new work")
	assertBoth(t, local, remote, "notes.txt", "remote new notes")
	assertBoth(t, local, remote, "only-local.txt", "L")
	assertBoth(t, local, remote, "only-remote.txt", "R")
	if c := conflictCopies(t, local, remote); len(c) != 0 {
		t.Fatalf("conflict copies left in the trees: %v", c)
	}
	day := filepath.Join(trashDir, time.Now().Format("2006-01-02"))
	if got := mustReadFile(t, filepath.Join(day, fs.RootFolder(remote), "work.txt")); got != "remote stale work" {
		t.Errorf("replaced remote version in trash = %q", got)
	}
	if got := mustReadFile(t, filepath.Join(day, fs.RootFolder(local), "notes.txt")); got != "local stale notes" {
		t.Errorf("replaced local version in trash = %q", got)
	}

	// The pair now has history: a second cycle is a no-op...
	plan, failures, err := RunRobustOnce(ctx, p)
	if err != nil || failures != 0 || len(nonNoOpActions(plan)) != 0 {
		t.Fatalf("second cycle: actions=%v failures=%d err=%v", nonNoOpActions(plan), failures, err)
	}
	// ...and a file later created on both sides with different content is
	// a real conflict, which keeps both.
	if err := os.WriteFile(filepath.Join(local, "later.txt"), []byte("local later"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "later.txt"), []byte("remote later, longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatal(err)
	}
	if c := conflictCopies(t, local); len(c) != 1 {
		t.Fatalf("a later both-sides conflict should keep both; conflict copies: %v", c)
	}
}

func TestFirstSyncLocalAndRemote_ThatSideWins(t *testing.T) {
	for _, tc := range []struct {
		policy      FirstSyncPolicy
		work, notes string
	}{
		{FirstSyncLocal, "local new work", "local stale notes"},
		{FirstSyncRemote, "remote stale work", "remote new notes"},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			local, remote := firstSyncFixture(t)
			p := newTestRobustParams(t, local, remote)
			p.Trash = fs.NewTrash(filepath.Join(t.TempDir(), "trash"))
			p.FirstSync = tc.policy
			if _, failures, err := RunRobustOnce(context.Background(), p); err != nil || failures != 0 {
				t.Fatalf("failures=%d err=%v", failures, err)
			}
			assertBoth(t, local, remote, "work.txt", tc.work)
			assertBoth(t, local, remote, "notes.txt", tc.notes)
			if c := conflictCopies(t, local, remote); len(c) != 0 {
				t.Fatalf("conflict copies: %v", c)
			}
		})
	}
}

// keep-both (and the zero value) is the engine's plain conflict handling;
// newer with no trash, or with nothing to tell the versions apart, must
// fall back to it rather than overwrite a version it can't keep.
func TestFirstSync_FallsBackToKeepingBoth(t *testing.T) {
	t.Run("keep-both", func(t *testing.T) {
		local, remote := firstSyncFixture(t)
		p := newTestRobustParams(t, local, remote)
		p.Trash = fs.NewTrash(filepath.Join(t.TempDir(), "trash"))
		p.FirstSync = FirstSyncKeepBoth
		if _, _, err := RunRobustOnce(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if c := conflictCopies(t, local); len(c) != 2 {
			t.Fatalf("want 2 conflict copies, got %v", c)
		}
	})
	t.Run("newer without a trash", func(t *testing.T) {
		local, remote := firstSyncFixture(t)
		p := newTestRobustParams(t, local, remote)
		p.FirstSync = FirstSyncNewer
		if _, _, err := RunRobustOnce(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if c := conflictCopies(t, local); len(c) != 2 {
			t.Fatalf("want 2 conflict copies, got %v", c)
		}
	})
	t.Run("newer with equal mtimes", func(t *testing.T) {
		local, remote := t.TempDir(), t.TempDir()
		at := time.Now().Add(-time.Hour)
		for root, content := range map[string]string{local: "one", remote: "two"} {
			p := filepath.Join(root, "f.txt")
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(p, at, at); err != nil {
				t.Fatal(err)
			}
		}
		p := newTestRobustParams(t, local, remote)
		p.Trash = fs.NewTrash(filepath.Join(t.TempDir(), "trash"))
		p.FirstSync = FirstSyncNewer
		if _, _, err := RunRobustOnce(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if c := conflictCopies(t, local); len(c) != 1 {
			t.Fatalf("want 1 conflict copy, got %v", c)
		}
	})
}

func TestParseFirstSyncPolicy(t *testing.T) {
	for _, ok := range []string{"newer", "local", "remote", "keep-both"} {
		if _, err := ParseFirstSyncPolicy(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "newest", "both"} {
		if _, err := ParseFirstSyncPolicy(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
