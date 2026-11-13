package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"syncd/internal/db"
	"syncd/internal/reconcile"
)

// The --hash confirm must approve exactly the plan that then runs. It used
// to approve a separate preview scan's plan and then execute a fresh scan's
// plan, so anything that changed while the prompt was open (here: a file
// created mid-prompt) executed without ever being shown to the user.
func TestSyncOnceHash_ExecutesExactlyTheApprovedPlan(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "shown.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	var prompts []string
	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, UseHash: true, Workers: 2,
		DBPath: filepath.Join(t.TempDir(), "m.db"),
		Confirm: func(summary string) bool {
			prompts = append(prompts, summary)
			// Appears after the plan was built: must not be acted on.
			if err := os.WriteFile(filepath.Join(left, "unseen.txt"), []byte("b"), 0o644); err != nil {
				t.Fatal(err)
			}
			return true
		},
	})
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("want exactly 1 confirm prompt, got %d", len(prompts))
	}
	if !strings.Contains(prompts[0], "shown.txt") || strings.Contains(prompts[0], "unseen.txt") {
		t.Fatalf("unexpected summary:\n%s", prompts[0])
	}
	if _, err := os.Stat(filepath.Join(right, "shown.txt")); err != nil {
		t.Fatalf("approved upload did not run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(right, "unseen.txt")); !os.IsNotExist(err) {
		t.Fatalf("file never shown in the approved summary was uploaded (stat err=%v)", err)
	}
}

func TestSyncOnceHash_DeclinedPlanChangesNothing(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(left, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "m.db")
	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, UseHash: true, Workers: 2, DBPath: dbPath,
		Confirm: func(string) bool { return false },
	})
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("want ErrAborted, got %v", err)
	}
	entries, _ := os.ReadDir(right)
	if len(entries) != 0 {
		t.Fatalf("declined run touched the destination: %v", entries)
	}
	sqlDB, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	rows, err := db.NewManifestRepo(sqlDB).LoadAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("declined run wrote the manifest: %+v", rows)
	}
}

func TestSyncOnceHash_NoActionsDoesNotPrompt(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, UseHash: true, Workers: 1,
		DBPath:  filepath.Join(t.TempDir(), "m.db"),
		Confirm: func(string) bool { t.Fatal("prompted for a plan with no actions"); return false },
	})
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
}

// Zero tasks: no "continue with the file transfers?" prompt, no progress
// bar - but two-way state must still be persisted, so a first run over two
// already-identical trees establishes its baseline.
func TestSyncOnceFast_NoTasksSkipsPromptButPersistsTwoWayState(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for _, root := range []string{left, right} {
		if err := os.WriteFile(filepath.Join(root, "same.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Make both copies' mtimes identical so the fast reconciler sees no work.
	info, err := os.Stat(filepath.Join(left, "same.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(right, "same.txt"), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	progress := &countingSink{}
	err = SyncOnce(context.Background(), SyncOptions{
		Name: "tw", Left: left, Right: right, Mode: reconcile.FastTwoWay, Workers: 1, DBPath: dbPath,
		Confirm:  func(string) bool { t.Fatal("prompted for zero tasks"); return false },
		Progress: progress,
	})
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if progress.started {
		t.Fatal("progress bar started for zero tasks")
	}
	sqlDB, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	state, err := db.NewJobStateRepo(sqlDB).Load(context.Background(), "tw")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state["same.txt"]; !ok {
		t.Fatalf("two-way baseline not persisted: %+v", state)
	}
}

type countingSink struct{ started bool }

func (c *countingSink) Start(int)     { c.started = true }
func (c *countingSink) Advance(int64) {}
func (c *countingSink) Done()         {}

func TestRunInit_RejectsOverlappingRoots(t *testing.T) {
	base := t.TempDir()
	p := newTestRobustParams(t, base, base)
	for _, remote := range []string{base, filepath.Join(base, "nested")} {
		if err := RunInit(base, remote, p.Logger); err == nil {
			t.Fatalf("RunInit(%s, %s) accepted overlapping roots", base, remote)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "nested")); !os.IsNotExist(err) {
		t.Fatalf("rejected init still created the nested remote root (stat err=%v)", err)
	}
}
