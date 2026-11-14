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
