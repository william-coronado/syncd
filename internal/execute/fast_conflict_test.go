package execute

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"syncd/internal/reconcile"
)

func conflictCopies(t *testing.T, dir, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix+"_conflict_") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestRunFastTasks_ConflictKeepsLosingSide(t *testing.T) {
	for _, tc := range []struct {
		typ           reconcile.FastTaskType
		winner, loser string // "left"/"right"
	}{
		{reconcile.ConflictL2R, "left", "right"},
		{reconcile.ConflictR2L, "right", "left"},
	} {
		t.Run(string(tc.typ), func(t *testing.T) {
			roots := map[string]string{"left": t.TempDir(), "right": t.TempDir()}
			writeAt(t, filepath.Join(roots[tc.winner], "doc.txt"), "winner edit", tNew)
			writeAt(t, filepath.Join(roots[tc.loser], "doc.txt"), "loser edit", tOld)

			if err := RunFastTasks(context.Background(), 1, roots["left"], roots["right"],
				[]reconcile.FastTask{{Type: tc.typ, RelPath: "doc.txt", Size: 11}}, nil); err != nil {
				t.Fatalf("RunFastTasks: %v", err)
			}
			if got := readString(t, filepath.Join(roots[tc.loser], "doc.txt")); got != "winner edit" {
				t.Fatalf("canonical on losing side = %q, want winner's content", got)
			}
			copies := conflictCopies(t, roots[tc.loser], "doc")
			if len(copies) != 1 {
				t.Fatalf("want 1 conflict copy on the losing side, got %v", copies)
			}
			if got := readString(t, filepath.Join(roots[tc.loser], copies[0])); got != "loser edit" {
				t.Fatalf("conflict copy = %q, want the losing edit", got)
			}
			if len(conflictCopies(t, roots[tc.winner], "doc")) != 0 {
				t.Fatal("conflict copy must be created on the losing side only")
			}
		})
	}
}

func TestRunFastTasks_ConflictWithMissingDestinationJustCopies(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	writeAt(t, filepath.Join(left, "doc.txt"), "only left", tNew)
	if err := RunFastTasks(context.Background(), 1, left, right,
		[]reconcile.FastTask{{Type: reconcile.ConflictL2R, RelPath: "doc.txt"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, filepath.Join(right, "doc.txt")); got != "only left" {
		t.Fatalf("got %q", got)
	}
	if len(conflictCopies(t, right, "doc")) != 0 {
		t.Fatal("no conflict copy expected when there was nothing to preserve")
	}
}

// If the winning copy fails, the losing side's file is put back where it
// was rather than left only under its conflict name.
func TestRunFastTasks_ConflictCopyFailureRestoresDestination(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	writeAt(t, filepath.Join(left, "doc.txt"), "winner", tNew)
	writeAt(t, filepath.Join(right, "doc.txt"), "loser", tOld)
	orig := copyFunc
	copyFunc = func(string, string) error { return errors.New("injected copy failure") }
	t.Cleanup(func() { copyFunc = orig })

	err := RunFastTasks(context.Background(), 1, left, right,
		[]reconcile.FastTask{{Type: reconcile.ConflictL2R, RelPath: "doc.txt"}}, nil)
	if err == nil {
		t.Fatal("want the injected failure")
	}
	if got := readString(t, filepath.Join(right, "doc.txt")); got != "loser" {
		t.Fatalf("destination not restored: %q", got)
	}
	if len(conflictCopies(t, right, "doc")) != 0 {
		t.Fatal("conflict copy left behind after rollback")
	}
}
