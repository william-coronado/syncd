package app

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

func TestSafetySymlinkEscape(t *testing.T) {
	for _, robust := range []bool{false, true} {
		t.Run(map[bool]string{false: "fast", true: "robust"}[robust], func(t *testing.T) {
			left, right, outside := t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.Mkdir(filepath.Join(left, "sub"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(right, "sub")); err != nil {
				t.Fatal(err)
			}
			setFile(t, filepath.Join(left, "sub", "payload"), "outside write", time.Unix(1700000000, 0))
			if robust {
				p := newTestRobustParams(t, left, right)
				_, failures, err := RunRobustOnce(context.Background(), p)
				if err != nil || failures != 0 {
					t.Fatalf("cycle failed: %v, %d", err, failures)
				}
			} else {
				err := SyncOnce(context.Background(), SyncOptions{Name: "review", Left: left, Right: right, Mode: reconcile.FastMirror, Assume: true, Workers: 1, DBPath: filepath.Join(t.TempDir(), "state.db")})
				if err != nil {
					t.Fatal(err)
				}
			}
			if b, err := os.ReadFile(filepath.Join(outside, "payload")); err == nil {
				t.Fatalf("sync escaped root and created %q outside it", b)
			}
		})
	}
}

func TestSafetyCaseOnlyMirror(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	setFile(t, filepath.Join(left, "Report.txt"), "new content", time.Unix(1700000001, 0))
	setFile(t, filepath.Join(right, "report.txt"), "old", time.Unix(1700000000, 0))
	err := SyncOnce(context.Background(), SyncOptions{Name: "review", Left: left, Right: right, Mode: reconcile.FastMirror, Assume: true, Workers: 1, DBPath: filepath.Join(t.TempDir(), "state.db")})
	if err == nil {
		t.Fatal("case collision should refuse the cycle")
	}
	if b, err := os.ReadFile(filepath.Join(right, "report.txt")); err != nil || string(b) != "old" {
		t.Fatalf("mirror changed destination before rejecting collision: content=%q err=%v", b, err)
	}
}

func TestSafetyScopedSymlinkRead(t *testing.T) {
	left, right, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(left, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(right, "sub")); err != nil {
		t.Fatal(err)
	}
	setFile(t, filepath.Join(outside, "secret"), "private data", time.Unix(1700000000, 0))
	p := newTestRobustParams(t, left, right)
	p.Policy = fs.HashAlways
	_, failures, err := RunRobustScoped(context.Background(), p, map[string]struct{}{"sub/secret": {}})
	if err != nil || failures != 0 {
		t.Fatalf("cycle failed: %v %d", err, failures)
	}
	if b, err := os.ReadFile(filepath.Join(left, "sub", "secret")); err == nil {
		t.Fatalf("scoped scan copied data from outside remote root: %q", b)
	}
}

func TestFastSyncKeepsScannedRootThroughConfirmation(t *testing.T) {
	left, base, other := t.TempDir(), t.TempDir(), t.TempDir()
	parent := filepath.Join(base, "parent")
	right := filepath.Join(parent, "right")
	if err := os.MkdirAll(right, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(other, "right"), 0700); err != nil {
		t.Fatal(err)
	}
	setFile(t, filepath.Join(left, "file"), "incoming", time.Now())
	err := SyncOnce(context.Background(), SyncOptions{Left: left, Right: right, Mode: reconcile.FastMirror, DBPath: filepath.Join(t.TempDir(), "state.db"), Confirm: func(string) bool {
		if err := os.Rename(parent, filepath.Join(base, "original")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, parent); err != nil {
			t.Fatal(err)
		}
		return true
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "right", "file")); !os.IsNotExist(err) {
		t.Fatalf("redirected write to a replacement root: %v", err)
	}
	if got := mustReadFile(t, filepath.Join(base, "original", "right", "file")); got != "incoming" {
		t.Fatalf("original tree content = %q", got)
	}
}

// A case-only rename seen by a scoped batch is escalated to a full cycle,
// which propagates it instead of refusing the pair forever.
func TestScopedCaseOnlyRenamePropagates(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	setFile(t, filepath.Join(local, "report.txt"), "base", time.Unix(1700000000, 0))
	p := newTestRobustParams(t, local, remote)
	ctx := context.Background()
	if _, n, err := RunRobustOnce(ctx, p); err != nil || n != 0 {
		t.Fatalf("baseline: %d failures, %v", n, err)
	}
	if err := os.Rename(filepath.Join(local, "report.txt"), filepath.Join(local, "Report.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunRobustScoped(ctx, p, map[string]struct{}{"Report.txt": {}}); err != nil {
		t.Fatalf("scoped cycle refused a case-only rename: %v", err)
	}
	// On a case-insensitive remote the create may wait one cycle for the
	// old spelling's delete; a second cycle must settle it either way.
	if _, _, err := RunRobustOnce(ctx, p); err != nil {
		t.Fatalf("follow-up cycle: %v", err)
	}
	entries, err := os.ReadDir(remote)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() != ".synclock" {
			names = append(names, e.Name())
		}
	}
	if len(names) != 1 || names[0] != "Report.txt" {
		t.Fatalf("remote entries = %v, want [Report.txt]", names)
	}
	if got := mustReadFile(t, filepath.Join(remote, "Report.txt")); got != "base" {
		t.Fatalf("remote content = %q", got)
	}
	rows, err := p.Repo.LoadAll(ctx)
	if err != nil || len(rows) != 1 || rows["Report.txt"].Path == "" {
		t.Fatalf("manifest = %v, %v", rows, err)
	}
}

// Genuine collisions (two spellings on one side) are still refused, by the
// escalated full cycle as well.
func TestScopedCaseCollisionStillRefused(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	setFile(t, filepath.Join(local, "report.txt"), "a", time.Unix(1700000000, 0))
	setFile(t, filepath.Join(remote, "Report.txt"), "b", time.Unix(1700000000, 0))
	p := newTestRobustParams(t, local, remote)
	_, _, err := RunRobustScoped(context.Background(), p, map[string]struct{}{"report.txt": {}})
	if !errors.Is(err, fs.ErrCaseAlias) {
		t.Fatalf("expected case-alias refusal, got %v", err)
	}
	rows, _ := p.Repo.LoadAll(context.Background())
	if len(rows) != 0 {
		t.Fatalf("manifest changed: %v", rows)
	}
}
