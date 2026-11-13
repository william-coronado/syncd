package execute

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"syncd/internal/fs"
	"syncd/internal/reconcile"
)

// ProgressSink receives progress updates from RunFastTasks. It is satisfied
// by a pterm-backed sink in the cli layer and by a nop in tests. progress
// may be nil, in which case no progress is reported.
type ProgressSink interface {
	// Start is called once before any task runs, with the total number of
	// tasks about to be processed. This gives the progress bar a real
	// denominator instead of a placeholder.
	Start(totalTasks int)
	// Advance reports that one task completed, transferring bytes bytes
	// (0 for deletes and moves). It is called once per task, so the bar
	// ticks per file rather than by raw byte count.
	Advance(bytes int64)
	Done()
}

// RunFastTasks executes fast tasks against the two roots using atomic writes.
// progress may be nil (non-interactive). Returns the first error encountered.
func RunFastTasks(ctx context.Context, workers int, left, right string, tasks []reconcile.FastTask, progress ProgressSink) error {
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan reconcile.FastTask)
	errs := make(chan error, len(tasks))

	if progress != nil {
		progress.Start(len(tasks))
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range jobs {
				if err := ctx.Err(); err != nil {
					errs <- err
					continue
				}
				err := applyFastTask(left, right, task)
				if err == nil && progress != nil {
					progress.Advance(task.Size)
				}
				errs <- err
			}
		}()
	}

	// Feeding and draining run as independent goroutines so that an early
	// return here (on cancellation) still always closes jobs, and workers
	// finishing still always closes errs - neither side can leave the other
	// blocked forever.
	go func() {
		defer close(jobs)
		for _, task := range tasks {
			select {
			case <-ctx.Done():
				return
			case jobs <- task:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(errs)
	}()

	var firstErr error
	for err := range errs {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if progress != nil {
		progress.Done()
	}

	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func applyFastTask(left, right string, task reconcile.FastTask) error {
	switch task.Type {
	case reconcile.CopyL2R:
		src := filepath.Join(left, filepath.FromSlash(task.RelPath))
		dst := filepath.Join(right, filepath.FromSlash(task.RelPath))
		return fs.AtomicCopyFile(src, dst)
	case reconcile.CopyR2L:
		src := filepath.Join(right, filepath.FromSlash(task.RelPath))
		dst := filepath.Join(left, filepath.FromSlash(task.RelPath))
		return fs.AtomicCopyFile(src, dst)
	case reconcile.ConflictL2R:
		return copyPreservingDestination(left, right, task.RelPath)
	case reconcile.ConflictR2L:
		return copyPreservingDestination(right, left, task.RelPath)
	case reconcile.DeleteL:
		return removeIfExists(filepath.Join(left, filepath.FromSlash(task.RelPath)))
	case reconcile.DeleteR:
		return removeIfExists(filepath.Join(right, filepath.FromSlash(task.RelPath)))
	case reconcile.MoveL:
		return moveFastFile(left, task)
	case reconcile.MoveR:
		return moveFastFile(right, task)
	default:
		return fmt.Errorf("unknown action type %s", task.Type)
	}
}

// moveFastFile moves a file within root from task.OldRelPath to
// task.RelPath. It tries os.Rename first, falling back to an atomic
// copy+remove when rename fails (e.g. across devices).
func moveFastFile(root string, task reconcile.FastTask) error {
	src := filepath.Join(root, filepath.FromSlash(task.OldRelPath))
	dst := filepath.Join(root, filepath.FromSlash(task.RelPath))

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	if err := renameFunc(src, dst); err != nil {
		if err := copyFunc(src, dst); err != nil {
			return err
		}
		// os.Remove, not os.RemoveAll: FastCompare only queues a move for
		// paths it saw as FILES during scanning. If src was replaced by a
		// non-empty directory between scan and now (a TOCTOU race), os.Remove
		// fails safely with ENOTEMPTY rather than silently wiping the whole
		// subtree - the same safety convention used by DeleteL/DeleteR tasks
		// (see TestApplyFastTask_DeleteFailsSafelyWhenPathIsActuallyANonEmptyDirectory).
		return os.Remove(src)
	}
	return nil
}

// renameFunc is os.Rename, indirected through a package-level variable so
// tests can force the copy+remove fallback path in moveFastFile
// deterministically (simulating a cross-device rename failure) without
// needing an actual cross-device filesystem setup. Same pattern as
// conflict.go's timeNow and atomic_copy.go's tempSiblingPathFunc.
var renameFunc = os.Rename

// copyFunc is fs.AtomicCopyFile, indirected through a package-level variable
// so tests can simulate the copy succeeding and then src being replaced by a
// non-empty directory before the subsequent os.Remove(src) call - the TOCTOU
// race that call guards against - without needing real concurrent
// filesystem mutation.
var copyFunc = fs.AtomicCopyFile

// copyPreservingDestination copies srcRoot/rel over dstRoot/rel, first
// renaming any existing destination aside to a conflict copy (the same
// "_conflict_<ts>" naming the robust engine uses) so the losing side of a
// two-way conflict is kept rather than overwritten. If the copy fails, the
// destination is put back.
func copyPreservingDestination(srcRoot, dstRoot, rel string) error {
	src := filepath.Join(srcRoot, filepath.FromSlash(rel))
	dst := filepath.Join(dstRoot, filepath.FromSlash(rel))
	if _, err := os.Lstat(dst); err != nil {
		if os.IsNotExist(err) {
			return copyFunc(src, dst) // nothing to preserve
		}
		return fmt.Errorf("stat conflict destination %s: %w", rel, err)
	}
	conflictRel, err := findAvailableConflictPath(srcRoot, dstRoot, rel, timeNow().UTC())
	if err != nil {
		return err
	}
	preserved := filepath.Join(dstRoot, filepath.FromSlash(conflictRel))
	if err := renameFunc(dst, preserved); err != nil {
		return fmt.Errorf("preserve conflicting %s: %w", rel, err)
	}
	if err := copyFunc(src, dst); err != nil {
		if rerr := renameFunc(preserved, dst); rerr != nil {
			return fmt.Errorf("copy %s after preserving conflict copy: %w (restore also failed, original kept at %s: %v)", rel, err, conflictRel, rerr)
		}
		return fmt.Errorf("copy %s after preserving conflict copy: %w", rel, err)
	}
	return nil
}
