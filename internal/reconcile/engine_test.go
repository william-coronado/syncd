package reconcile

import (
	"testing"

	"syncd/internal/db"
	"syncd/internal/fs"
)

func entry(hash string) fs.FileEntry {
	return fs.FileEntry{Size: 1, Mtime: 1, Hash: hash}
}

func manifestRow(hash string) db.ManifestRow {
	return db.ManifestRow{Size: 1, Mtime: 1, Hash: hash}
}

func actionFor(t *testing.T, plan Plan, path string) Action {
	t.Helper()
	for _, a := range plan.Actions {
		if a.Path == path {
			return a
		}
	}
	t.Fatalf("no action found for path %q in plan %+v", path, plan)
	return Action{}
}

func TestBuildPlan(t *testing.T) {
	const path = "file.txt"

	tests := []struct {
		name     string
		local    map[string]fs.FileEntry
		remote   map[string]fs.FileEntry
		manifest map[string]db.ManifestRow
		skipped  map[string]struct{}
		want     ActionType
	}{
		{
			name:   "hashes match on both sides is a no-op",
			local:  map[string]fs.FileEntry{path: entry("A")},
			remote: map[string]fs.FileEntry{path: entry("A")},
			want:   ActionNoOp,
		},
		{
			name:   "differing content with no manifest row is a conflict",
			local:  map[string]fs.FileEntry{path: entry("A")},
			remote: map[string]fs.FileEntry{path: entry("B")},
			want:   ActionConflict,
		},
		{
			name:     "local changed, remote unchanged is an upload",
			local:    map[string]fs.FileEntry{path: entry("A2")},
			remote:   map[string]fs.FileEntry{path: entry("A")},
			manifest: map[string]db.ManifestRow{path: manifestRow("A")},
			want:     ActionUpload,
		},
		{
			name:     "remote changed, local unchanged is a download",
			local:    map[string]fs.FileEntry{path: entry("A")},
			remote:   map[string]fs.FileEntry{path: entry("A2")},
			manifest: map[string]db.ManifestRow{path: manifestRow("A")},
			want:     ActionDownload,
		},
		{
			name:     "both changed since manifest is a conflict",
			local:    map[string]fs.FileEntry{path: entry("A2")},
			remote:   map[string]fs.FileEntry{path: entry("A3")},
			manifest: map[string]db.ManifestRow{path: manifestRow("A")},
			want:     ActionConflict,
		},
		{
			name:  "new local-only file is an upload",
			local: map[string]fs.FileEntry{path: entry("A")},
			want:  ActionUpload,
		},
		{
			name:   "new remote-only file is a download",
			remote: map[string]fs.FileEntry{path: entry("A")},
			want:   ActionDownload,
		},
		{
			name:     "local changed while remote deleted preserves the local file",
			local:    map[string]fs.FileEntry{path: entry("A2")},
			manifest: map[string]db.ManifestRow{path: manifestRow("A")},
			want:     ActionUpload,
		},
		{
			name:     "remote deleted, local unchanged propagates the delete locally",
			manifest: map[string]db.ManifestRow{path: manifestRow("A")},
			local:    map[string]fs.FileEntry{path: entry("A")},
			want:     ActionDeleteLocal,
		},
		{
			name:     "remote changed while local deleted preserves the remote file",
			remote:   map[string]fs.FileEntry{path: entry("A2")},
			manifest: map[string]db.ManifestRow{path: manifestRow("A")},
			want:     ActionDownload,
		},
		{
			name:     "local deleted, remote unchanged propagates the delete remotely",
			manifest: map[string]db.ManifestRow{path: manifestRow("A")},
			remote:   map[string]fs.FileEntry{path: entry("A")},
			want:     ActionDeleteRemote,
		},
		{
			name:     "already deleted on both sides is a no-op",
			manifest: map[string]db.ManifestRow{path: manifestRow("A")},
			want:     ActionNoOp,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := BuildPlan(tt.local, tt.remote, tt.manifest, NewSkipSet(tt.skipped, nil))
			got := actionFor(t, plan, path)
			if got.Type != tt.want {
				t.Fatalf("action type = %s, want %s (reason: %s)", got.Type, tt.want, got.Reason)
			}
		})
	}
}

func TestBuildPlan_SkippedPathExcluded(t *testing.T) {
	const path = "bad:name.txt"
	local := map[string]fs.FileEntry{path: entry("A")}
	remote := map[string]fs.FileEntry{path: entry("B")}
	skipped := NewSkipSet(map[string]struct{}{path: {}}, nil)

	plan := BuildPlan(local, remote, nil, skipped)
	for _, a := range plan.Actions {
		if a.Path == path {
			t.Fatalf("expected skipped path to be excluded from plan, found action %+v", a)
		}
	}
}

// TestBuildPlan_SkippedDirSuppressesDescendantActions is a regression test
// for Critical 2: an unreadable local subdirectory ("secret") means its
// descendants were never enumerated locally, so they appear only in remote
// (from the manifest's steady-state perspective too). Without subtree-aware
// skip suppression, BuildPlan reads each descendant as "locally deleted,
// propagate the delete" and emits DELETE_REMOTE for files that are actually
// fine - they just live under a directory this scan couldn't read. This must
// produce NO action at all for anything under the skipped directory.
func TestBuildPlan_SkippedDirSuppressesDescendantActions(t *testing.T) {
	remote := map[string]fs.FileEntry{
		"secret/a.txt": entry("A"),
		"secret/b.txt": entry("B"),
		"keep.txt":     entry("K"),
	}
	manifest := map[string]db.ManifestRow{
		"secret/a.txt": manifestRow("A"),
		"secret/b.txt": manifestRow("B"),
		"keep.txt":     manifestRow("K"),
	}
	local := map[string]fs.FileEntry{
		"keep.txt": entry("K"),
	}
	skipped := NewSkipSet(map[string]struct{}{"secret": {}}, map[string]struct{}{"secret": {}})

	plan := BuildPlan(local, remote, manifest, skipped)

	for _, a := range plan.Actions {
		if a.Path == "secret/a.txt" || a.Path == "secret/b.txt" {
			t.Fatalf("expected descendants of skipped dir 'secret' to be excluded from the plan, got action %+v", a)
		}
	}
	// keep.txt (outside the skipped subtree) must still be processed
	// normally - the skip must not suppress unrelated paths.
	for _, a := range plan.Actions {
		if a.Path == "keep.txt" && a.Type != ActionNoOp {
			t.Fatalf("expected keep.txt to be a no-op (hashes match), got %+v", a)
		}
	}
}

// TestBuildPlan_AllSkipReasonsSuppressActions is the BuildPlan-level half of
// TestFastMirror_AllSkipReasonsSuppressTasks - see its doc comment for the
// full rationale. Each scenario sets up a path present in the manifest and
// on remote (so, absent the skip, BuildPlan would read it as "locally
// deleted; propagate delete" and emit ActionDeleteRemote) but skipped this
// scan for a different reason.
func TestBuildPlan_AllSkipReasonsSuppressActions(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		skipped SkipSet
	}{
		{
			name:    "symlink",
			path:    "link.txt",
			skipped: NewSkipSet(map[string]struct{}{"link.txt": {}}, nil),
		},
		{
			name:    "unreadable file",
			path:    "secret.txt",
			skipped: NewSkipSet(map[string]struct{}{"secret.txt": {}}, nil),
		},
		{
			name:    "unreadable subdirectory",
			path:    "locked/inside.txt",
			skipped: NewSkipSet(map[string]struct{}{"locked": {}}, map[string]struct{}{"locked": {}}),
		},
		{
			name:    "path vanishing mid-scan",
			path:    "flaky.txt",
			skipped: NewSkipSet(map[string]struct{}{"flaky.txt": {}}, nil),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remote := map[string]fs.FileEntry{tc.path: entry("A")}
			manifest := map[string]db.ManifestRow{tc.path: manifestRow("A")}
			plan := BuildPlan(nil, remote, manifest, tc.skipped)
			for _, a := range plan.Actions {
				if a.Path == tc.path {
					t.Fatalf("expected no action for skipped path (%s), got %+v", tc.name, a)
				}
			}
		})
	}
}

func TestBuildPlan_ActionsAreSortedByTypeThenPath(t *testing.T) {
	local := map[string]fs.FileEntry{
		"b.txt": entry("A"),
		"a.txt": entry("A"),
	}
	remote := map[string]fs.FileEntry{}

	plan := BuildPlan(local, remote, nil, SkipSet{})
	if len(plan.Actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(plan.Actions))
	}
	if plan.Actions[0].Path != "a.txt" || plan.Actions[1].Path != "b.txt" {
		t.Fatalf("expected actions sorted by path within the same type, got %+v", plan.Actions)
	}
}
