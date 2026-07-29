package execute

import (
	"context"
	"testing"
	"time"

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
