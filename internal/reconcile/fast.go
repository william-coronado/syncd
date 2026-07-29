package reconcile

import "syncd/internal/db"

// FastMode selects the reconciliation strategy for the fast (size+mtime)
// reconciler used by the ad-hoc `sync` command.
type FastMode string

const (
	FastMirror FastMode = "mirror"
	FastUpdate FastMode = "update"
	FastTwoWay FastMode = "two-way"
)

// FastTaskType enumerates the actions the fast reconciler can request.
type FastTaskType string

const (
	CopyL2R FastTaskType = "CopyL2R"
	CopyR2L FastTaskType = "CopyR2L"
	DeleteL FastTaskType = "DeleteL"
	DeleteR FastTaskType = "DeleteR"
	MoveL   FastTaskType = "MoveL"
	MoveR   FastTaskType = "MoveR"
)

// FastTask is a single unit of work produced by FastCompare.
type FastTask struct {
	Type       FastTaskType
	RelPath    string
	OldRelPath string
	Size       int64
}

// FastResult is the output of FastCompare: the tasks required to reconcile
// left and right, plus the state to persist for the next two-way run.
type FastResult struct {
	Tasks    []FastTask
	NewState map[string]db.JobFileState
}

type fastSignature struct {
	Size  int64
	Mtime int64
}

func fastGetSig(s db.JobFileState) fastSignature {
	return fastSignature{Size: s.Size, Mtime: s.Mtime}
}

// FastCompare analyzes the state of both folders and returns a list of
// required sync tasks. This is a direct port of file_sync_util's
// internal/sync.Compare, with FileState{Size,ModTime} swapped for
// db.JobFileState{Size,Mtime} (mtime as unix seconds). prev is the previous
// two-way state and is only consulted in FastTwoWay mode.
func FastCompare(mode FastMode, left, right, prev map[string]db.JobFileState) FastResult {
	var tasks []FastTask
	newState := make(map[string]db.JobFileState)

	if mode == FastMirror {
		for relPath, lState := range left {
			newState[relPath] = lState
			rState, rExists := right[relPath]
			if !rExists {
				tasks = append(tasks, FastTask{Type: CopyL2R, RelPath: relPath, Size: lState.Size})
			} else if lState.Size != rState.Size || lState.Mtime != rState.Mtime {
				tasks = append(tasks, FastTask{Type: CopyL2R, RelPath: relPath, Size: lState.Size})
			}
		}
		for relPath := range right {
			if _, lExists := left[relPath]; !lExists {
				tasks = append(tasks, FastTask{Type: DeleteR, RelPath: relPath})
			}
		}
	} else if mode == FastUpdate {
		for relPath, lState := range left {
			newState[relPath] = lState
			rState, rExists := right[relPath]
			if !rExists || lState.Size != rState.Size || lState.Mtime > rState.Mtime {
				tasks = append(tasks, FastTask{Type: CopyL2R, RelPath: relPath, Size: lState.Size})
			}
		}
		for relPath, rState := range right {
			if _, exists := newState[relPath]; !exists {
				newState[relPath] = rState
			}
		}
	} else if mode == FastTwoWay {
		leftMissingSigs := make(map[fastSignature]string)
		rightMissingSigs := make(map[fastSignature]string)

		for path, state := range prev {
			if _, ok := left[path]; !ok {
				leftMissingSigs[fastGetSig(state)] = path
			}
			if _, ok := right[path]; !ok {
				rightMissingSigs[fastGetSig(state)] = path
			}
		}

		allPaths := make(map[string]bool)
		for p := range left {
			allPaths[p] = true
		}
		for p := range right {
			allPaths[p] = true
		}
		for p := range prev {
			allPaths[p] = true
		}

		processedMoves := make(map[string]bool)

		for p := range allPaths {
			if processedMoves[p] {
				continue
			}

			lState, lOk := left[p]
			rState, rOk := right[p]
			prevState, prevOk := prev[p]

			lChanged := lOk && (!prevOk || fastGetSig(lState) != fastGetSig(prevState))
			rChanged := rOk && (!prevOk || fastGetSig(rState) != fastGetSig(prevState))
			lDeleted := !lOk && prevOk
			rDeleted := !rOk && prevOk

			if lChanged && rChanged {
				// Conflict: newer wins
				if lState.Mtime > rState.Mtime {
					tasks = append(tasks, FastTask{Type: CopyL2R, RelPath: p, Size: lState.Size})
					newState[p] = lState
				} else if rState.Mtime > lState.Mtime {
					tasks = append(tasks, FastTask{Type: CopyR2L, RelPath: p, Size: rState.Size})
					newState[p] = rState
				} else {
					newState[p] = lState // identical
				}
			} else if lChanged {
				// Check for move in Left
				isMove := false
				if !prevOk {
					if oldPath, ok := leftMissingSigs[fastGetSig(lState)]; ok {
						tasks = append(tasks, FastTask{Type: MoveR, RelPath: p, OldRelPath: oldPath})
						newState[p] = lState
						delete(leftMissingSigs, fastGetSig(lState))
						processedMoves[oldPath] = true // prevent OldRelPath from triggering deletion logic
						isMove = true
					}
				}
				if !isMove {
					tasks = append(tasks, FastTask{Type: CopyL2R, RelPath: p, Size: lState.Size})
					newState[p] = lState
				}
			} else if rChanged {
				// Check for move in Right
				isMove := false
				if !prevOk {
					if oldPath, ok := rightMissingSigs[fastGetSig(rState)]; ok {
						tasks = append(tasks, FastTask{Type: MoveL, RelPath: p, OldRelPath: oldPath})
						newState[p] = rState
						delete(rightMissingSigs, fastGetSig(rState))
						processedMoves[oldPath] = true // prevent OldRelPath from triggering deletion logic
						isMove = true
					}
				}
				if !isMove {
					tasks = append(tasks, FastTask{Type: CopyR2L, RelPath: p, Size: rState.Size})
					newState[p] = rState
				}
			} else if lDeleted && !rDeleted && !processedMoves[p] {
				tasks = append(tasks, FastTask{Type: DeleteR, RelPath: p})
			} else if rDeleted && !lDeleted && !processedMoves[p] {
				tasks = append(tasks, FastTask{Type: DeleteL, RelPath: p})
			} else if !lDeleted && !rDeleted {
				newState[p] = lState
			}
		}
	}

	return FastResult{Tasks: tasks, NewState: newState}
}
