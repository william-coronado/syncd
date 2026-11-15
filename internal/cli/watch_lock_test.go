package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"syncd/internal/app"
	"syncd/internal/fs"
	"syncd/internal/runtime"
)

func TestSyncAndWatchShareLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	left, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	right, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.EnsureRemoteCanary(right); err != nil {
		t.Fatal(err)
	}
	cfg := app.WatchConfig{LocalRoot: left, RemoteRoot: right, Silent: true, NoTrash: true}
	err = app.RunWatch(context.Background(), cfg, func(ctx context.Context, p app.RobustParams, m *fs.IgnoreMatcher, cfg app.WatchConfig) error {
		err := runSyncJob(ctx, SyncJob{Name: "review", Left: left, Right: right, Mode: "mirror"}, syncRunOptions{Assume: true, DryRun: true, JobsDBPath: filepath.Join(t.TempDir(), "jobs.db")})
		if err == nil || !strings.Contains(err.Error(), "lock already held") {
			return fmt.Errorf("sync acquired a second lock while watch still holds its lock for identical canonical roots")
		}
		t.Logf("sync refused as expected: %v", err)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
