package execute

import (
	"context"
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
	AddBytes(n int64)
	Done()
}

// RunFastTasks executes fast tasks against the two roots using atomic writes.
// progress may be nil (non-interactive). Returns the first error encountered.
func RunFastTasks(ctx context.Context, workers int, left, right string, tasks []reconcile.FastTask, progress ProgressSink) error {
	jobs := make(chan reconcile.FastTask)
	errs := make(chan error, len(tasks))

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
				errs <- applyFastTask(left, right, task, progress)
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

func applyFastTask(left, right string, task reconcile.FastTask, progress ProgressSink) error {
	switch task.Type {
	case reconcile.CopyL2R:
		src := filepath.Join(left, filepath.FromSlash(task.RelPath))
		dst := filepath.Join(right, filepath.FromSlash(task.RelPath))
		if err := fs.AtomicCopyFile(src, dst); err != nil {
			return err
		}
		if progress != nil {
			progress.AddBytes(task.Size)
		}
		return nil
	case reconcile.CopyR2L:
		src := filepath.Join(right, filepath.FromSlash(task.RelPath))
		dst := filepath.Join(left, filepath.FromSlash(task.RelPath))
		if err := fs.AtomicCopyFile(src, dst); err != nil {
			return err
		}
		if progress != nil {
			progress.AddBytes(task.Size)
		}
		return nil
	case reconcile.DeleteL:
		return os.RemoveAll(filepath.Join(left, filepath.FromSlash(task.RelPath)))
	case reconcile.DeleteR:
		return os.RemoveAll(filepath.Join(right, filepath.FromSlash(task.RelPath)))
	case reconcile.MoveL:
		return moveFastFile(left, task)
	case reconcile.MoveR:
		return moveFastFile(right, task)
	default:
		return nil
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

	if err := os.Rename(src, dst); err != nil {
		if err := fs.AtomicCopyFile(src, dst); err != nil {
			return err
		}
		return os.RemoveAll(src)
	}
	return nil
}
