package reconcile

import (
	"sort"

	"drivesync/internal/db"
	"drivesync/internal/fs"
)

type ActionType string

const (
	ActionNoOp         ActionType = "NO_OP"
	ActionUpload       ActionType = "UPLOAD"
	ActionDownload     ActionType = "DOWNLOAD"
	ActionDeleteRemote ActionType = "DELETE_REMOTE"
	ActionDeleteLocal  ActionType = "DELETE_LOCAL"
	ActionConflict     ActionType = "CONFLICT"
)

type Action struct {
	Type   ActionType `json:"type"`
	Path   string     `json:"path"`
	Reason string     `json:"reason"`
}

type Plan struct {
	Actions []Action `json:"actions"`
}

func BuildPlan(
	local map[string]fs.FileEntry,
	remote map[string]fs.FileEntry,
	manifest map[string]db.ManifestRow,
	skipped map[string]struct{},
) Plan {
	keys := allKeys(local, remote, manifest)
	actions := make([]Action, 0, len(keys))

	for _, path := range keys {
		if _, skip := skipped[path]; skip {
			continue
		}
		manifestRow, inManifest := manifest[path]
		l, inLocal := local[path]
		r, inRemote := remote[path]

		if inLocal && inRemote {
			if l.Hash == r.Hash {
				actions = append(actions, Action{Type: ActionNoOp, Path: path, Reason: "hashes match"})
				continue
			}
			if !inManifest {
				actions = append(actions, Action{Type: ActionConflict, Path: path, Reason: "first-sync path collision with differing content"})
				continue
			}
			localChanged := l.Hash != manifestRow.Hash
			remoteChanged := r.Hash != manifestRow.Hash
			switch {
			case localChanged && remoteChanged:
				actions = append(actions, Action{Type: ActionConflict, Path: path, Reason: "modified on both sides since last sync"})
			case localChanged:
				actions = append(actions, Action{Type: ActionUpload, Path: path, Reason: "local changed; remote unchanged"})
			case remoteChanged:
				actions = append(actions, Action{Type: ActionDownload, Path: path, Reason: "remote changed; local unchanged"})
			default:
				actions = append(actions, Action{Type: ActionConflict, Path: path, Reason: "diverged from manifest without side attribution"})
			}
			continue
		}

		if inLocal && !inRemote {
			if !inManifest {
				actions = append(actions, Action{Type: ActionUpload, Path: path, Reason: "new local file"})
				continue
			}
			localChanged := l.Hash != manifestRow.Hash
			if localChanged {
				actions = append(actions, Action{Type: ActionUpload, Path: path, Reason: "local changed while remote deleted; preserving changed file"})
			} else {
				actions = append(actions, Action{Type: ActionDeleteLocal, Path: path, Reason: "remote deleted; propagating delete"})
			}
			continue
		}

		if !inLocal && inRemote {
			if !inManifest {
				actions = append(actions, Action{Type: ActionDownload, Path: path, Reason: "new remote file"})
				continue
			}
			remoteChanged := r.Hash != manifestRow.Hash
			if remoteChanged {
				actions = append(actions, Action{Type: ActionDownload, Path: path, Reason: "remote changed while local deleted; preserving changed file"})
			} else {
				actions = append(actions, Action{Type: ActionDeleteRemote, Path: path, Reason: "local deleted; propagating delete"})
			}
			continue
		}

		if inManifest {
			actions = append(actions, Action{Type: ActionNoOp, Path: path, Reason: "already deleted on both sides"})
		}
	}

	sort.SliceStable(actions, func(i, j int) bool {
		if actions[i].Type == actions[j].Type {
			return actions[i].Path < actions[j].Path
		}
		return actions[i].Type < actions[j].Type
	})

	return Plan{Actions: actions}
}

func allKeys(
	local map[string]fs.FileEntry,
	remote map[string]fs.FileEntry,
	manifest map[string]db.ManifestRow,
) []string {
	set := make(map[string]struct{}, len(local)+len(remote)+len(manifest))
	for k := range local {
		set[k] = struct{}{}
	}
	for k := range remote {
		set[k] = struct{}{}
	}
	for k := range manifest {
		set[k] = struct{}{}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
