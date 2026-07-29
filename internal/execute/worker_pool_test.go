package execute

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"syncd/internal/fs"
	"syncd/internal/reconcile"
)

// Regression test: RunActions must never hang when ctx is cancelled while
// actions are still being dispatched. It previously returned early from the
// dispatch goroutine without closing jobs or results, deadlocking both the
// worker pool (blocked reading from an open, unclosed jobs channel) and the
// caller (blocked reading from an open, unclosed results channel).
func TestRunActions_CancelledContextDoesNotHang(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before RunActions is even called

	actions := make([]reconcile.Action, 20)
	for i := range actions {
		actions[i] = reconcile.Action{Type: reconcile.ActionNoOp, Path: "noop"}
	}

	type outcome struct {
		results []Result
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		results, err := RunActions(ctx, 2, SyncState{}, actions)
		done <- outcome{results: results, err: err}
	}()

	select {
	case o := <-done:
		if o.err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", o.err)
		}
		if len(o.results) > len(actions) {
			t.Fatalf("got more results (%d) than actions submitted (%d)", len(o.results), len(actions))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunActions did not return within 5s of context cancellation - deadlock")
	}
}

// TestRunActions_WorkersClampedToMinimumOne is a regression test: with
// workers == 0, no worker goroutines started, so results closed empty
// (results=0, err=nil) while the feeder goroutine blocked forever trying to
// send on the unbuffered jobs channel - a sync engine silently reporting
// success while syncing nothing, and leaking a goroutine. RunActions must
// clamp workers to a minimum of 1, matching what SyncOnce already does.
func TestRunActions_WorkersClampedToMinimumOne(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	state := SyncState{
		LocalRoot:  left,
		RemoteRoot: right,
		LocalFiles: map[string]fs.FileEntry{
			"a.txt": {Path: "a.txt", AbsPath: filepath.Join(left, "a.txt"), Size: 2},
		},
	}
	actions := []reconcile.Action{{Type: reconcile.ActionUpload, Path: "a.txt"}}

	type outcome struct {
		results []Result
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		results, err := RunActions(context.Background(), 0, state, actions)
		done <- outcome{results: results, err: err}
	}()

	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("RunActions returned error: %v", o.err)
		}
		if len(o.results) != 1 {
			t.Fatalf("expected 1 result with workers=0, got %d", len(o.results))
		}
		if o.results[0].Error != nil {
			t.Fatalf("upload failed: %v", o.results[0].Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunActions(workers=0) did not return within 5s - feeder goroutine leaked")
	}

	if b, err := os.ReadFile(filepath.Join(right, "a.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("upload did not happen with workers=0: err=%v content=%q", err, b)
	}
}

func TestRunActions_AllActionsCompleteWithoutCancellation(t *testing.T) {
	actions := make([]reconcile.Action, 10)
	for i := range actions {
		actions[i] = reconcile.Action{Type: reconcile.ActionNoOp, Path: "noop"}
	}

	results, err := RunActions(context.Background(), 3, SyncState{}, actions)
	if err != nil {
		t.Fatalf("RunActions returned error: %v", err)
	}
	if len(results) != len(actions) {
		t.Fatalf("expected %d results, got %d", len(actions), len(results))
	}
	for _, res := range results {
		if res.Error != nil {
			t.Fatalf("unexpected error on NO_OP action: %v", res.Error)
		}
	}
}
