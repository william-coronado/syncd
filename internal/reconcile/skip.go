package reconcile

import "strings"

// SkipSet answers whether a root-relative, forward-slash-separated path was
// skipped during scanning - either the path itself (Exact: a symlink, an
// unreadable file, an SMB-forbidden name, ...) or because it falls beneath a
// directory that itself could not be enumerated (Dirs: e.g. a
// permission-denied subdirectory). A directory whose contents couldn't be
// listed means every path beneath it is unknown, not "absent" - fs.ScanTree
// only ever records the directory's OWN relpath (it never visited what's
// inside), so without subtree matching a reconciler would read every
// descendant as "deleted" and act on that (see the Critical 2 finding this
// type exists to close).
//
// Both reconcilers (FastCompare and BuildPlan) funnel every skip check
// through Contains, so there is exactly one place subtree-prefix matching is
// implemented - they cannot silently drift apart on what "skipped" means.
//
// The zero value is a valid, always-empty SkipSet (Contains always reports
// false), so callers that don't care about skips can pass SkipSet{}.
type SkipSet struct {
	exact map[string]struct{}
	dirs  map[string]struct{}
}

// NewSkipSet builds a SkipSet from a set of exactly-skipped paths and a set
// of skipped directories (each is typically the union of a scan pair's
// local+remote results - see app.mergeSkipSet). Either may be nil.
func NewSkipSet(exact, dirs map[string]struct{}) SkipSet {
	return SkipSet{exact: exact, dirs: dirs}
}

// Contains reports whether path was skipped: either an exact match, or a
// descendant of a skipped directory. Directory matching is by path segment -
// "secret" matches "secret/x.txt" but never "secretive/x.txt" - since dirs
// holds root-relative directory paths, never arbitrary string prefixes.
func (s SkipSet) Contains(path string) bool {
	if _, ok := s.exact[path]; ok {
		return true
	}
	if len(s.dirs) == 0 {
		return false
	}
	for _, prefix := range dirPrefixes(path) {
		if _, ok := s.dirs[prefix]; ok {
			return true
		}
	}
	return false
}

// dirPrefixes returns every proper ancestor directory of path, segment by
// segment: "a/b/c" -> ["a", "a/b"]. path itself is never included (a file
// cannot be its own parent directory), and matching is purely on whole
// segments, never a plain string prefix.
func dirPrefixes(path string) []string {
	segs := strings.Split(path, "/")
	if len(segs) <= 1 {
		return nil
	}
	prefixes := make([]string, 0, len(segs)-1)
	prefix := segs[0]
	prefixes = append(prefixes, prefix)
	for i := 1; i < len(segs)-1; i++ {
		prefix = prefix + "/" + segs[i]
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}
