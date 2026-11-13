package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"syncd/internal/reconcile"
)

func setFile(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// Both sides edit the same file between two-way runs: the newer edit wins,
// the older one survives as a conflict copy, and the run after that
// propagates the conflict copy so both trees end up with both versions
// (docs/follow-ups.md #25 - the older edit used to be silently overwritten).
func TestSyncOnceTwoWay_ConflictKeepsBothEditsAndConverges(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	run := func() {
		t.Helper()
		if err := SyncOnce(context.Background(), SyncOptions{
			Name: "tw", Left: left, Right: right, Mode: reconcile.FastTwoWay,
			Assume: true, Workers: 2, DBPath: dbPath,
		}); err != nil {
			t.Fatalf("SyncOnce: %v", err)
		}
	}
	base := time.Unix(1_700_000_000, 0)
	setFile(t, filepath.Join(left, "doc.txt"), "base", base)
	run() // copies to right, baselines

	setFile(t, filepath.Join(left, "doc.txt"), "left edit (older)", base.Add(time.Hour))
	setFile(t, filepath.Join(right, "doc.txt"), "right edit (newer!)", base.Add(2*time.Hour))
	run() // conflict: right wins, left's edit preserved on left
	run() // conflict copy propagates to right

	for _, root := range []string{left, right} {
		if got := mustReadFile(t, filepath.Join(root, "doc.txt")); got != "right edit (newer!)" {
			t.Fatalf("%s/doc.txt = %q, want the newer edit", root, got)
		}
		entries, _ := os.ReadDir(root)
		var kept string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "doc_conflict_") {
				kept = mustReadFile(t, filepath.Join(root, e.Name()))
			}
		}
		if kept != "left edit (older)" {
			t.Fatalf("%s: older edit not kept as a conflict copy (got %q)", root, kept)
		}
	}
}

func TestFastSummary_ShowsMoveSourceAndConflicts(t *testing.T) {
	got := fastSummary("j", reconcile.FastTwoWay, []reconcile.FastTask{
		{Type: reconcile.MoveR, RelPath: "new/name.txt", OldRelPath: "old/name.txt"},
		{Type: reconcile.ConflictL2R, RelPath: "doc.txt"},
	})
	if !strings.Contains(got, "old/name.txt -> new/name.txt") {
		t.Fatalf("move source not shown:\n%s", got)
	}
	if !strings.Contains(got, "conflict copy") || !strings.Contains(got, "doc.txt") {
		t.Fatalf("conflict not shown:\n%s", got)
	}
}

// A task type absent from the fixed display order must still appear in the
// summary the user approves (docs/follow-ups.md #22).
func TestSummaries_IncludeUnlistedTypes(t *testing.T) {
	fast := fastSummary("j", reconcile.FastMirror, []reconcile.FastTask{{Type: "FutureTask", RelPath: "x.txt"}})
	if !strings.Contains(fast, "FutureTask") || !strings.Contains(fast, "x.txt") {
		t.Fatalf("unlisted fast type missing:\n%s", fast)
	}
	robust := robustSummary("j", []reconcile.Action{{Type: "FUTURE_ACTION", Path: "y.txt"}})
	if !strings.Contains(robust, "FUTURE_ACTION") || !strings.Contains(robust, "y.txt") {
		t.Fatalf("unlisted robust type missing:\n%s", robust)
	}
}
