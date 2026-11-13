package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn and returns what it printed to stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	_ = w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// The --hash confirm lists the directory operations the run will make, not
// just file actions (docs/follow-ups.md #29) - and a run whose only change
// is a directory operation still prompts.
func TestSyncOnceHash_ConfirmListsDirectoryOperations(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(left, "newdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "m.db")
	opts := SyncOptions{Name: "t", Left: left, Right: right, UseHash: true, Workers: 1, DBPath: dbPath}

	var summaries []string
	opts.Confirm = func(s string) bool { summaries = append(summaries, s); return true }
	if err := SyncOnce(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || !strings.Contains(summaries[0], "MKDIR_REMOTE") || !strings.Contains(summaries[0], "newdir/") {
		t.Fatalf("directory create not in the confirm summary: %q", summaries)
	}
	if !exists(filepath.Join(right, "newdir")) {
		t.Fatal("approved directory create did not run")
	}

	// Baseline it, then delete it locally: the removal must be shown too,
	// and declining must keep the remote directory.
	if err := SyncOnce(context.Background(), SyncOptions{Name: "t", Left: left, Right: right, UseHash: true, Workers: 1, DBPath: dbPath, Assume: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(left, "newdir")); err != nil {
		t.Fatal(err)
	}
	summaries = nil
	opts.Confirm = func(s string) bool { summaries = append(summaries, s); return false }
	if err := SyncOnce(context.Background(), opts); !errors.Is(err, ErrAborted) {
		t.Fatalf("want ErrAborted, got %v", err)
	}
	if len(summaries) != 1 || !strings.Contains(summaries[0], "RMDIR_REMOTE") {
		t.Fatalf("directory removal not in the confirm summary: %q", summaries)
	}
	if !exists(filepath.Join(right, "newdir")) {
		t.Fatal("declined run removed the remote directory")
	}
}

func TestSyncOnceHash_DryRunListsDirectoryOperations(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(left, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := SyncOnce(context.Background(), SyncOptions{
			Name: "t", Left: left, Right: right, UseHash: true, DryRun: true, Workers: 1,
			DBPath: filepath.Join(t.TempDir(), "m.db"),
		}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "MKDIR_REMOTE") || !strings.Contains(out, "d/") {
		t.Fatalf("dry-run summary missing directory op:\n%s", out)
	}
	if exists(filepath.Join(right, "d")) {
		t.Fatal("dry run created a directory")
	}
}
