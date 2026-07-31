package reconcile

import (
	"sort"

	"syncd/internal/db"
)

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
//
// skipped identifies paths this run's scan could not read (a symlink, an
// unreadable file, or anything beneath an unreadable subdirectory - see
// SkipSet). Such a path is excluded from every branch below, in both
// directions: a path we know nothing about must never drive a copy, a
// delete, or a move, since "not in left/right" is ambiguous between "really
// absent" and "couldn't be read this scan" (see the Critical 1/2 findings
// this parameter exists to close - a skipped counterpart used to look
// "deleted" and drive a real delete on the other side).
func FastCompare(mode FastMode, left, right, prev map[string]db.JobFileState, skipped SkipSet) FastResult {
	var tasks []FastTask
	newState := make(map[string]db.JobFileState)

	if mode == FastMirror {
		for relPath, lState := range left {
			if skipped.Contains(relPath) {
				continue
			}
			newState[relPath] = lState
			rState, rExists := right[relPath]
			if !rExists {
				tasks = append(tasks, FastTask{Type: CopyL2R, RelPath: relPath, Size: lState.Size})
			} else if lState.Size != rState.Size || lState.Mtime != rState.Mtime {
				tasks = append(tasks, FastTask{Type: CopyL2R, RelPath: relPath, Size: lState.Size})
			}
		}
		for relPath := range right {
			if skipped.Contains(relPath) {
				continue
			}
			if _, lExists := left[relPath]; !lExists {
				tasks = append(tasks, FastTask{Type: DeleteR, RelPath: relPath})
			}
		}
	} else if mode == FastUpdate {
		for relPath, lState := range left {
			if skipped.Contains(relPath) {
				continue
			}
			newState[relPath] = lState
			rState, rExists := right[relPath]
			if !rExists || lState.Size != rState.Size || lState.Mtime > rState.Mtime {
				tasks = append(tasks, FastTask{Type: CopyL2R, RelPath: relPath, Size: lState.Size})
			}
		}
		for relPath, rState := range right {
			if skipped.Contains(relPath) {
				continue
			}
			if _, exists := newState[relPath]; !exists {
				newState[relPath] = rState
			}
		}
	} else if mode == FastTwoWay {
		// Build signature -> path lookups for files that vanished from each
		// side since prev, so a "new" path elsewhere can be matched back to
		// where it came from (a rename/move). Signatures are {Size,Mtime}
		// only, so two vanished files can collide on the same signature; a
		// collision must not let either be picked arbitrarily (that risks
		// emitting a move to the wrong destination, which is data loss). If
		// a signature collides, drop it from the map entirely so the later
		// lookup simply misses and both paths safely fall through to the
		// slower-but-always-correct copy/delete path.
		leftMissingSigs := make(map[fastSignature]string)
		leftMissingCollided := make(map[fastSignature]bool)
		rightMissingSigs := make(map[fastSignature]string)
		rightMissingCollided := make(map[fastSignature]bool)

		// Iterate prev via a sorted slice, not a bare map. This loop has no
		// order-dependent side effect today - collision handling is symmetric
		// (see leftMissingCollided/rightMissingCollided below), so any visit
		// order converges on the same leftMissingSigs/rightMissingSigs - but
		// sorting costs nothing at these sizes and matches the determinism
		// this whole function is documented to guarantee (see the allPaths
		// sort a few lines below), rather than leaving that reasoning
		// unstated (docs/follow-ups.md #2).
		prevPaths := make([]string, 0, len(prev))
		for p := range prev {
			prevPaths = append(prevPaths, p)
		}
		sort.Strings(prevPaths)

		for _, path := range prevPaths {
			state := prev[path]
			if skipped.Contains(path) {
				// Ambiguous: we don't know this scan whether path is really
				// gone from left/right or just unreadable, so it must not
				// seed a move-detection candidate (that could pair it with
				// an unrelated new path and emit a wrong move) or a delete
				// (handled by the same check in pass 2 below).
				continue
			}
			sig := fastGetSig(state)
			if _, ok := left[path]; !ok {
				if leftMissingCollided[sig] {
					// already known to collide; nothing to do
				} else if _, exists := leftMissingSigs[sig]; exists {
					delete(leftMissingSigs, sig)
					leftMissingCollided[sig] = true
				} else {
					leftMissingSigs[sig] = path
				}
			}
			if _, ok := right[path]; !ok {
				if rightMissingCollided[sig] {
					// already known to collide; nothing to do
				} else if _, exists := rightMissingSigs[sig]; exists {
					delete(rightMissingSigs, sig)
					rightMissingCollided[sig] = true
				} else {
					rightMissingSigs[sig] = path
				}
			}
		}

		allPathsSet := make(map[string]bool, len(left)+len(right)+len(prev))
		for p := range left {
			allPathsSet[p] = true
		}
		for p := range right {
			allPathsSet[p] = true
		}
		for p := range prev {
			allPathsSet[p] = true
		}

		// Iterate a sorted slice, never a bare map, anywhere the classification
		// below has an order-dependent side effect. This is what makes
		// FastCompare's output deterministic: identical inputs always produce
		// an identical task list and NewState.
		allPaths := make([]string, 0, len(allPathsSet))
		for p := range allPathsSet {
			allPaths = append(allPaths, p)
		}
		sort.Strings(allPaths)

		// Pass 1: resolve every move before classifying anything else, so
		// `processedMoves` is fully populated before a delete for either side
		// of a move can be considered. This is the fix for the historical bug:
		// previously moves and deletes were decided in the same single pass
		// over a Go map, so whichever of {old path, new path} the (randomized)
		// map order visited first determined whether a spurious extra DeleteR
		// was also emitted alongside the correct MoveR/MoveL.
		processedMoves := make(map[string]bool)
		for _, p := range allPaths {
			_, prevOk := prev[p]
			if prevOk {
				continue // a move is only ever a genuinely new path (not in prev)
			}
			if skipped.Contains(p) {
				continue // unknown this scan; never a move source or destination
			}

			lState, lOk := left[p]
			rState, rOk := right[p]

			if lOk && !rOk {
				if oldPath, ok := leftMissingSigs[fastGetSig(lState)]; ok {
					// MoveR renames right's oldPath to p, so oldPath must
					// still actually be there to rename. If right has
					// independently deleted or renamed it elsewhere since
					// prev, oldPath is gone and the rename can only fail -
					// don't mark this as a move at all; let p fall through
					// to pass 2 and copy in as a new file instead (review
					// finding: a guaranteed-failing MoveR aborted the whole
					// RunFastTasks batch, including unrelated tasks).
					if _, oldExistsOnRight := right[oldPath]; oldExistsOnRight {
						tasks = append(tasks, FastTask{Type: MoveR, RelPath: p, OldRelPath: oldPath})
						newState[p] = lState
						delete(leftMissingSigs, fastGetSig(lState))
						processedMoves[oldPath] = true
						processedMoves[p] = true
					}
				}
			} else if rOk && !lOk {
				if oldPath, ok := rightMissingSigs[fastGetSig(rState)]; ok {
					// Symmetric guard for MoveL: oldPath must still exist on
					// left to rename from.
					if _, oldExistsOnLeft := left[oldPath]; oldExistsOnLeft {
						tasks = append(tasks, FastTask{Type: MoveL, RelPath: p, OldRelPath: oldPath})
						newState[p] = rState
						delete(rightMissingSigs, fastGetSig(rState))
						processedMoves[oldPath] = true
						processedMoves[p] = true
					}
				}
			}
		}

		// Pass 2: classify every remaining path (moves already fully handled
		// and skipped via processedMoves).
		for _, p := range allPaths {
			if processedMoves[p] {
				continue
			}
			if skipped.Contains(p) {
				// Unknown this scan: take no action, but preserve whatever
				// baseline prev already had (if any) so the next
				// successful scan can still compute a correct diff instead
				// of this path looking brand new.
				if prevState, ok := prev[p]; ok {
					newState[p] = prevState
				}
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
				} else if lState.Size == rState.Size {
					// Equal mtime AND equal size: the fast reconciler never
					// reads content, so this is the closest it can get to
					// "actually identical" - safe to baseline.
					newState[p] = lState
				}
				// Equal mtime, different size: both sides genuinely changed
				// and there is no size+mtime-only signal to break the tie -
				// this reconciler never hashes content (that's --hash's
				// job). Deliberately queue no task AND do not assign
				// newState[p]: fabricating either side as the new baseline
				// would let a later run diff the OTHER (truly unchanged
				// since now) side against that fabricated state, read it as
				// "changed", and silently overwrite the real edit it's
				// missing from (review finding "Medium 8"). Leaving p out of
				// newState means next run's prev has no entry for it either,
				// so this same ambiguous conflict re-surfaces (still as no
				// task) every run until the user resolves it manually,
				// rather than ever being silently resolved wrong.
			} else if lChanged {
				tasks = append(tasks, FastTask{Type: CopyL2R, RelPath: p, Size: lState.Size})
				newState[p] = lState
			} else if rChanged {
				tasks = append(tasks, FastTask{Type: CopyR2L, RelPath: p, Size: rState.Size})
				newState[p] = rState
			} else if lDeleted && !rDeleted {
				tasks = append(tasks, FastTask{Type: DeleteR, RelPath: p})
			} else if rDeleted && !lDeleted {
				tasks = append(tasks, FastTask{Type: DeleteL, RelPath: p})
			} else if !lDeleted && !rDeleted {
				newState[p] = lState
			}
		}
	}

	return FastResult{Tasks: tasks, NewState: newState}
}
