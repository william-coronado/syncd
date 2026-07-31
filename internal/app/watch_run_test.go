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

// TestRunWatch_ResolvesSymlinkedLocalRootBeforeHandingToRunLoop is a
// regression test for a review finding (GPT-5.6 Sol): NewWatcher already
// resolves a symlinked root via filepath.EvalSymlinks internally (see its
// own comment in watcher.go - filepath.WalkDir never follows a symlink
// passed as its root, so an unresolved symlinked root makes the watcher see
// zero directories), but that resolution was local to NewWatcher alone.
// RobustParams (built by RunWatch, consumed by every ScanTree-driven
// poll/deep-scan cycle) still carried the raw, unresolved root. A full scan
// over an unresolved symlinked root also sees that side as completely
// empty - and once a manifest baseline exists, BuildPlan reads every
// already-synced file on the OTHER side as "deleted here" and removes it.
// RunWatch must resolve both roots once, up front, and use the resolved
// paths for everything downstream, not leave NewWatcher's internal
// resolution as the only place symlinks are handled.
func TestRunWatch_ResolvesSymlinkedLocalRootBeforeHandingToRunLoop(t *testing.T) {
	realLocal := t.TempDir()
	symlinkParent := t.TempDir()
	symlinkLocal := filepath.Join(symlinkParent, "local-link")
	if err := os.Symlink(realLocal, symlinkLocal); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	remote := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, ".synclock"), []byte("syncd canary\n"), 0o644); err != nil {
		t.Fatalf("write remote canary: %v", err)
	}

	var gotLocal, gotRemote string
	fakeLoop := func(ctx context.Context, p RobustParams, matcher *fs.IgnoreMatcher, cfg WatchConfig) error {
		gotLocal = p.LocalRoot
		gotRemote = p.RemoteRoot
		return nil
	}

	err := RunWatch(context.Background(), WatchConfig{
		LocalRoot:  symlinkLocal,
		RemoteRoot: remote,
		StateDir:   t.TempDir(),
	}, fakeLoop)
	if err != nil {
		t.Fatalf("RunWatch returned error: %v", err)
	}

	wantLocal, err := filepath.EvalSymlinks(realLocal)
	if err != nil {
		t.Fatalf("EvalSymlinks(realLocal): %v", err)
	}
	wantRemote, err := filepath.EvalSymlinks(remote)
	if err != nil {
		t.Fatalf("EvalSymlinks(remote): %v", err)
	}

	if gotLocal != wantLocal {
		t.Fatalf("RobustParams.LocalRoot = %q, want resolved path %q (the symlink %q was handed through unresolved)", gotLocal, wantLocal, symlinkLocal)
	}
	if gotRemote != wantRemote {
		t.Fatalf("RobustParams.RemoteRoot = %q, want %q", gotRemote, wantRemote)
	}
}
