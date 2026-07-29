package watch

import (
	"context"

	"syncd/internal/app"
	"syncd/internal/fs"
)

// ReconcilePaths runs one scoped reconcile cycle for a batch of changed
// local paths (as coalesced by Watcher): it stat/hashes ONLY those paths on
// both sides via app.RunRobustScoped, with no full-tree walk, so a local
// edit reconciles in time proportional to the size of the batch rather than
// the size of the tree.
func ReconcilePaths(ctx context.Context, p app.RobustParams, paths map[string]struct{}) (int, error) {
	p.Policy = fs.HashIfChanged
	_, failures, err := app.RunRobustScoped(ctx, p, paths)
	return failures, err
}
