package reconcile

import (
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
