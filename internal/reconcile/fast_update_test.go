package reconcile

import (
	"testing"

	"syncd/internal/db"
)

// update follows rsync --update: copy a missing or older destination, never
// a newer one (docs/follow-ups.md #24 - it used to copy whenever the sizes
// differed, overwriting a newer destination edit).
func TestFastUpdate_Semantics(t *testing.T) {
	st := func(size, mtime int64) db.JobFileState { return db.JobFileState{Size: size, Mtime: mtime} }
	cases := []struct {
		name     string
		l        db.JobFileState
		r        *db.JobFileState
		wantCopy bool
	}{
		{"destination missing", st(5, 100), nil, true},
		{"source newer, same size", st(5, 200), &[]db.JobFileState{st(5, 100)}[0], true},
		{"source newer, different size", st(9, 200), &[]db.JobFileState{st(5, 100)}[0], true},
		{"identical", st(5, 100), &[]db.JobFileState{st(5, 100)}[0], false},
		{"same mtime, different size", st(9, 100), &[]db.JobFileState{st(5, 100)}[0], true},
		{"destination newer, same size", st(5, 100), &[]db.JobFileState{st(5, 200)}[0], false},
		{"destination newer, different size", st(9, 100), &[]db.JobFileState{st(5, 200)}[0], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			left := map[string]db.JobFileState{"f": tc.l}
			right := map[string]db.JobFileState{}
			if tc.r != nil {
				right["f"] = *tc.r
			}
			res := FastCompare(FastUpdate, left, right, nil, SkipSet{})
			gotCopy := false
			for _, task := range res.Tasks {
				if task.Type == CopyL2R && task.RelPath == "f" {
					gotCopy = true
				} else {
					t.Fatalf("unexpected task %+v", task)
				}
			}
			if gotCopy != tc.wantCopy {
				t.Fatalf("copy = %v, want %v", gotCopy, tc.wantCopy)
			}
		})
	}
}

// update never deletes or copies right-to-left.
func TestFastUpdate_RightOnlyFileIsLeftAlone(t *testing.T) {
	res := FastCompare(FastUpdate, map[string]db.JobFileState{}, map[string]db.JobFileState{"r": {Size: 1, Mtime: 1}}, nil, SkipSet{})
	if len(res.Tasks) != 0 {
		t.Fatalf("want no tasks, got %+v", res.Tasks)
	}
}
