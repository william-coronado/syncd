package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"syncd/internal/reconcile"
)

// The pre-sync summary must say which directory is "left" and which is
// "right" - as absolute paths, with each root's role in this mode - since a
// summary read from a script's output otherwise only says "CopyL2R".
func TestSummaries_ShowAbsoluteRootsWithRoles(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	absLeft, _ := filepath.Abs("photos")
	absRight, _ := filepath.Abs("backup")

	cases := []struct {
		name      string
		summary   string
		leftLine  string
		rightLine string
	}{
		{"mirror", fastSummary(SyncOptions{Name: "j", Mode: reconcile.FastMirror, Left: "photos", Right: "backup"},
			[]reconcile.FastTask{{Type: reconcile.CopyL2R, RelPath: "a.jpg"}}),
			"left (source):", "right (destination):"},
		{"update", fastSummary(SyncOptions{Name: "j", Mode: reconcile.FastUpdate, Left: "photos", Right: "backup"}, nil),
			"left (source):", "right (destination):"},
		{"two-way", fastSummary(SyncOptions{Name: "j", Mode: reconcile.FastTwoWay, Left: "photos", Right: "backup"}, nil),
			"left:", "right:"},
		{"hash", robustSummary(SyncOptions{Name: "j", Left: "photos", Right: "backup"},
			[]reconcile.Action{{Type: reconcile.ActionUpload, Path: "a.jpg"}}),
			"left (local):", "right (remote):"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := strings.Split(tc.summary, "\n")
			if len(lines) < 3 {
				t.Fatalf("summary too short:\n%s", tc.summary)
			}
			// Roots come straight after the headline, before any task list.
			if !strings.Contains(lines[1], tc.leftLine) || !strings.HasSuffix(lines[1], absLeft) {
				t.Errorf("left root line = %q, want %q ... %s", lines[1], tc.leftLine, absLeft)
			}
			if !strings.Contains(lines[2], tc.rightLine) || !strings.HasSuffix(lines[2], absRight) {
				t.Errorf("right root line = %q, want %q ... %s", lines[2], tc.rightLine, absRight)
			}
			// The two paths line up in one column.
			if strings.Index(lines[1], absLeft) != strings.Index(lines[2], absRight) {
				t.Errorf("root paths not aligned:\n%s\n%s", lines[1], lines[2])
			}
		})
	}
}

func TestSummaries_GroupLabelsSayWhichWay(t *testing.T) {
	fast := fastSummary(SyncOptions{Name: "j", Mode: reconcile.FastTwoWay}, []reconcile.FastTask{
		{Type: reconcile.CopyL2R, RelPath: "a"}, {Type: reconcile.CopyR2L, RelPath: "b"},
		{Type: reconcile.DeleteL, RelPath: "c"}, {Type: reconcile.DeleteR, RelPath: "d"},
	})
	for _, want := range []string{"CopyL2R (copy left -> right)", "CopyR2L (copy right -> left)", "DeleteL (delete from left)", "DeleteR (delete from right)"} {
		if !strings.Contains(fast, want) {
			t.Errorf("fast summary missing %q:\n%s", want, fast)
		}
	}
	robust := robustSummary(SyncOptions{Name: "j"}, []reconcile.Action{
		{Type: reconcile.ActionUpload, Path: "a"}, {Type: reconcile.ActionDeleteRemote, Path: "b"},
	})
	for _, want := range []string{"UPLOAD (copy left -> right)", "DELETE_REMOTE (delete from right)"} {
		if !strings.Contains(robust, want) {
			t.Errorf("robust summary missing %q:\n%s", want, robust)
		}
	}
}

func TestSummaries_HighlightAppliesToRootPathsOnly(t *testing.T) {
	opts := SyncOptions{Name: "j", Mode: reconcile.FastMirror, Left: "/l", Right: "/r",
		HighlightPath: func(s string) string { return "<<" + s + ">>" }}
	out := fastSummary(opts, []reconcile.FastTask{{Type: reconcile.CopyL2R, RelPath: "f.txt"}})
	if !strings.Contains(out, "<</l>>") || !strings.Contains(out, "<</r>>") {
		t.Fatalf("root paths not highlighted:\n%s", out)
	}
	if strings.Count(out, "<<") != 2 {
		t.Fatalf("highlight applied beyond the two root paths:\n%s", out)
	}
}
