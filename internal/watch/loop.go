package watch

import (
	"context"
	"sync"
	"time"

	"syncd/internal/app"
	"syncd/internal/fs"
	"syncd/internal/runtime"
)

// LoopConfig configures the always-on watch loop's three sync triggers.
type LoopConfig struct {
	RemotePoll time.Duration // full remote HashIfChanged cycle cadence (default 30s)
	DeepScan   time.Duration // full-tree HashIfChanged safety net (default 1h)
	Debounce   time.Duration // watcher batch window (default 500ms)
}

// Run blocks until ctx is cancelled. It starts an fsnotify Watcher on
// p.LocalRoot and drives three triggers, each running one reconcile cycle:
//
//   - a batch from the Watcher -> a SCOPED ReconcilePaths over just the
//     changed paths (Task 11) - fast, but never walks the tree, so it can't
//     see new empty directories or the children of a recursively-deleted
//     local directory (fsnotify only surfaces the top-level path for those).
//   - cfg.RemotePoll ticks -> a full RunRobustOnce (whole tree, HashIfChanged)
//     - this is what catches writes made directly on the remote by another
//     machine, since a network mount can't be watched by fsnotify.
//   - cfg.DeepScan ticks -> the same full RunRobustOnce, run as a periodic
//     safety net; it is the ONLY trigger that reconciles new empty
//     directories and recursively-deleted local directories (see above), so
//     it must not be disabled in production.
//
// All three triggers are serialized through a single mutex so cycles never
// overlap - concurrent scans/writes against the same manifest and files
// would corrupt state. Each cycle re-checks the remote canary
// (runtime.CheckRemoteCanary) before touching anything; if the canary check
// fails (e.g. the network mount vanished), the cycle is logged and skipped
// - Run itself never exits because of it, and the next tick/batch retries.
//
// Run returns nil on clean ctx cancellation (after closing the Watcher) or
// a non-nil error if the Watcher itself failed to start.
func Run(ctx context.Context, p app.RobustParams, matcher *fs.IgnoreMatcher, cfg LoopConfig) error {
	w, err := NewWatcher(p.LocalRoot, matcher, cfg.Debounce)
	if err != nil {
		return err
	}
	defer w.Close()

	pollTicker := time.NewTicker(cfg.RemotePoll)
	defer pollTicker.Stop()
	deepTicker := time.NewTicker(cfg.DeepScan)
	defer deepTicker.Stop()

	// batches is a local copy of w.Batches() that gets nil'd out once the
	// channel closes, so the select case blocks forever afterward instead of
	// spinning on a closed channel.
	batches := w.Batches()

	var mu sync.Mutex

	// guardedCycle serializes cycles through mu, re-checks the remote canary
	// first, and logs+skips fn on either a vanished canary or a cycle error
	// - it never returns an error to the caller, since a single bad cycle
	// must not stop the always-on loop.
	guardedCycle := func(fn func() error) {
		mu.Lock()
		defer mu.Unlock()
		if err := runtime.CheckRemoteCanary(p.RemoteRoot); err != nil {
			p.Logger.Error("watch: remote canary check failed, skipping cycle", map[string]any{"error": err.Error()})
			return
		}
		if err := fn(); err != nil {
			p.Logger.Error("watch: reconcile cycle failed", map[string]any{"error": err.Error()})
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case batch, ok := <-batches:
			if !ok {
				// Watcher shut itself down (fsnotify Events/Errors closed).
				// Keep serving ticks; nothing more will ever arrive on this
				// channel, so nil it out (a nil channel blocks forever in
				// select) rather than spin on the closed channel.
				batches = nil
				continue
			}
			guardedCycle(func() error {
				_, err := ReconcilePaths(ctx, p, batch)
				return err
			})

		case <-pollTicker.C:
			guardedCycle(func() error {
				pollParams := p
				pollParams.Policy = fs.HashIfChanged
				_, _, err := app.RunRobustOnce(ctx, pollParams)
				return err
			})

		case <-deepTicker.C:
			guardedCycle(func() error {
				deepParams := p
				deepParams.Policy = fs.HashIfChanged
				_, _, err := app.RunRobustOnce(ctx, deepParams)
				return err
			})
		}
	}
}
