package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"syncd/internal/reconcile"
)

func TestSyncOnceMirrorEndToEnd(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, Mode: reconcile.FastMirror,
		Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(right, "a.txt")); string(b) != "hi" {
		t.Error("mirror did not copy file")
	}
}

func TestSyncOnceTwoWayPersistsState(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	dbp := filepath.Join(t.TempDir(), "m.db")
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	opts := SyncOptions{Name: "j", Left: left, Right: right, Mode: reconcile.FastTwoWay, Assume: true, Workers: 1, DBPath: dbp}
	if err := SyncOnce(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	// Delete on left; second run should propagate delete to right (state proves prior sync).
	if err := os.Remove(filepath.Join(left, "a.txt")); err != nil {
		t.Fatalf("remove a.txt: %v", err)
	}
	if err := SyncOnce(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(right, "a.txt")); !os.IsNotExist(err) {
		t.Error("two-way delete not propagated using persisted state")
	}
}

// TestSyncOnceMirrorMissingSourceErrorsWithoutTouchingDest guards against a
// data-loss footgun: if a mirror's --left (source) is mistyped or missing,
// SyncOnce must refuse rather than silently treating a fabricated empty
// source as "the source has nothing, so delete everything on the
// destination" - which, under --yes, would wipe the destination outright.
func TestSyncOnceMirrorMissingSourceErrorsWithoutTouchingDest(t *testing.T) {
	right := t.TempDir()
	if err := os.WriteFile(filepath.Join(right, "keep.txt"), []byte("precious"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}
	missingLeft := filepath.Join(t.TempDir(), "does-not-exist")

	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: missingLeft, Right: right, Mode: reconcile.FastMirror,
		Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err == nil {
		t.Fatal("expected SyncOnce to error when the mirror source does not exist, got nil")
	}

	if b, readErr := os.ReadFile(filepath.Join(right, "keep.txt")); readErr != nil || string(b) != "precious" {
		t.Fatalf("destination file was touched: read err=%v content=%q", readErr, b)
	}
	if _, statErr := os.Stat(missingLeft); !os.IsNotExist(statErr) {
		t.Fatalf("expected source path to remain nonexistent (must not be auto-created), stat err = %v", statErr)
	}
}
