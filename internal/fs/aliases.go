package fs

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"syncd/internal/db"
)

// ErrCaseAlias marks a refusal caused by two spellings of one path that
// differ only by case. Callers with a narrower view (the scoped cycle) treat
// it as a reason to run the authoritative full-tree check instead.
var ErrCaseAlias = errors.New("case-insensitive path collision")

// CheckCaseAliases refuses ambiguous names before reconciliation. Sync pairs
// must be portable to case-insensitive destinations (notably APFS and SMB).
// Directory components are included: a copy and delete of two spellings of
// the same file must never be scheduled together.
//
// Only names that exist on a side are compared. A baseline spelling that is
// gone from both sides is a stale row the planner simply drops, so it must
// not block the pair forever (renaming both sides the same way resolves a
// collision). The baseline is otherwise used for one exemption: a case-only
// rename of a file on one side (see isCaseOnlyRename), which the planner
// turns into a delete of the old spelling and a create of the new one.
// scans are (local, remote) when two are given.
func CheckCaseAliases(manifest map[string]db.ManifestRow, scans ...ScanResult) error {
	fold := cases.Fold()
	groups := make(map[string][]map[string]struct{})
	add := func(side int, p string) {
		for ; p != "." && p != "/"; p = path.Dir(p) {
			key := fold.String(p)
			if groups[key] == nil {
				groups[key] = make([]map[string]struct{}, len(scans))
			}
			if groups[key][side] == nil {
				groups[key][side] = make(map[string]struct{})
			}
			groups[key][side][p] = struct{}{}
		}
	}
	for i, scan := range scans {
		for _, set := range []map[string]struct{}{keys(scan.Files), scan.Dirs, scan.Skipped, scan.SkippedDirs} {
			for p := range set {
				add(i, p)
			}
		}
	}
	keysSorted := make([]string, 0, len(groups))
	for key := range groups {
		keysSorted = append(keysSorted, key)
	}
	sort.Strings(keysSorted)
	for _, key := range keysSorted {
		sides := groups[key]
		present := make(map[string]struct{})
		for _, side := range sides {
			for p := range side {
				present[p] = struct{}{}
			}
		}
		if len(present) <= 1 || isCaseOnlyRename(manifest, scans, sides) {
			continue
		}
		names := make([]string, 0, len(present))
		for p := range present {
			names = append(names, p)
		}
		sort.Strings(names)
		return fmt.Errorf("%w: %q and %q; give these paths distinct names, or the same spelling on both sides, before syncing", ErrCaseAlias, names[0], names[1])
	}
	return nil
}

func keys(files map[string]FileEntry) map[string]struct{} {
	out := make(map[string]struct{}, len(files))
	for p := range files {
		out[p] = struct{}{}
	}
	return out
}

// isCaseOnlyRename recognizes a file renamed by case on one side only: each
// side holds exactly one spelling, they differ only in the last component,
// the old spelling is the tracked one, and its holder still has exactly the
// tracked content. The planner then deletes the unchanged old spelling and
// creates the new one; the executor's exclusive publish and observed-entry
// checks keep those two actions from touching one file through two names,
// so on a case-insensitive side the create simply waits a cycle.
//
// Content is compared by hash, so the fast (size+mtime) path, whose
// baseline carries no hashes, never qualifies: its executor overwrites and
// deletes without those checks.
func isCaseOnlyRename(manifest map[string]db.ManifestRow, scans []ScanResult, sides []map[string]struct{}) bool {
	if len(scans) != 2 || len(sides[0]) != 1 || len(sides[1]) != 1 {
		return false
	}
	var spelled [2]string
	for i, side := range sides {
		for p := range side {
			spelled[i] = p
		}
	}
	if spelled[0] == spelled[1] || path.Dir(spelled[0]) != path.Dir(spelled[1]) {
		return false
	}
	_, tracked0 := manifest[spelled[0]]
	_, tracked1 := manifest[spelled[1]]
	if tracked0 == tracked1 {
		return false
	}
	oldSide := 0
	if tracked1 {
		oldSide = 1
	}
	old, renamed := spelled[oldSide], spelled[1-oldSide]
	row := manifest[old]
	entry, ok := scans[oldSide].Files[old]
	if !ok || row.Hash == "" || entry.Hash != row.Hash || entry.Size != row.Size {
		return false
	}
	_, isFile := scans[1-oldSide].Files[renamed]
	return isFile
}

// CheckScopedCaseAliases inspects names in affected parent directories
// without scanning or hashing their unrelated files. It cannot tell a
// case-only rename from a genuine collision; RunRobustScoped answers an
// ErrCaseAlias from it with a full cycle, whose CheckCaseAliases can.
//
// The baseline is deliberately not consulted: listing each affected parent
// on both sides already sees every spelling that exists there, and a
// baseline spelling that exists on neither side is a stale row the planner
// only drops (see CheckCaseAliases). Loading the whole manifest here made
// every fsnotify batch cost as much as the tree.
func CheckScopedCaseAliases(local, remote *Tree, paths map[string]struct{}, scopes ...ScanResult) error {
	fold := cases.Fold()
	targets := map[string]map[string]string{}
	add := func(rel string) {
		for ; rel != "." && rel != "/"; rel = path.Dir(rel) {
			parent, name := path.Dir(rel), path.Base(rel)
			if targets[parent] == nil {
				targets[parent] = map[string]string{}
			}
			targets[parent][fold.String(name)] = name
		}
	}
	for rel := range paths {
		add(rel)
	}
	for _, scope := range scopes {
		for rel := range scope.Files {
			add(rel)
		}
		for rel := range scope.Dirs {
			add(rel)
		}
	}
	check := func(parent, actual string) error {
		actual = norm.NFC.String(actual)
		if wanted, ok := targets[parent][fold.String(actual)]; ok && wanted != actual {
			return fmt.Errorf("%w: %q and %q", ErrCaseAlias, path.Join(parent, wanted), path.Join(parent, actual))
		}
		return nil
	}
	for _, tree := range []*Tree{local, remote} {
		for parent := range targets {
			dir, err := tree.directory(filepath.FromSlash(parent), 0)
			if os.IsNotExist(err) || errors.Is(err, ErrSymlink) {
				continue
			}
			if err != nil {
				return err
			}
			names, err := dir.Readdirnames(-1)
			dir.Close()
			if err != nil {
				return err
			}
			for _, actual := range names {
				// Another syncd's in-flight (or retained) entry: reported
				// by the scanner, never reconciled.
				if strings.HasPrefix(actual, holdingPrefix) {
					continue
				}
				if err := check(parent, actual); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
