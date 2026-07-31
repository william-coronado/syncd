package app

import (
	"os"
	"path/filepath"
	"testing"

	"syncd/internal/reconcile"
	"syncd/internal/runtime"
)

func newTestLogger(t *testing.T) *runtime.Logger {
	t.Helper()
	logger, err := runtime.NewLogger(true, filepath.Join(t.TempDir(), "sync.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	return logger
}

// emptyDirSet is a convenience nil-map-avoiding empty baseline for tests
// that don't care about prior baseline state.
func emptyDirSet() map[string]struct{} { return map[string]struct{}{} }

// noSkips is a convenience zero-value SkipSet for tests where nothing was
// skipped during scanning.
func noSkips() reconcile.SkipSet { return reconcile.SkipSet{} }

func TestReconcileDirectories_NewDirNotInBaselineCreatesOnMissingSide(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	logger := newTestLogger(t)

	localDirs := map[string]struct{}{"only-local": {}, "shared": {}}
	remoteDirs := map[string]struct{}{"only-remote": {}, "shared": {}}

	toAdd, toRemove, failures := reconcileDirectories(logger, localRoot, remoteRoot, localDirs, remoteDirs, emptyDirSet(), noSkips())
	if failures != 0 {
		t.Fatalf("expected 0 failures, got %d", failures)
	}
	if len(toRemove) != 0 {
		t.Fatalf("expected no baseline removals, got %v", toRemove)
	}

	if info, err := os.Stat(filepath.Join(remoteRoot, "only-local")); err != nil || !info.IsDir() {
		t.Fatalf("expected only-local mirrored to remote, stat err = %v", err)
	}
	if info, err := os.Stat(filepath.Join(localRoot, "only-remote")); err != nil || !info.IsDir() {
		t.Fatalf("expected only-remote mirrored to local, stat err = %v", err)
	}
	// "shared" already exists conceptually on both sides in the input maps;
	// this only asserts we don't error attempting to create it - since
	// neither temp dir actually has it on disk, MkdirAll would create it if
	// (incorrectly) attempted, so absence would indicate a bug too. Since
	// it's present in both maps, no creation should be attempted at all.
	if _, err := os.Stat(filepath.Join(remoteRoot, "shared")); !os.IsNotExist(err) {
		t.Fatalf("did not expect 'shared' to be created on remote (present in both sides' maps already), err = %v", err)
	}

	wantAdds := map[string]bool{"only-local": true, "only-remote": true, "shared": true}
	if len(toAdd) != len(wantAdds) {
		t.Fatalf("expected all 3 new-to-baseline dirs added, got %v", toAdd)
	}
	for _, d := range toAdd {
		if !wantAdds[d] {
			t.Errorf("unexpected baseline addition %q", d)
		}
	}
}

func TestReconcileDirectories_NoOpWhenAllShared(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	logger := newTestLogger(t)

	dirs := map[string]struct{}{"a": {}, "a/b": {}}
	baseline := map[string]struct{}{"a": {}, "a/b": {}}

	toAdd, toRemove, failures := reconcileDirectories(logger, localRoot, remoteRoot, dirs, dirs, baseline, noSkips())
	if failures != 0 {
		t.Fatalf("expected 0 failures, got %d", failures)
	}
	if len(toAdd) != 0 || len(toRemove) != 0 {
		t.Fatalf("expected no baseline changes when already baselined on both sides, got toAdd=%v toRemove=%v", toAdd, toRemove)
	}
	entries, err := os.ReadDir(localRoot)
	if err != nil {
		t.Fatalf("read local root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no directories created locally, got %v", entries)
	}
}

// TestReconcileDirectories_DeletedFromOneSidePropagatesRatherThanResurrecting
// is a regression test for review finding "Medium 9": before this fix,
// createMissingEmptyDirs unconditionally recreated any directory missing on
// one side, with no baseline to distinguish "the user just deleted this
// empty directory on remote" from "this is a brand-new empty directory the
// user is organizing ahead of time on local". A directory already recorded
// in the baseline (present on both sides as of the last cycle) that is now
// missing from remote must have its local copy removed (delete propagated),
// not recreated on remote.
func TestReconcileDirectories_DeletedFromOneSidePropagatesRatherThanResurrecting(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	logger := newTestLogger(t)

	if err := os.MkdirAll(filepath.Join(localRoot, "empty-dir"), 0o755); err != nil {
		t.Fatalf("mkdir local empty-dir: %v", err)
	}
	// remote's "empty-dir" was deliberately deleted by the user - absent from
	// remoteDirs and from disk.

	localDirs := map[string]struct{}{"empty-dir": {}}
	remoteDirs := map[string]struct{}{}
	baseline := map[string]struct{}{"empty-dir": {}} // present on both sides as of the last cycle

	toAdd, toRemove, failures := reconcileDirectories(logger, localRoot, remoteRoot, localDirs, remoteDirs, baseline, noSkips())
	if failures != 0 {
		t.Fatalf("expected 0 failures, got %d", failures)
	}

	if _, err := os.Stat(filepath.Join(remoteRoot, "empty-dir")); !os.IsNotExist(err) {
		t.Fatalf("expected remote's deleted empty-dir to stay deleted (not resurrected), stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(localRoot, "empty-dir")); !os.IsNotExist(err) {
		t.Fatalf("expected local's empty-dir to be removed (delete propagated), stat err = %v", err)
	}
	if len(toAdd) != 0 {
		t.Fatalf("expected no baseline additions, got %v", toAdd)
	}
	if len(toRemove) != 1 || toRemove[0] != "empty-dir" {
		t.Fatalf("expected empty-dir removed from the baseline, got %v", toRemove)
	}
}

// TestReconcileDirectories_SkippedLocalDirIsLeftAlone is a regression test
// for review finding P1 #2 (Codex): fs.ScanTree omits an unreadable
// directory from ScanResult.Dirs entirely (it can't enumerate what's inside,
// so it never gets to record the directory itself as present) - it only
// records the directory in Skipped/SkippedDirs. Before this fix,
// reconcileDirectories only ever consulted Dirs, so a directory that exists
// on disk but became unreadable (e.g. a permission change) looked identical
// to "deleted locally": present in the baseline, absent from localDirs. That
// misread the remote counterpart as needing its delete propagated - and the
// baseline row for the perfectly-intact local directory got dropped, wiping
// data that was never actually deleted anywhere. A skipped directory must be
// left completely alone: no create, no delete, baseline unchanged.
func TestReconcileDirectories_SkippedLocalDirIsLeftAlone(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	logger := newTestLogger(t)

	// "unreadable-dir" still exists locally (unlike the genuine-delete case),
	// but scanning couldn't read it, so it's absent from localDirs and
	// present in the skip set instead - unlike remoteDirs, which the scan of
	// the OTHER side had no trouble with.
	if err := os.MkdirAll(filepath.Join(localRoot, "unreadable-dir"), 0o755); err != nil {
		t.Fatalf("mkdir local unreadable-dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(remoteRoot, "unreadable-dir"), 0o755); err != nil {
		t.Fatalf("mkdir remote unreadable-dir: %v", err)
	}

	localDirs := map[string]struct{}{}
	remoteDirs := map[string]struct{}{"unreadable-dir": {}}
	baseline := map[string]struct{}{"unreadable-dir": {}} // present on both sides as of the last cycle
	skipped := reconcile.NewSkipSet(
		map[string]struct{}{"unreadable-dir": {}},
		map[string]struct{}{"unreadable-dir": {}},
	)

	toAdd, toRemove, failures := reconcileDirectories(logger, localRoot, remoteRoot, localDirs, remoteDirs, baseline, skipped)
	if failures != 0 {
		t.Fatalf("expected 0 failures, got %d", failures)
	}
	if len(toAdd) != 0 || len(toRemove) != 0 {
		t.Fatalf("expected no baseline changes for a skipped directory, got toAdd=%v toRemove=%v", toAdd, toRemove)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "unreadable-dir")); err != nil {
		t.Fatalf("remote's unreadable-dir must not be removed just because local couldn't be scanned: stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(localRoot, "unreadable-dir")); err != nil {
		t.Fatalf("local's unreadable-dir must still exist (only its listing failed): stat err = %v", err)
	}
}

// TestReconcileDirectories_NestedDeletionRemovesDeepestFirst is a regression
// test for review finding P2 #3 (Codex): allDirs used to be ranged directly
// as a plain map, whose iteration order Go deliberately randomizes. When
// both "parent" and "parent/child" are slated for local removal in the same
// cycle, os.Remove (non-recursive) fails with ENOTEMPTY if "parent" happens
// to be visited before its still-present child - a spurious failure that
// only self-heals on the NEXT cycle, and wrongly reports a failure for a
// case that isn't really one. Removal must proceed deepest-first so both
// come out clean in a single cycle regardless of map iteration order.
func TestReconcileDirectories_NestedDeletionRemovesDeepestFirst(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	logger := newTestLogger(t)

	if err := os.MkdirAll(filepath.Join(localRoot, "parent", "child"), 0o755); err != nil {
		t.Fatalf("mkdir local parent/child: %v", err)
	}
	// remote's "parent" (and "parent/child") were deliberately deleted by the
	// user - absent from remoteDirs and from disk.

	localDirs := map[string]struct{}{"parent": {}, "parent/child": {}}
	remoteDirs := map[string]struct{}{}
	baseline := map[string]struct{}{"parent": {}, "parent/child": {}}

	toAdd, toRemove, failures := reconcileDirectories(logger, localRoot, remoteRoot, localDirs, remoteDirs, baseline, noSkips())
	if failures != 0 {
		t.Fatalf("expected 0 failures (deepest-first removal must avoid ENOTEMPTY), got %d", failures)
	}
	if len(toAdd) != 0 {
		t.Fatalf("expected no baseline additions, got %v", toAdd)
	}
	wantRemoved := map[string]bool{"parent": true, "parent/child": true}
	if len(toRemove) != len(wantRemoved) {
		t.Fatalf("expected both parent and parent/child removed from the baseline in one cycle, got %v", toRemove)
	}
	for _, d := range toRemove {
		if !wantRemoved[d] {
			t.Errorf("unexpected baseline removal %q", d)
		}
	}
	if _, err := os.Stat(filepath.Join(localRoot, "parent")); !os.IsNotExist(err) {
		t.Fatalf("expected local parent (and child) fully removed, stat err = %v", err)
	}
}

// TestReconcileDirectories_StaleBaselineEntryGoneFromBothSidesIsCleanedUp
// checks that a baseline row for a directory now absent from BOTH sides
// (e.g. deleted on both independently, or cleaned up by cleanupEmptiedDirs
// on both sides in an earlier cycle) is swept from the baseline rather than
// lingering forever.
func TestReconcileDirectories_StaleBaselineEntryGoneFromBothSidesIsCleanedUp(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()
	logger := newTestLogger(t)

	toAdd, toRemove, failures := reconcileDirectories(logger, localRoot, remoteRoot, emptyDirSet(), emptyDirSet(), map[string]struct{}{"long-gone": {}}, noSkips())
	if failures != 0 {
		t.Fatalf("expected 0 failures, got %d", failures)
	}
	if len(toAdd) != 0 {
		t.Fatalf("expected no additions, got %v", toAdd)
	}
	if len(toRemove) != 1 || toRemove[0] != "long-gone" {
		t.Fatalf("expected stale baseline entry 'long-gone' removed, got %v", toRemove)
	}
}

func TestCleanupEmptiedDirs_RemovesOnlyDirsEmptiedByDeletes(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()

	// "emptied" existed on both sides with exactly the deleted file in it.
	if err := os.MkdirAll(filepath.Join(localRoot, "emptied"), 0o755); err != nil {
		t.Fatalf("mkdir local emptied: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(remoteRoot, "emptied"), 0o755); err != nil {
		t.Fatalf("mkdir remote emptied: %v", err)
	}

	// "still-has-files" also had a deletion, but still contains another file.
	if err := os.MkdirAll(filepath.Join(localRoot, "still-has-files"), 0o755); err != nil {
		t.Fatalf("mkdir local still-has-files: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "still-has-files", "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}

	// "untouched-empty" was already empty before this run and never had a
	// delete in it - must be left alone entirely.
	if err := os.MkdirAll(filepath.Join(localRoot, "untouched-empty"), 0o755); err != nil {
		t.Fatalf("mkdir untouched-empty: %v", err)
	}

	deleted := []string{"emptied/gone.txt", "still-has-files/gone.txt"}
	cleanupEmptiedDirs(localRoot, remoteRoot, deleted)

	if _, err := os.Stat(filepath.Join(localRoot, "emptied")); !os.IsNotExist(err) {
		t.Fatalf("expected local 'emptied' dir to be removed, err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "emptied")); !os.IsNotExist(err) {
		t.Fatalf("expected remote 'emptied' dir to be removed, err = %v", err)
	}
	if info, err := os.Stat(filepath.Join(localRoot, "still-has-files")); err != nil || !info.IsDir() {
		t.Fatalf("expected 'still-has-files' to remain (not empty), stat err = %v", err)
	}
	if info, err := os.Stat(filepath.Join(localRoot, "untouched-empty")); err != nil || !info.IsDir() {
		t.Fatalf("expected untouched pre-existing empty dir to be left alone, stat err = %v", err)
	}
}

// TestCleanupEmptiedDirs_CascadesUpMultipleAncestorLevels is a regression
// test for a review finding (Gemini 3.6): cleanupEmptiedDirs only ever
// computed path.Dir(relPath) - the deleted file's IMMEDIATE parent - and
// never re-examined that parent's own ancestors. Deleting
// "sub/nested/emptied/gone.txt" correctly removes "sub/nested/emptied", but
// that leaves "sub/nested" (now itself empty) and "sub" behind forever:
// reconcileDirectories only ever compares live scans against the baseline,
// and both are already present-on-both-sides-and-baselined, so neither
// orphan is ever revisited. Every ancestor up to (not including) the sync
// root must be considered, deepest first, so the emptiness cascades all the
// way up in one pass.
func TestCleanupEmptiedDirs_CascadesUpMultipleAncestorLevels(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()

	if err := os.MkdirAll(filepath.Join(localRoot, "sub", "nested", "emptied"), 0o755); err != nil {
		t.Fatalf("mkdir local sub/nested/emptied: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(remoteRoot, "sub", "nested", "emptied"), 0o755); err != nil {
		t.Fatalf("mkdir remote sub/nested/emptied: %v", err)
	}

	deleted := []string{"sub/nested/emptied/gone.txt"}
	cleanupEmptiedDirs(localRoot, remoteRoot, deleted)

	for _, root := range []string{localRoot, remoteRoot} {
		if _, err := os.Stat(filepath.Join(root, "sub")); !os.IsNotExist(err) {
			t.Errorf("expected %q's 'sub' ancestor chain fully cleaned up, stat err = %v", root, err)
		}
	}
}

// TestCleanupEmptiedDirs_StopsAtFirstNonEmptyAncestor ensures the cascade
// isn't unconditional: an ancestor that still holds an unrelated sibling
// must stop the walk right there, leaving everything above it (which was
// never actually emptied) untouched.
func TestCleanupEmptiedDirs_StopsAtFirstNonEmptyAncestor(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()

	if err := os.MkdirAll(filepath.Join(localRoot, "sub", "nested", "emptied"), 0o755); err != nil {
		t.Fatalf("mkdir local sub/nested/emptied: %v", err)
	}
	// "sub" has an unrelated sibling directory untouched by this run's
	// deletes - the cascade must stop before removing "sub" itself.
	if err := os.MkdirAll(filepath.Join(localRoot, "sub", "sibling"), 0o755); err != nil {
		t.Fatalf("mkdir local sub/sibling: %v", err)
	}

	deleted := []string{"sub/nested/emptied/gone.txt"}
	cleanupEmptiedDirs(localRoot, remoteRoot, deleted)

	if _, err := os.Stat(filepath.Join(localRoot, "sub", "nested")); !os.IsNotExist(err) {
		t.Errorf("expected 'sub/nested' removed (it was genuinely emptied), stat err = %v", err)
	}
	if info, err := os.Stat(filepath.Join(localRoot, "sub")); err != nil || !info.IsDir() {
		t.Errorf("expected 'sub' to survive - it still holds 'sibling', stat err = %v", err)
	}
	if info, err := os.Stat(filepath.Join(localRoot, "sub", "sibling")); err != nil || !info.IsDir() {
		t.Errorf("expected 'sub/sibling' untouched, stat err = %v", err)
	}
}
