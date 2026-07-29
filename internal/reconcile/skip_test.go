package reconcile

import "testing"

func TestSkipSet_ExactMatch(t *testing.T) {
	s := NewSkipSet(map[string]struct{}{"link.txt": {}}, nil)
	if !s.Contains("link.txt") {
		t.Error("expected exact match to be skipped")
	}
	if s.Contains("other.txt") {
		t.Error("expected unrelated path not to be skipped")
	}
}

func TestSkipSet_DirPrefixMatchesDescendants(t *testing.T) {
	// Only Dirs is populated here (not Exact) to isolate the prefix-matching
	// behavior: a directory's own relpath is never itself a descendant of
	// its own prefix set (dirPrefixes never includes path itself) - in
	// practice fs.ScanTree always records a skipped directory's own relpath
	// in BOTH Skipped (exact) and SkippedDirs, so the combined SkipSet built
	// by app.mergeSkipSet always covers the directory's own path too (see
	// TestSkipSet_DirOwnPathAlsoSkippedWhenBothSetsPopulated below).
	s := NewSkipSet(nil, map[string]struct{}{"secret": {}})
	cases := []struct {
		path string
		want bool
	}{
		{"secret/a.txt", true},
		{"secret/sub/b.txt", true},
		{"secretive/x.txt", false},
		{"other/secret.txt", false},
	}
	for _, tc := range cases {
		if got := s.Contains(tc.path); got != tc.want {
			t.Errorf("Contains(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestSkipSet_DirOwnPathAlsoSkippedWhenBothSetsPopulated pins the realistic
// case: fs.ScanTree records an unreadable directory's own relpath in both
// Skipped and SkippedDirs, so a SkipSet built from both (as app.mergeSkipSet
// does) treats the directory's own path as skipped too, not just its
// descendants.
func TestSkipSet_DirOwnPathAlsoSkippedWhenBothSetsPopulated(t *testing.T) {
	s := NewSkipSet(map[string]struct{}{"secret": {}}, map[string]struct{}{"secret": {}})
	if !s.Contains("secret") {
		t.Error("expected the skipped directory's own path to be skipped")
	}
	if !s.Contains("secret/a.txt") {
		t.Error("expected a descendant to be skipped")
	}
}

func TestSkipSet_ZeroValueContainsNothing(t *testing.T) {
	var s SkipSet
	if s.Contains("anything") {
		t.Error("zero-value SkipSet must never report a path as skipped")
	}
}

func TestSkipSet_NestedDirPrefix(t *testing.T) {
	s := NewSkipSet(nil, map[string]struct{}{"a/b": {}})
	if !s.Contains("a/b/c.txt") {
		t.Error("expected a/b/c.txt to be skipped under skipped dir a/b")
	}
	if s.Contains("a/bc.txt") {
		t.Error("a/b must not match a/bc.txt as a substring")
	}
	if s.Contains("a/x.txt") {
		t.Error("a alone is not skipped, only a/b")
	}
}
