package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"syncd/internal/fs"
)

// TestRunWatch_RejectsNestedRoots is a regression test for a review finding:
// unlike SyncOnce (which calls rootsOverlap via validateRoots), RunWatch
// handed --local/--remote straight to runtime.ResolvePaths and the runLoop
// callback with no equivalent check. `syncd watch --local /data --remote
// /data/backup` would start the daemon and, on the first deep scan, walk the
// same files under two different relative-path keys - the exact corruption
// rootsOverlap exists to prevent for `sync`. RunWatch must reject overlapping
// roots before doing anything else (acquiring the lock, opening the db,
// invoking runLoop).
func TestRunWatch_RejectsNestedRoots(t *testing.T) {
	local := t.TempDir()
	remote := filepath.Join(local, "backup")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatalf("mkdir remote: %v", err)
	}

	loopCalled := false
	fakeLoop := func(ctx context.Context, p RobustParams, matcher *fs.IgnoreMatcher, cfg WatchConfig) error {
		loopCalled = true
		return nil
	}

	err := RunWatch(context.Background(), WatchConfig{
		LocalRoot:  local,
		RemoteRoot: remote,
		StateDir:   t.TempDir(),
	}, fakeLoop)

	if err == nil {
		t.Fatal("expected an error for nested local/remote roots, got nil")
	}
	if !strings.Contains(err.Error(), "overlap") && !strings.Contains(err.Error(), "nested") {
		t.Fatalf("expected an overlap-related error, got: %v", err)
	}
	if loopCalled {
		t.Fatal("runLoop was invoked despite overlapping roots")
	}
}

// TestRunWatch_RejectsIdenticalRoots covers the same-directory case (not
// just nesting).
func TestRunWatch_RejectsIdenticalRoots(t *testing.T) {
	root := t.TempDir()

	loopCalled := false
	fakeLoop := func(ctx context.Context, p RobustParams, matcher *fs.IgnoreMatcher, cfg WatchConfig) error {
		loopCalled = true
		return nil
	}

	err := RunWatch(context.Background(), WatchConfig{
		LocalRoot:  root,
		RemoteRoot: root,
		StateDir:   t.TempDir(),
	}, fakeLoop)

	if err == nil {
		t.Fatal("expected an error for identical local/remote roots, got nil")
	}
	if !strings.Contains(err.Error(), "overlap") && !strings.Contains(err.Error(), "same directory") {
		t.Fatalf("expected an overlap-related error, got: %v", err)
	}
	if loopCalled {
		t.Fatal("runLoop was invoked despite identical roots")
	}
}
