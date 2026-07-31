package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"syncd/internal/app"
	"syncd/internal/db"
	"syncd/internal/fs"
	"syncd/internal/runtime"
)

func TestLoopSyncsOnLocalEvent(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	// canary so CheckRemoteCanary passes
	os.WriteFile(filepath.Join(remote, ".synclock"), []byte("x"), 0o644)
	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	m, _ := fs.LoadIgnoreMatcher(local)
	p := app.RobustParams{LocalRoot: local, RemoteRoot: remote, Workers: 2, Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, p, m, LoopConfig{RemotePoll: time.Hour, DeepScan: time.Hour, Debounce: 80 * time.Millisecond})

	time.Sleep(150 * time.Millisecond) // let the watcher arm
	os.WriteFile(filepath.Join(local, "a.txt"), []byte("hi"), 0o644)

	deadline := time.After(3 * time.Second)
	for {
		if b, _ := os.ReadFile(filepath.Join(remote, "a.txt")); string(b) == "hi" {
			return // success
		}
		select {
		case <-deadline:
			t.Fatal("local event did not propagate to remote")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestRunClampsNonPositiveDurationsToDefaults is a regression test: Run
// passes cfg.RemotePoll/cfg.DeepScan straight to time.NewTicker, which
// panics on a non-positive duration ("non-positive interval for
// NewTicker"). A user-supplied --remote-poll 0 (or a negative value) must
// not be able to crash the always-on daemon - Run must fall back to the
// documented defaults (30s / 1h) instead.
func TestRunClampsNonPositiveDurationsToDefaults(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(remote, ".synclock"), []byte("x"), 0o644)
	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	m, _ := fs.LoadIgnoreMatcher(local)
	p := app.RobustParams{LocalRoot: local, RemoteRoot: remote, Workers: 1, Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, p, m, LoopConfig{RemotePoll: 0, DeepScan: -1, Debounce: 10 * time.Millisecond})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancellation - possibly panicked or hung on non-positive ticker duration")
	}
}

// TestRunClampsNonPositiveDurationsToDefaults_AssertsCadence closes the gap
// left by TestRunClampsNonPositiveDurationsToDefaults (docs/follow-ups.md
// #6): that test only proved Run doesn't panic or hang on a non-positive
// duration, not that the clamp actually lands on the documented 30s/1h
// defaults. newTicker is swapped here to record every duration Run passes to
// it, without needing a full fake clock.
func TestRunClampsNonPositiveDurationsToDefaults_AssertsCadence(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(remote, ".synclock"), []byte("x"), 0o644)
	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	m, _ := fs.LoadIgnoreMatcher(local)
	p := app.RobustParams{LocalRoot: local, RemoteRoot: remote, Workers: 1, Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger}

	var mu sync.Mutex
	var got []time.Duration
	orig := newTicker
	newTicker = func(d time.Duration) *time.Ticker {
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
		return orig(d)
	}
	defer func() { newTicker = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, p, m, LoopConfig{RemotePoll: 0, DeepScan: -1, Debounce: 10 * time.Millisecond})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancellation - possibly panicked or hung on non-positive ticker duration")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 newTicker calls (poll ticker + deep scan ticker), got %d: %v", len(got), got)
	}
	if got[0] != defaultRemotePoll {
		t.Errorf("RemotePoll=0 should clamp to defaultRemotePoll (%v), got %v", defaultRemotePoll, got[0])
	}
	if got[1] != defaultDeepScan {
		t.Errorf("DeepScan=-1 should clamp to defaultDeepScan (%v), got %v", defaultDeepScan, got[1])
	}
}

// TestDeepScanCatchesSameSizeSameMtimeContentChange is a regression test for
// review finding "High 5": the deep-scan ticker branch must use
// fs.HashAlways (per RobustParams.Policy's own doc comment: "HashAlways for
// deep verify"), not fs.HashIfChanged - otherwise a deep scan is byte-for-
// byte identical to a remote poll and never catches a same-size, same-mtime
// content change (the classic case HashIfChanged is documented to skip via
// its cached-hash reuse in fs.ScanTree). RemotePoll is set to an hour so only
// the deep-scan ticker can possibly be responsible for propagating the
// change within the test's deadline.
func TestDeepScanCatchesSameSizeSameMtimeContentChange(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(remote, ".synclock"), []byte("x"), 0o644)

	const relPath = "a.txt"
	localPath := filepath.Join(local, relPath)
	remotePath := filepath.Join(remote, relPath)

	if err := os.WriteFile(localPath, []byte("hello1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remotePath, []byte("hello1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Force a stable, already-in-the-past mtime on both sides so ScanTree's
	// racy-mtime guard (mtime < scanStart) never disqualifies the cached-hash
	// reuse path - that path is exactly what this test needs HashAlways to
	// bypass.
	pastMtime := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(localPath, pastMtime, pastMtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(remotePath, pastMtime, pastMtime); err != nil {
		t.Fatal(err)
	}

	hash1, err := fs.FileBLAKE3(localPath)
	if err != nil {
		t.Fatal(err)
	}

	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	repo := db.NewManifestRepo(dbh)
	if err := repo.ApplyUpdates(context.Background(), []db.ManifestRow{
		{Path: relPath, Size: 6, Mtime: pastMtime.Unix(), Hash: hash1, LastSynced: time.Now().Unix()},
	}, nil); err != nil {
		t.Fatal(err)
	}

	// Now change the local file's CONTENT only - same size (6 bytes), same
	// mtime as the manifest baseline. HashIfChanged would reuse the cached
	// (now-stale) manifest hash and see no change at all.
	if err := os.WriteFile(localPath, []byte("hello2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localPath, pastMtime, pastMtime); err != nil {
		t.Fatal(err)
	}

	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	m, _ := fs.LoadIgnoreMatcher(local)
	p := app.RobustParams{LocalRoot: local, RemoteRoot: remote, Workers: 1, Repo: repo, DB: dbh, Logger: logger}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, p, m, LoopConfig{RemotePoll: time.Hour, DeepScan: 100 * time.Millisecond, Debounce: 50 * time.Millisecond})

	deadline := time.After(3 * time.Second)
	for {
		if b, _ := os.ReadFile(remotePath); string(b) == "hello2" {
			return // success: the deep scan caught the same-size/same-mtime content change
		}
		select {
		case <-deadline:
			t.Fatal("deep scan did not propagate a same-size/same-mtime content change - it must use fs.HashAlways, not fs.HashIfChanged")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestRunLogsWatcherAddErrorsOnce checks that Run surfaces NewWatcher's
// best-effort AddErrors (Task 5d): the Watcher itself has no logger (kept
// as a known follow-up per CLAUDE.md), so Run - which does have p.Logger -
// must log them once after NewWatcher returns, rather than the degraded
// coverage passing silently.
func TestRunLogsWatcherAddErrorsOnce(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(remote, ".synclock"), []byte("x"), 0o644)
	if err := os.MkdirAll(filepath.Join(local, "bad"), 0o755); err != nil {
		t.Fatal(err)
	}

	orig := fsAdd
	fsAdd = func(fsw *fsnotify.Watcher, path string) error {
		if filepath.Base(path) == "bad" {
			return fmt.Errorf("simulated add failure")
		}
		return orig(fsw, path)
	}
	defer func() { fsAdd = orig }()

	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	logPath := filepath.Join(t.TempDir(), "l.log")
	logger, _ := runtime.NewLogger(true, logPath)
	defer logger.Close()
	m, _ := fs.LoadIgnoreMatcher(local)
	p := app.RobustParams{LocalRoot: local, RemoteRoot: remote, Workers: 1, Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := Run(ctx, p, m, LoopConfig{RemotePoll: time.Hour, DeepScan: time.Hour, Debounce: 50 * time.Millisecond}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "could not be watched") {
		t.Errorf("expected the watcher's add errors to be logged, got log contents:\n%s", data)
	}
}
