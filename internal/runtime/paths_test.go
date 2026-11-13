package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolvePaths_DistinctRootPairsGetDistinctDefaultDBPaths is a
// regression test for review finding "Critical 3": watch's default manifest
// db was a single fixed path (manifest.db) regardless of --local/--remote,
// and file_manifest is keyed only by relative path (no root-pair column) -
// so starting `syncd watch` for root pair B after pair A would read pair A's
// manifest rows as pair B's own baseline, and could delete or overwrite
// pair B's files based on state that has nothing to do with it. Two
// different (LocalRoot, RemoteRoot) pairs, both leaving DBPath empty, must
// resolve to two different default database paths.
func TestResolvePaths_DistinctRootPairsGetDistinctDefaultDBPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	pathsA, err := ResolvePaths(Config{LocalRoot: "/data/project-a/local", RemoteRoot: "/Volumes/share/project-a"})
	if err != nil {
		t.Fatalf("ResolvePaths pair A: %v", err)
	}
	pathsB, err := ResolvePaths(Config{LocalRoot: "/data/project-b/local", RemoteRoot: "/Volumes/share/project-b"})
	if err != nil {
		t.Fatalf("ResolvePaths pair B: %v", err)
	}

	if pathsA.DBPath == pathsB.DBPath {
		t.Fatalf("expected distinct root pairs to get distinct default DB paths, both got %q", pathsA.DBPath)
	}
}

// TestResolvePaths_SameRootPairIsStableAcrossCalls ensures the default DB
// path derivation is deterministic, not random - the always-on daemon must
// reconnect to the SAME manifest db on every restart for the same root pair,
// or its three-way baseline would be silently discarded each time.
func TestResolvePaths_SameRootPairIsStableAcrossCalls(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := Config{LocalRoot: "/data/project-a/local", RemoteRoot: "/Volumes/share/project-a"}
	first, err := ResolvePaths(cfg)
	if err != nil {
		t.Fatalf("ResolvePaths (first): %v", err)
	}
	second, err := ResolvePaths(cfg)
	if err != nil {
		t.Fatalf("ResolvePaths (second): %v", err)
	}
	if first.DBPath != second.DBPath {
		t.Errorf("default DB path for the same root pair must be stable across calls: %q vs %q", first.DBPath, second.DBPath)
	}
}

// TestResolvePaths_ExplicitDBPathOverridesDefault checks that an
// explicitly-configured DBPath (the --db-path flag) is never overridden by
// the root-pair-derived default - a user pointing two watch pairs at the
// same db on purpose (or migrating from a pre-fix db) must still be able to.
func TestResolvePaths_ExplicitDBPathOverridesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := filepath.Join(home, "custom-manifest.db")
	paths, err := ResolvePaths(Config{LocalRoot: "/data/a", RemoteRoot: "/data/b", DBPath: want})
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	if paths.DBPath != want {
		t.Errorf("DBPath = %q, want explicit override %q", paths.DBPath, want)
	}
}

// TestResolvePaths_DistinctRootPairsGetDistinctDefaultLockPaths is a
// regression test for review finding P2 #4 (Codex): the default DB path is
// already isolated per root pair (see
// TestResolvePaths_DistinctRootPairsGetDistinctDefaultDBPaths above), but the
// default lock path was left as the fixed "sync.lock" regardless of
// LocalRoot/RemoteRoot - so two `syncd watch` instances for unrelated root
// pairs could never run concurrently, contradicting RunWatch's stated
// one-watcher-per-local-root behavior and the per-pair database design.
func TestResolvePaths_DistinctRootPairsGetDistinctDefaultLockPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	pathsA, err := ResolvePaths(Config{LocalRoot: "/data/project-a/local", RemoteRoot: "/Volumes/share/project-a"})
	if err != nil {
		t.Fatalf("ResolvePaths pair A: %v", err)
	}
	pathsB, err := ResolvePaths(Config{LocalRoot: "/data/project-b/local", RemoteRoot: "/Volumes/share/project-b"})
	if err != nil {
		t.Fatalf("ResolvePaths pair B: %v", err)
	}

	if pathsA.LockPath == pathsB.LockPath {
		t.Fatalf("expected distinct root pairs to get distinct default lock paths, both got %q", pathsA.LockPath)
	}
}

// TestResolvePaths_SameRootPairLockPathIsStableAcrossCalls mirrors the DB
// path's determinism requirement: the daemon must reacquire the SAME lock
// file on every restart for the same root pair.
func TestResolvePaths_SameRootPairLockPathIsStableAcrossCalls(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := Config{LocalRoot: "/data/project-a/local", RemoteRoot: "/Volumes/share/project-a"}
	first, err := ResolvePaths(cfg)
	if err != nil {
		t.Fatalf("ResolvePaths (first): %v", err)
	}
	second, err := ResolvePaths(cfg)
	if err != nil {
		t.Fatalf("ResolvePaths (second): %v", err)
	}
	if first.LockPath != second.LockPath {
		t.Errorf("default lock path for the same root pair must be stable across calls: %q vs %q", first.LockPath, second.LockPath)
	}
}

// TestResolvePaths_ExplicitLockPathOverridesDefault checks an explicit
// LockPath override is never replaced by the root-pair-derived default.
func TestResolvePaths_ExplicitLockPathOverridesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := filepath.Join(home, "custom-sync.lock")
	paths, err := ResolvePaths(Config{LocalRoot: "/data/a", RemoteRoot: "/data/b", LockPath: want})
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	if paths.LockPath != want {
		t.Errorf("LockPath = %q, want explicit override %q", paths.LockPath, want)
	}
}

func TestResolvePaths_CreatesStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	paths, err := ResolvePaths(Config{LocalRoot: "/data/a", RemoteRoot: "/data/b"})
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	if _, statErr := os.Stat(paths.StateDir); statErr != nil {
		t.Errorf("expected state dir %s to be created: %v", paths.StateDir, statErr)
	}
}

// Two watch daemons for different root pairs must not share (and rotate
// each other's) default log file (docs/follow-ups.md #27); the same pair
// must keep its log across restarts; an explicit --log-path still wins; and
// a caller with no roots keeps the historical sync.log.
func TestResolvePaths_DefaultLogPathIsPerRootPair(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgA := Config{LocalRoot: "/data/project-a/local", RemoteRoot: "/Volumes/share/project-a"}
	cfgB := Config{LocalRoot: "/data/project-b/local", RemoteRoot: "/Volumes/share/project-b"}

	a1, err := ResolvePaths(cfgA)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := ResolvePaths(cfgA)
	b, _ := ResolvePaths(cfgB)
	if a1.LogPath == b.LogPath {
		t.Fatalf("distinct root pairs share default log %q", a1.LogPath)
	}
	if a1.LogPath != a2.LogPath {
		t.Fatalf("default log path not stable: %q vs %q", a1.LogPath, a2.LogPath)
	}

	want := filepath.Join(home, "custom.log")
	explicit, _ := ResolvePaths(Config{LocalRoot: "/a", RemoteRoot: "/b", LogPath: want})
	if explicit.LogPath != want {
		t.Fatalf("LogPath = %q, want explicit %q", explicit.LogPath, want)
	}
	none, _ := ResolvePaths(Config{})
	if filepath.Base(none.LogPath) != "sync.log" {
		t.Fatalf("rootless default = %q, want sync.log", none.LogPath)
	}
}
