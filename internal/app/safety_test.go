package app

import (
	"context"
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

func TestScopedScanRejectsCaseAliasOutsideEvent(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	setFile(t, filepath.Join(local, "report.txt"), "base", time.Now())
	p := newTestRobustParams(t, local, remote)
	ctx := context.Background()
	if _, n, err := RunRobustOnce(ctx, p); err != nil || n != 0 {
		t.Fatalf("baseline: %d failures, %v", n, err)
	}
	if err := os.Rename(filepath.Join(local, "report.txt"), filepath.Join(local, "Report.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunRobustScoped(ctx, p, map[string]struct{}{"Report.txt": {}}); err == nil {
		t.Fatal("scoped check accepted case alias")
	}
	rows, err := p.Repo.LoadAll(ctx)
	if err != nil || len(rows) != 1 || rows["report.txt"].Path == "" {
		t.Fatalf("manifest changed: %v %v", rows, err)
	}
}
