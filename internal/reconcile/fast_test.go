package reconcile

import (
	"reflect"
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
	res := FastCompare(FastMirror, left, right, nil)
	assertHasTask(t, res, FastTask{Type: CopyL2R, RelPath: "a", Size: 1})
	assertHasTask(t, res, FastTask{Type: DeleteR, RelPath: "b"})
}

func TestFastTwoWayDetectsMove(t *testing.T) {
	prev := map[string]db.JobFileState{"old": {Path: "old", Size: 3, Mtime: 5}}
	left := map[string]db.JobFileState{"new": {Path: "new", Size: 3, Mtime: 5}}
	right := map[string]db.JobFileState{"old": {Path: "old", Size: 3, Mtime: 5}}
	res := FastCompare(FastTwoWay, left, right, prev)
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
		res := FastCompare(FastTwoWay, left, right, prev)
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

	for i := 0; i < 50; i++ {
		res := FastCompare(FastTwoWay, left, right, prev)
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

	res := FastCompare(FastTwoWay, left, right, prev)

	assertHasTask(t, res, FastTask{Type: MoveR, RelPath: "fileC.txt", OldRelPath: "fileA.txt"})
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

	res := FastCompare(FastTwoWay, left, right, prev)

	if len(res.Tasks) != 1 {
		t.Fatalf("Expected 1 task for conflict resolution, got %d: %+v", len(res.Tasks), res.Tasks)
	}
	if res.Tasks[0].Type != CopyL2R || res.Tasks[0].RelPath != "fileA.txt" {
		t.Errorf("Expected newer file in left to overwrite right, got %+v", res.Tasks[0])
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

	res := FastCompare(FastMirror, left, right, nil)

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
