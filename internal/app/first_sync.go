package app

import (
	"fmt"

	"syncd/internal/db"
	"syncd/internal/fs"
	"syncd/internal/reconcile"
)

// FirstSyncPolicy decides what a pair's very first robust cycle - one with
// no manifest at all - does with a file that exists on both sides with
// different content. With no history there is no telling which side
// changed, and the engine's general answer (a conflict: the remote version
// takes the name, the local one is kept beside it as a "_conflict_" copy)
// is a poor fit for a first run over two long-lived trees: it replaces
// every such local file with the remote's version and scatters conflict
// copies through the tree.
type FirstSyncPolicy string

const (
	// FirstSyncKeepBoth (also the zero value) is the conflict behavior
	// described above.
	FirstSyncKeepBoth FirstSyncPolicy = "keep-both"
	// FirstSyncNewer keeps whichever version has the later mtime under the
	// file's name on both sides and saves the other to the trash. Files
	// with equal mtimes, and every file when there is no trash, fall back
	// to keep-both.
	FirstSyncNewer FirstSyncPolicy = "newer"
	// FirstSyncLocal / FirstSyncRemote make that side's version win; the
	// other is saved to the trash when there is one, else overwritten.
	FirstSyncLocal  FirstSyncPolicy = "local"
	FirstSyncRemote FirstSyncPolicy = "remote"
)

// ParseFirstSyncPolicy parses a --first-sync value.
func ParseFirstSyncPolicy(s string) (FirstSyncPolicy, error) {
	switch p := FirstSyncPolicy(s); p {
	case FirstSyncKeepBoth, FirstSyncNewer, FirstSyncLocal, FirstSyncRemote:
		return p, nil
	}
	return "", fmt.Errorf("invalid first-sync policy %q (want newer, local, remote, or keep-both)", s)
}

// applyFirstSync rewrites plan's baseline-less conflicts according to
// p.FirstSync, in place, and returns how many it resolved. It only acts on
// a true first sync: firstSync reports whether the manifest is empty, and
// is consulted only if there is something to rewrite. On a pair that
// already has history, a new file created on both sides with different
// content is a genuine conflict and keeps both.
func (p RobustParams) applyFirstSync(plan *reconcile.Plan, local, remote map[string]fs.FileEntry, manifest map[string]db.ManifestRow, firstSync func() (bool, error)) (int, error) {
	if p.FirstSync == "" || p.FirstSync == FirstSyncKeepBoth {
		return 0, nil
	}
	if p.FirstSync == FirstSyncNewer && p.Trash == nil {
		return 0, nil // nowhere safe to put the older version
	}
	checked := false
	resolved := 0
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if a.Type != reconcile.ActionConflict {
			continue
		}
		if _, tracked := manifest[a.Path]; tracked {
			continue
		}
		if !checked {
			first, err := firstSync()
			if err != nil {
				return 0, err
			}
			if !first {
				return 0, nil
			}
			checked = true
		}
		l, r := local[a.Path], remote[a.Path]
		var localWins bool
		switch p.FirstSync {
		case FirstSyncLocal:
			localWins = true
		case FirstSyncRemote:
			localWins = false
		default: // newer
			if l.Mtime == r.Mtime {
				continue // no basis to choose: keep both
			}
			localWins = l.Mtime > r.Mtime
		}
		if localWins {
			a.Type = reconcile.ActionUpload
			a.Reason = fmt.Sprintf("first sync (%s): local version kept; remote version saved to trash", p.FirstSync)
		} else {
			a.Type = reconcile.ActionDownload
			a.Reason = fmt.Sprintf("first sync (%s): remote version kept; local version saved to trash", p.FirstSync)
		}
		a.PreserveDest = true
		resolved++
	}
	if resolved > 0 {
		p.Logger.Printf("first sync: %d file(s) differed between local and remote; resolved with --first-sync %s, the replaced versions are in the trash", resolved, p.FirstSync)
	}
	return resolved, nil
}
