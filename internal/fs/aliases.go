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

// CheckCaseAliases refuses ambiguous names before reconciliation. Sync pairs
// must be portable to case-insensitive destinations (notably APFS and SMB).
// Include directory components and baseline paths: a copy and delete of two
// spellings of the same file must never be scheduled together.
func CheckCaseAliases(manifest map[string]db.ManifestRow, scans ...ScanResult) error {
	paths := make(map[string]struct{})
	add := func(p string) {
		for ; p != "." && p != "/"; p = path.Dir(p) {
			paths[p] = struct{}{}
		}
	}
	for p := range manifest {
		add(p)
	}
	for _, scan := range scans {
		for p := range scan.Files {
			add(p)
		}
		for p := range scan.Dirs {
			add(p)
		}
		for p := range scan.Skipped {
			add(p)
		}
		for p := range scan.SkippedDirs {
			add(p)
		}
	}
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	fold := cases.Fold()
	seen := make(map[string]string)
	for _, p := range names {
		key := fold.String(p)
		if prev, ok := seen[key]; ok && prev != p {
			return fmt.Errorf("case-insensitive path collision: %q and %q; give these paths distinct names before syncing", prev, p)
		}
		seen[key] = p
	}
	return nil
}

// CheckScopedCaseAliases inspects names in affected parent directories
// without scanning or hashing their unrelated files.
func CheckScopedCaseAliases(local, remote *Tree, paths, baseline, dirs map[string]struct{}, scopes ...ScanResult) error {
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
			return fmt.Errorf("case-insensitive path collision: %q and %q", path.Join(parent, wanted), path.Join(parent, actual))
		}
		return nil
	}
	for _, names := range []map[string]struct{}{baseline, dirs} {
		for rel := range names {
			for ; rel != "." && rel != "/"; rel = path.Dir(rel) {
				if err := check(path.Dir(rel), path.Base(rel)); err != nil {
					return err
				}
			}
		}
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
				if strings.HasPrefix(actual, holdingPrefix) {
					return fmt.Errorf("unfinished sync operation at %s; inspect the retained file before retrying", filepath.Join(tree.Name(), parent, actual))
				}
				if err := check(parent, actual); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
