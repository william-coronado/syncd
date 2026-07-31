package reconcile

import (
	"reflect"
	"strings"
	"testing"

	"syncd/internal/db"
)

// assertHasTask scans res.Tasks (ignoring order) for a task matching want on
// Type, RelPath, and OldRelPath. Size is only checked when want.Size != 0.
func assertHasTask(t *testing.T, res FastResult, want FastTask) {
	t.Helper()
	for _, got := range res.Tasks {
		if got.Type != want.Type || got.RelPath != want.RelPath || got.OldRelPath != want.OldRelPath {
			continue
		}
		if want.Size != 0 && got.Size != want.Size {
			continue
		}
		return
	}
	t.Errorf("expected task %+v not found in %+v", want, res.Tasks)
}

// --- Cases from the task brief ---

func TestFastMirrorCopiesNewAndDeletesMissing(t *testing.T) {
	left := map[string]db.JobFileState{"a": {Path: "a", Size: 1, Mtime: 10}}
	right := map[string]db.JobFileState{"b": {Path: "b", Size: 1, Mtime: 10}}
	res := FastCompare(FastMirror, left, right, nil, SkipSet{})
	assertHasTask(t, res, FastTask{Type: CopyL2R, RelPath: "a", Size: 1})
	assertHasTask(t, res, FastTask{Type: DeleteR, RelPath: "b"})
}

func TestFastTwoWayDetectsMove(t *testing.T) {
	prev := map[string]db.JobFileState{"old": {Path: "old", Size: 3, Mtime: 5}}
	left := map[string]db.JobFileState{"new": {Path: "new", Size: 3, Mtime: 5}}
	right := map[string]db.JobFileState{"old": {Path: "old", Size: 3, Mtime: 5}}
	res := FastCompare(FastTwoWay, left, right, prev, SkipSet{})
	assertHasTask(t, res, FastTask{Type: MoveR, RelPath: "new", OldRelPath: "old"})
}

// TestFastTwoWayMoveIsDeterministic guards against the race described in
// review-fixes task 4: fast.go used to iterate `allPaths` (a Go map) in a
// single pass where `processedMoves[oldPath]` was only set once the *new*
// path was visited, but the delete branch fired as soon as the *old* path
// was visited. When map order happened to visit the old path first, a run
// emitted BOTH a spurious DeleteR for the old path AND the correct MoveR -
// two different task sets from identical inputs, observed ~13.5% of the
// time. Since execute.RunFastTasks runs tasks concurrently, a stray DeleteR
// racing the MoveR can destroy the file's only remaining copy.
//
// This asserts every one of >=200 runs on the same inputs produces the
// exact same, complete task list (and the same NewState, since that is
// persisted as the next run's baseline).
func TestFastTwoWayMoveIsDeterministic(t *testing.T) {
	prev := map[string]db.JobFileState{"old.txt": {Path: "old.txt", Size: 3, Mtime: 5}}
	left := map[string]db.JobFileState{"new.txt": {Path: "new.txt", Size: 3, Mtime: 5}}
	right := map[string]db.JobFileState{"old.txt": {Path: "old.txt", Size: 3, Mtime: 5}}

	wantTasks := []FastTask{
		{Type: MoveR, RelPath: "new.txt", OldRelPath: "old.txt"},
	}
	wantState := map[string]db.JobFileState{
		"new.txt": {Path: "new.txt", Size: 3, Mtime: 5},
	}

	const iterations = 200
	for i := 0; i < iterations; i++ {
		res := FastCompare(FastTwoWay, left, right, prev, SkipSet{})
		if !reflect.DeepEqual(res.Tasks, wantTasks) {
			t.Fatalf("run %d: task list not deterministic/correct: got %+v, want %+v", i, res.Tasks, wantTasks)
		}
		if !reflect.DeepEqual(res.NewState, wantState) {
			t.Fatalf("run %d: NewState not deterministic/correct: got %+v, want %+v", i, res.NewState, wantState)
		}
	}
}

// TestFastTwoWaySignatureCollisionFallsBackToCopyDelete: two files deleted
// from left share the same {Size,Mtime} signature. leftMissingSigs/
// rightMissingSigs are keyed by signature alone, so a naive implementation
// silently drops one of the two candidates and may pick an arbitrary (wrong)
// move. The safe behavior is to treat a colliding signature as "no match" -
// never emit a move for it - and fall through to copy/delete, which is
// always correct (if slower).
func TestFastTwoWaySignatureCollisionFallsBackToCopyDelete(t *testing.T) {
	prev := map[string]db.JobFileState{
		"old1.txt": {Path: "old1.txt", Size: 5, Mtime: 100},
		"old2.txt": {Path: "old2.txt", Size: 5, Mtime: 100},
	}
	left := map[string]db.JobFileState{
		"new.txt": {Path: "new.txt", Size: 5, Mtime: 100},
	}
	right := map[string]db.JobFileState{
		"old1.txt": {Path: "old1.txt", Size: 5, Mtime: 100},
		"old2.txt": {Path: "old2.txt", Size: 5, Mtime: 100},
	}
	wantState := map[string]db.JobFileState{
		"new.txt": {Path: "new.txt", Size: 5, Mtime: 100},
	}

	for i := 0; i < 50; i++ {
		res := FastCompare(FastTwoWay, left, right, prev, SkipSet{})
		for _, task := range res.Tasks {
			if task.Type == MoveR || task.Type == MoveL {
				t.Fatalf("run %d: expected no move for colliding signatures, got %+v in %+v", i, task, res.Tasks)
			}
		}
		if len(res.Tasks) != 3 {
			t.Fatalf("run %d: expected exactly 3 tasks (1 copy + 2 deletes), got %d: %+v", i, len(res.Tasks), res.Tasks)
		}
		assertHasTask(t, res, FastTask{Type: CopyL2R, RelPath: "new.txt", Size: 5})
		assertHasTask(t, res, FastTask{Type: DeleteR, RelPath: "old1.txt"})
		assertHasTask(t, res, FastTask{Type: DeleteR, RelPath: "old2.txt"})
		if !reflect.DeepEqual(res.NewState, wantState) {
			t.Fatalf("run %d: NewState not deterministic/correct: got %+v, want %+v", i, res.NewState, wantState)
		}
	}
}

// TestFastTwoWaySignatureCollisionFallsBackToCopyDelete_RightMissingDirection
// mirrors TestFastTwoWaySignatureCollisionFallsBackToCopyDelete on the other
// side: two files vanished from RIGHT (not left) since prev, colliding on
// signature. rightMissingSigs is keyed by signature alone exactly like
// leftMissingSigs, so the same "a collision means no match" safety rule must
// hold in this direction too - structurally identical to the left-missing
// case but previously untested (docs/follow-ups.md #5).
func TestFastTwoWaySignatureCollisionFallsBackToCopyDelete_RightMissingDirection(t *testing.T) {
	prev := map[string]db.JobFileState{
		"old1.txt": {Path: "old1.txt", Size: 5, Mtime: 100},
		"old2.txt": {Path: "old2.txt", Size: 5, Mtime: 100},
	}
	left := map[string]db.JobFileState{
		"old1.txt": {Path: "old1.txt", Size: 5, Mtime: 100},
		"old2.txt": {Path: "old2.txt", Size: 5, Mtime: 100},
	}
	right := map[string]db.JobFileState{
		"new.txt": {Path: "new.txt", Size: 5, Mtime: 100},
	}
	wantState := map[string]db.JobFileState{
		"new.txt": {Path: "new.txt", Size: 5, Mtime: 100},
	}

	for i := 0; i < 50; i++ {
		res := FastCompare(FastTwoWay, left, right, prev, SkipSet{})
		for _, task := range res.Tasks {
			if task.Type == MoveR || task.Type == MoveL {
				t.Fatalf("run %d: expected no move for colliding signatures, got %+v in %+v", i, task, res.Tasks)
			}
		}
		if len(res.Tasks) != 3 {
			t.Fatalf("run %d: expected exactly 3 tasks (1 copy + 2 deletes), got %d: %+v", i, len(res.Tasks), res.Tasks)
		}
		assertHasTask(t, res, FastTask{Type: CopyR2L, RelPath: "new.txt", Size: 5})
		assertHasTask(t, res, FastTask{Type: DeleteL, RelPath: "old1.txt"})
		assertHasTask(t, res, FastTask{Type: DeleteL, RelPath: "old2.txt"})
		if !reflect.DeepEqual(res.NewState, wantState) {
			t.Fatalf("run %d: NewState not deterministic/correct: got %+v, want %+v", i, res.NewState, wantState)
		}
	}
}

// --- Skip-suppression regression tests (review-fixes Criticals 1 & 2) ---

// TestFastMirror_SkippedExactPathSuppressesDelete is a regression test for
// Critical 1: left has an unreadable/symlinked counterpart for a path that
// really exists (as a regular file) on the right. Before this fix,
// FastCompare had no way to know the left-side path was merely unreadable
// rather than absent, so mirror's set-difference logic concluded "gone from
// left" and emitted a DeleteR that would destroy the real file on the right.
func TestFastMirror_SkippedExactPathSuppressesDelete(t *testing.T) {
	left := map[string]db.JobFileState{
		"target.txt": {Path: "target.txt", Size: 1, Mtime: 10},
	}
	right := map[string]db.JobFileState{
		"link.txt": {Path: "link.txt", Size: 5, Mtime: 20},
	}
	skipped := NewSkipSet(map[string]struct{}{"link.txt": {}}, nil)

	res := FastCompare(FastMirror, left, right, nil, skipped)

	for _, task := range res.Tasks {
		if task.RelPath == "link.txt" {
			t.Fatalf("expected no task for skipped path link.txt, got %+v", task)
		}
	}
}

// TestFastMirror_SkippedDirSuppressesDescendantDeletes is a regression test
// for Critical 2: an unreadable local subdirectory ("secret") means its
// descendants were never scanned locally, so they're absent from `left`
// entirely - but they exist on `right`. Without subtree-aware skip
// suppression, mirror's right-iteration loop reads every descendant as
// "gone from left" and emits DeleteR for each, mass-deleting a subtree the
// scan simply couldn't read - the exact scenario from the review evidence.
func TestFastMirror_SkippedDirSuppressesDescendantDeletes(t *testing.T) {
	left := map[string]db.JobFileState{
		"keep.txt": {Path: "keep.txt", Size: 1, Mtime: 10},
	}
	right := map[string]db.JobFileState{
		"keep.txt":     {Path: "keep.txt", Size: 1, Mtime: 10},
		"secret/a.txt": {Path: "secret/a.txt", Size: 2, Mtime: 10},
		"secret/b.txt": {Path: "secret/b.txt", Size: 3, Mtime: 10},
	}
	skipped := NewSkipSet(map[string]struct{}{"secret": {}}, map[string]struct{}{"secret": {}})

	res := FastCompare(FastMirror, left, right, nil, skipped)

	for _, task := range res.Tasks {
		if strings.HasPrefix(task.RelPath, "secret/") {
			t.Fatalf("expected no task under skipped dir 'secret', got %+v in %+v", task, res.Tasks)
		}
	}
}

// TestFastTwoWay_SkippedPathNeverTreatedAsDeleted mirrors the same
// suppression requirement for two-way mode: a path previously synced
// (present in prev) that is skipped this scan on one side must not be
// classified as a delete-to-propagate, and must not participate in move
// detection either.
func TestFastTwoWay_SkippedPathNeverTreatedAsDeleted(t *testing.T) {
	prev := map[string]db.JobFileState{
		"secret/a.txt": {Path: "secret/a.txt", Size: 2, Mtime: 10},
	}
	left := map[string]db.JobFileState{} // "secret" unreadable locally this scan
	right := map[string]db.JobFileState{
		"secret/a.txt": {Path: "secret/a.txt", Size: 2, Mtime: 10},
	}
	skipped := NewSkipSet(nil, map[string]struct{}{"secret": {}})

	res := FastCompare(FastTwoWay, left, right, prev, skipped)

	for _, task := range res.Tasks {
		t.Fatalf("expected no tasks for a skipped path, got %+v", task)
	}
	if got, ok := res.NewState["secret/a.txt"]; !ok || got != prev["secret/a.txt"] {
		t.Errorf("expected NewState to preserve prev baseline for skipped path, got %+v (ok=%v)", got, ok)
	}
}

// TestFastMirror_AllSkipReasonsSuppressTasks is the reconciler-level half of
// the review's required cross product: each of the four scan-time conditions
// that populate a fs.ScanResult's Skipped/SkippedDirs - a symlink, an
// unreadable file, an unreadable subdirectory, and a path that vanished
// between readdir and stat (fs.ScanTree's "stat file: ..." issue) - must
// suppress FastCompare task generation the same way. FastCompare itself is
// agnostic to *why* a path was skipped (only fs.ScanTree attaches the
// Reason text); what matters here is that every one of these shapes
// (exact-path skip for the first, second, and fourth; dir-prefix skip for
// the third) is honored identically.
func TestFastMirror_AllSkipReasonsSuppressTasks(t *testing.T) {
	cases := []struct {
		name    string
		right   map[string]db.JobFileState // present on right only, so mirror would otherwise DeleteR
		skipped SkipSet
	}{
		{
			name:    "symlink",
			right:   map[string]db.JobFileState{"link.txt": {Path: "link.txt", Size: 1, Mtime: 1}},
			skipped: NewSkipSet(map[string]struct{}{"link.txt": {}}, nil),
		},
		{
			name:    "unreadable file",
			right:   map[string]db.JobFileState{"secret.txt": {Path: "secret.txt", Size: 1, Mtime: 1}},
			skipped: NewSkipSet(map[string]struct{}{"secret.txt": {}}, nil),
		},
		{
			name:    "unreadable subdirectory",
			right:   map[string]db.JobFileState{"locked/inside.txt": {Path: "locked/inside.txt", Size: 1, Mtime: 1}},
			skipped: NewSkipSet(map[string]struct{}{"locked": {}}, map[string]struct{}{"locked": {}}),
		},
		{
			name:    "path vanishing mid-scan",
			right:   map[string]db.JobFileState{"flaky.txt": {Path: "flaky.txt", Size: 1, Mtime: 1}},
			skipped: NewSkipSet(map[string]struct{}{"flaky.txt": {}}, nil),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			left := map[string]db.JobFileState{} // nothing on left this scan (unknown/unreadable)
			res := FastCompare(FastMirror, left, tc.right, nil, tc.skipped)
			if len(res.Tasks) != 0 {
				t.Fatalf("expected no tasks when the only right-side path is skipped (%s), got %+v", tc.name, res.Tasks)
			}
		})
	}
}

// --- Cases ported from file_sync_util/internal/sync/compare_test.go ---

// Ported from TestCompareTwoWayMoves: left moved fileA.txt to fileC.txt,
// right did nothing -> expect a MoveR task from fileA.txt to fileC.txt.
func TestFastTwoWayMoves(t *testing.T) {
	const now int64 = 1_000_000

	prev := map[string]db.JobFileState{
		"fileA.txt": {Path: "fileA.txt", Size: 100, Mtime: now},
		"fileB.txt": {Path: "fileB.txt", Size: 200, Mtime: now},
	}

	left := map[string]db.JobFileState{
		"fileC.txt": {Path: "fileC.txt", Size: 100, Mtime: now},
		"fileB.txt": {Path: "fileB.txt", Size: 200, Mtime: now},
	}

	right := map[string]db.JobFileState{
		"fileA.txt": {Path: "fileA.txt", Size: 100, Mtime: now},
		"fileB.txt": {Path: "fileB.txt", Size: 200, Mtime: now},
	}

	res := FastCompare(FastTwoWay, left, right, prev, SkipSet{})

	assertHasTask(t, res, FastTask{Type: MoveR, RelPath: "fileC.txt", OldRelPath: "fileA.txt"})
}

// assertNoTaskReferencesOldPath fails if any task in res.Tasks (move or
// otherwise) has an OldRelPath equal to oldPath - i.e. no task depends on
// oldPath still existing wherever it's about to be renamed from.
func assertNoTaskReferencesOldPath(t *testing.T, res FastResult, oldPath string) {
	t.Helper()
	for _, got := range res.Tasks {
		if got.OldRelPath == oldPath {
			t.Errorf("expected no task referencing OldRelPath %q, got %+v", oldPath, got)
		}
	}
}

// TestFastTwoWayMoveVsDelete_FallsBackToCopyInsteadOfUnexecutableMove is a
// regression test for a review finding: prev has "f" on both sides. Left
// renames f->g; right independently deletes f. The old code matched g's
// signature against leftMissingSigs["f"] with no check that right still HAS
// f, and emitted MoveR(g, oldPath=f) unconditionally. moveFastFile then tries
// os.Rename(right/f, right/g), which fails - right already deleted f - and
// RunFastTasks aborts the ENTIRE batch on that single error, so unrelated
// tasks in the same run never execute either.
//
// Once right has genuinely lost f (not renamed, actually gone), there is
// nothing on the right side to rename - the only executable outcome is
// copying left's g to right as a new file, same as any other lChanged path
// with no counterpart. No move task should be emitted here at all.
func TestFastTwoWayMoveVsDelete_FallsBackToCopyInsteadOfUnexecutableMove(t *testing.T) {
	prev := map[string]db.JobFileState{
		"f": {Path: "f", Size: 10, Mtime: 5},
	}
	left := map[string]db.JobFileState{
		"g": {Path: "g", Size: 10, Mtime: 5}, // left renamed f -> g
	}
	right := map[string]db.JobFileState{} // right deleted f

	res := FastCompare(FastTwoWay, left, right, prev, SkipSet{})

	assertNoTaskReferencesOldPath(t, res, "f")
	assertHasTask(t, res, FastTask{Type: CopyL2R, RelPath: "g", Size: 10})
}

// TestFastTwoWayMoveVsMove_FallsBackToCopyInsteadOfUnexecutableMoves is a
// regression test for the same class of bug: prev has "f" on both sides.
// Left renames f->g; right independently renames f->h (a different name).
// The old code emitted BOTH MoveR(g, oldPath=f) - which fails because right
// no longer has f, it has h - AND MoveL(h, oldPath=f) - which fails because
// left no longer has f, it has g. Two guaranteed-failing tasks, aborting the
// whole batch. The only executable outcome is copying each side's new name
// to the other side.
func TestFastTwoWayMoveVsMove_FallsBackToCopyInsteadOfUnexecutableMoves(t *testing.T) {
	prev := map[string]db.JobFileState{
		"f": {Path: "f", Size: 10, Mtime: 5},
	}
	left := map[string]db.JobFileState{
		"g": {Path: "g", Size: 10, Mtime: 5}, // left renamed f -> g
	}
	right := map[string]db.JobFileState{
		"h": {Path: "h", Size: 10, Mtime: 5}, // right renamed f -> h
	}

	res := FastCompare(FastTwoWay, left, right, prev, SkipSet{})

	assertNoTaskReferencesOldPath(t, res, "f")
	assertHasTask(t, res, FastTask{Type: CopyL2R, RelPath: "g", Size: 10})
	assertHasTask(t, res, FastTask{Type: CopyR2L, RelPath: "h", Size: 10})
}

// Ported from TestCompareTwoWayConflict: both sides modified fileA.txt;
// left is newer -> expect a single CopyL2R task, newer wins.
func TestFastTwoWayConflict(t *testing.T) {
	const now int64 = 1_000_000
	const hour int64 = 3600
	const minute int64 = 60

	prev := map[string]db.JobFileState{
		"fileA.txt": {Path: "fileA.txt", Size: 100, Mtime: now - hour},
	}

	left := map[string]db.JobFileState{
		"fileA.txt": {Path: "fileA.txt", Size: 150, Mtime: now},
	}
	right := map[string]db.JobFileState{
		"fileA.txt": {Path: "fileA.txt", Size: 200, Mtime: now - minute},
	}

	res := FastCompare(FastTwoWay, left, right, prev, SkipSet{})

	if len(res.Tasks) != 1 {
		t.Fatalf("Expected 1 task for conflict resolution, got %d: %+v", len(res.Tasks), res.Tasks)
	}
	if res.Tasks[0].Type != CopyL2R || res.Tasks[0].RelPath != "fileA.txt" {
		t.Errorf("Expected newer file in left to overwrite right, got %+v", res.Tasks[0])
	}
}

// TestFastTwoWayEqualMtimeConflictDoesNotFabricateBaseline is a regression
// test for review finding "Medium 8": when both sides changed since prev but
// their mtimes tie, neither the `>` nor the `<` branch fires, so no copy is
// queued - but the code then unconditionally set newState[p] = lState,
// asserting the pair as "identical" even though their sizes (and, in
// reality, their content) differ. A later run would then diff against that
// fabricated baseline: left's real, distinct content is now the recorded
// "last known" state, so right (genuinely unchanged since this run) reads as
// "changed" and gets force-copied over left - silently discarding left's own
// real edit.
func TestFastTwoWayEqualMtimeConflictDoesNotFabricateBaseline(t *testing.T) {
	const now int64 = 1_000_000

	prev := map[string]db.JobFileState{
		"fileA.txt": {Path: "fileA.txt", Size: 100, Mtime: now - 3600},
	}
	left := map[string]db.JobFileState{
		"fileA.txt": {Path: "fileA.txt", Size: 150, Mtime: now}, // changed since prev
	}
	right := map[string]db.JobFileState{
		"fileA.txt": {Path: "fileA.txt", Size: 200, Mtime: now}, // changed since prev, same mtime as left
	}

	res := FastCompare(FastTwoWay, left, right, prev, SkipSet{})

	if len(res.Tasks) != 0 {
		t.Fatalf("expected no task for an unresolvable equal-mtime conflict, got %+v", res.Tasks)
	}
	if got, ok := res.NewState["fileA.txt"]; ok {
		t.Fatalf("expected no fabricated baseline for the unresolved conflict, got %+v", got)
	}

	// Run again using this run's NewState as the next run's prev (exactly
	// what the caller persists - see sync_once.go's repo.Replace). Nothing
	// genuinely changed on either side between the two runs. This must
	// re-detect the same ambiguous conflict (and still queue no task) rather
	// than reading right as newly-changed-since-baseline and force-copying
	// it over left's real, distinct content.
	res2 := FastCompare(FastTwoWay, left, right, res.NewState, SkipSet{})
	if len(res2.Tasks) != 0 {
		t.Fatalf("expected the unresolved conflict to persist across runs without a fabricated baseline, got %+v", res2.Tasks)
	}
}

// Ported from TestCompareMirror: new file, updated file, and a file only on
// the right (to be deleted).
func TestFastMirror(t *testing.T) {
	const now int64 = 1_000_000
	const hour int64 = 3600

	left := map[string]db.JobFileState{
		"new.txt":    {Path: "new.txt", Size: 50, Mtime: now},
		"update.txt": {Path: "update.txt", Size: 200, Mtime: now},
	}

	right := map[string]db.JobFileState{
		"update.txt": {Path: "update.txt", Size: 100, Mtime: now - hour},
		"delete.txt": {Path: "delete.txt", Size: 50, Mtime: now},
	}

	res := FastCompare(FastMirror, left, right, nil, SkipSet{})

	if len(res.Tasks) != 3 {
		t.Fatalf("Expected 3 tasks, got %d: %+v", len(res.Tasks), res.Tasks)
	}

	var deletes, updates, news int
	for _, task := range res.Tasks {
		switch task.Type {
		case DeleteR:
			if task.RelPath == "delete.txt" {
				deletes++
			}
		case CopyL2R:
			if task.RelPath == "update.txt" {
				updates++
			}
			if task.RelPath == "new.txt" {
				news++
			}
		}
	}
	if deletes != 1 || updates != 1 || news != 1 {
		t.Errorf("Unexpected tasks distribution: %+v", res.Tasks)
	}
}
