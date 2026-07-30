# Follow-Ups Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve the six actionable items in `docs/follow-ups.md` (the "Worth
doing" and "Test-coverage gaps" sections, items #1-#6) from the 2026-07-29
full-codebase review, each with a pinning test and/or a load-bearing comment
so the invariant survives future edits.

**Architecture:** No new abstractions or files. Every task is a targeted edit
to an existing file (a comment, a doc string, a sort, or a test) plus the test
that proves it. Items #7-#10 in `docs/follow-ups.md` ("Deliberate, revisit
only if it bites") are explicitly out of scope — the review already judged
them correct as-is; this plan does not touch them beyond leaving them in
place in the doc.

**Tech Stack:** Go 1.26.4, standard `testing` package, no new dependencies.

## Global Constraints

- Module path `syncd`; imports `syncd/internal/...`. Must build darwin **and**
  linux (the plan touches no platform-specific code, but keep this in mind).
- Never rename `.synclock`, never touch the two deliberately-broken import
  cycles (`runtime.Config`, `app.RunWatch`'s injected `WatchLoopFunc`) — none
  of these tasks come near them, but don't introduce a new dependency that
  would.
- `go build ./... && go vet ./... && go test ./...` must stay green before
  every commit; anything touching `internal/watch` must also pass
  `go test ./... -race`.
- Follow the existing test-seam convention already used in this package:
  `internal/watch/watcher.go`'s `fsAdd` package-level func var, swapped in
  tests via `orig := fsAdd; fsAdd = fake; defer func(){ fsAdd = orig }()`.
  Task 6 adds a second var (`newTicker`) using the identical pattern — do not
  invent a different seam mechanism (e.g. an interface, a `Clock` type).
- No placeholders: every step below has the literal code to write.

---

### Task 1: Pin the `SkipSet` dir-equals-own-path invariant

**Files:**
- Modify: `internal/fs/scanner.go:185-194` (the per-entry `walkErr` branch)
- Test: `internal/fs/scanner_test.go`

**Interfaces:**
- Consumes: `ScanResult.Skipped map[string]struct{}`, `ScanResult.SkippedDirs map[string]struct{}` (both already defined in `internal/fs/scanner.go`)
- Produces: nothing new — this task only adds a regression test and a comment; no signature changes.

- [ ] **Step 1: Write the failing test**

Add to `internal/fs/scanner_test.go` (place it right after
`TestScanTree_UnreadableSubdirectoryRecordsSkippedDir`, which it complements):

```go
// TestScanTree_SkippedDirRecordedInBothSkippedAndSkippedDirs pins an
// invariant load-bearing for reconcile.SkipSet: ScanTree must record an
// unreadable directory's own relpath in BOTH Skipped (exact) and
// SkippedDirs (subtree). reconcile.SkipSet.Contains only ever treats a path
// as skipped via an exact Skipped match or as a descendant of a SkippedDirs
// entry - dirPrefixes never returns a path's own name as one of its own
// prefixes - so a directory's own path relies entirely on also being present
// in Skipped. If scanner.go's per-entry walkErr branch ever stopped writing
// to Skipped before the d.IsDir() check that populates SkippedDirs, this
// would silently reopen the subtree-delete data-loss bug SkipSet exists to
// prevent (see docs/follow-ups.md #1).
func TestScanTree_SkippedDirRecordedInBothSkippedAndSkippedDirs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "good.txt"), "good")
	badDir := filepath.Join(root, "locked")
	if err := os.Mkdir(badDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(badDir, "hidden.txt"), "hidden")
	if err := os.Chmod(badDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(badDir, 0o755) })

	result, err := ScanTree(context.Background(), root, nil, false, newTestMatcher(t, root), HashAlways, nil)
	if err != nil {
		t.Fatalf("ScanTree returned error for an unreadable subdirectory, want success: %v", err)
	}
	if _, ok := result.Skipped["locked"]; !ok {
		t.Errorf("expected %q recorded in Skipped, got %+v", "locked", result.Skipped)
	}
	if _, ok := result.SkippedDirs["locked"]; !ok {
		t.Errorf("expected %q recorded in SkippedDirs, got %+v", "locked", result.SkippedDirs)
	}
}
```

- [ ] **Step 2: Run the test to verify it currently passes (this is a pinning test, not a bugfix — it should already pass)**

Run: `go test ./internal/fs/... -run TestScanTree_SkippedDirRecordedInBothSkippedAndSkippedDirs -v`
Expected: PASS (confirms current behavior is correct before we lock it in with a comment)

- [ ] **Step 3: Add the load-bearing comment in scanner.go**

In `internal/fs/scanner.go`, find this block inside `ScanTree`'s `filepath.WalkDir` callback:

```go
			result.Issues = append(result.Issues, ScanIssue{Path: relPath, Reason: walkErr.Error()})
			result.Skipped[relPath] = struct{}{}
			if d != nil && d.IsDir() {
```

Replace it with:

```go
			result.Issues = append(result.Issues, ScanIssue{Path: relPath, Reason: walkErr.Error()})
			// Load-bearing order: this write to Skipped must happen before the
			// d.IsDir() check below. reconcile.SkipSet.Contains only ever
			// matches a directory's OWN path via the exact Skipped set -
			// dirPrefixes(path) never returns path itself as one of its own
			// prefixes - so if this line moved after the IsDir branch, a
			// skipped directory's own path would silently stop being treated
			// as skipped by every reconciler (see
			// TestScanTree_SkippedDirRecordedInBothSkippedAndSkippedDirs).
			result.Skipped[relPath] = struct{}{}
			if d != nil && d.IsDir() {
```

- [ ] **Step 4: Run the full fs package test suite**

Run: `go test ./internal/fs/... -v`
Expected: PASS, all tests including the new one

- [ ] **Step 5: Commit**

```bash
git add internal/fs/scanner.go internal/fs/scanner_test.go
git commit -m "$(cat <<'EOF'
test(fs): pin the SkipSet dir-equals-own-path invariant

Add a regression test asserting an unreadable directory's own relpath
lands in both Skipped and SkippedDirs, and a load-bearing comment at
the scanner.go site so an edit that reorders those two writes doesn't
silently reopen the subtree-delete data-loss bug SkipSet exists to
prevent.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Sort the `prev` iteration in two-way move detection

**Files:**
- Modify: `internal/reconcile/fast.go:128-158` (the `leftMissingSigs`/`rightMissingSigs` pre-pass)

**Interfaces:**
- Consumes: `db.JobFileState`, `fastSignature`, `fastGetSig` (all already defined in this file)
- Produces: no signature changes — `FastCompare`'s exported behavior is unchanged, only its internal iteration order.

- [ ] **Step 1: Make the change**

In `internal/reconcile/fast.go`, inside the `FastTwoWay` branch, find:

```go
		leftMissingSigs := make(map[fastSignature]string)
		leftMissingCollided := make(map[fastSignature]bool)
		rightMissingSigs := make(map[fastSignature]string)
		rightMissingCollided := make(map[fastSignature]bool)

		for path, state := range prev {
			if skipped.Contains(path) {
```

Replace the loop header with a sorted-slice iteration (leave everything
inside the loop body untouched):

```go
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
```

The rest of the loop body (`sig := fastGetSig(state)` through its closing
`}`) stays exactly as it is today — only the `for` header and the added
sort change.

- [ ] **Step 2: Run the reconcile package tests, with repetition to catch any order-dependent regression**

Run: `go test ./internal/reconcile/... -run TestFastTwoWay -v -count=20`
Expected: PASS on all 20 runs (this exercises `TestFastTwoWayMoveIsDeterministic` and `TestFastTwoWaySignatureCollisionFallsBackToCopyDelete`, which already iterate 200/50 times each)

- [ ] **Step 3: Run the full reconcile suite**

Run: `go test ./internal/reconcile/... -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/reconcile/fast.go
git commit -m "$(cat <<'EOF'
refactor(reconcile): sort the prev iteration in two-way move detection

The leftMissingSigs/rightMissingSigs pre-pass iterated prev as a bare
map while a comment a few lines below already documents that sorted
iteration is what makes FastCompare deterministic. It was already
order-invariant (collision handling is symmetric), but sorting costs
nothing at this scale and removes the need to re-derive that from
scratch on every read.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Document the `--dry-run` state-dir/lock decision

**Files:**
- Modify: `internal/cli/sync_cmd.go:62` (the `--dry-run` flag's help text)
- Modify: `internal/app/sync_once.go:32` (the `SyncOptions.DryRun` field doc comment)

**Interfaces:**
- Consumes: nothing new.
- Produces: nothing new — documentation only, no behavior change. The
  decision made here is: **keep** current behavior (a dry run still acquires
  the per-job flock and opens/creates the state db), because a dry run
  legitimately reads shared job state and skipping the lock would leave a
  dry run racing a real run without a documented answer either way.

- [ ] **Step 1: Update the flag help text**

In `internal/cli/sync_cmd.go`, find:

```go
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "Print the sync summary and exit without changing anything or persisting job state (does not require --yes)")
```

Replace with:

```go
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "Print the sync summary and exit without changing anything or persisting job state (does not require --yes). Still acquires the per-job lock and opens/creates the state database, since a dry run reads existing job state and that read should be serialized with real runs the same way a real run's read is.")
```

- [ ] **Step 2: Update the `SyncOptions.DryRun` doc comment**

In `internal/app/sync_once.go`, find:

```go
	DryRun            bool // --dry-run: print the summary and exit without mutating anything or persisting job state
```

Replace with:

```go
	DryRun            bool // --dry-run: print the summary and exit without mutating anything or persisting job state. Still acquires the per-job lock (cli.runSyncJob) and opens/creates the state db (SyncOnce below) - deliberate, since this is a read of shared job state, not a mutation; see docs/follow-ups.md #3.
```

- [ ] **Step 3: Rebuild and run the cli/app package tests to confirm nothing else references the old help string verbatim**

Run: `go build ./... && go test ./internal/cli/... ./internal/app/... -v`
Expected: PASS, no test asserts the literal old help string

- [ ] **Step 4: Commit**

```bash
git add internal/cli/sync_cmd.go internal/app/sync_once.go
git commit -m "$(cat <<'EOF'
docs(sync): document the --dry-run state-dir/lock decision

--dry-run has always acquired the per-job lock and opened/created the
state db (harmless - nothing under either sync root is touched - but
previously undocumented and inconsistent-looking next to the effort
spent keeping --dry-run from creating the destination root). Decide
deliberately to keep this behavior (a dry run's read of shared job
state should serialize with real runs the same way a real run's read
does) and document it in the flag help and the SyncOptions.DryRun
field comment.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Pin `ScanTree`'s root-`ReadDir`-failure abort shape

**Files:**
- Test: `internal/fs/scanner_test.go`

**Interfaces:**
- Consumes: `ScanTree(ctx, root, manifest, validateSMBName, matcher, policy, progress) (ScanResult, error)` (existing signature, unchanged)
- Produces: nothing new — test only.

- [ ] **Step 1: Write the test**

Add to `internal/fs/scanner_test.go`, right after
`TestScanTree_RootWalkErrorAborts` (which pins the "root doesn't exist"
shape this test complements):

```go
// TestScanTree_RootReadDirFailureAborts pins the other root-failure shape
// from docs/follow-ups.md #4: TestScanTree_RootWalkErrorAborts covers "root
// doesn't exist" (an Lstat failure on root). This covers "root exists but
// its own ReadDir fails" (e.g. permission denied) - filepath.WalkDir invokes
// the walk callback a SECOND time for root itself when os.ReadDir(root)
// fails, with that error attached, and ScanTree's `if path == root { return
// walkErr }` branch must treat that as a whole-scan abort too, not silently
// record it as a per-entry issue and continue as if the tree were empty.
func TestScanTree_RootReadDirFailureAborts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "locked-root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	_, err := ScanTree(context.Background(), root, nil, false, newTestMatcher(t, parent), HashAlways, nil)
	if err == nil {
		t.Fatal("expected an error when the root itself exists but its ReadDir fails, got nil")
	}
}
```

Note: `newTestMatcher(t, parent)` is passed `parent`, not `root` — the
matcher only needs a valid directory to load `.syncdignore` from, and `root`
is chmod'd 000 before the matcher would be loaded from it.

- [ ] **Step 2: Run the test**

Run: `go test ./internal/fs/... -run TestScanTree_RootReadDirFailureAborts -v`
Expected: PASS

- [ ] **Step 3: Run the full fs package suite**

Run: `go test ./internal/fs/... -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/fs/scanner_test.go
git commit -m "$(cat <<'EOF'
test(fs): pin ScanTree's root-ReadDir-failure abort shape

TestScanTree_RootWalkErrorAborts only covered a missing root (Lstat
failure). Add the other root-failure shape - root exists but its own
ReadDir fails (e.g. chmod 000) - which hits the same `path == root`
abort branch via filepath.WalkDir's second invocation of the walk
callback, but was previously untested.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Symmetric signature-collision test + `NewState` assertions

**Files:**
- Modify: `internal/reconcile/fast_test.go`

**Interfaces:**
- Consumes: `FastCompare(mode, left, right, prev, skipped) FastResult` (existing signature, unchanged)
- Produces: nothing new — test only.

- [ ] **Step 1: Add a `NewState` assertion to the existing left-missing collision test**

In `internal/reconcile/fast_test.go`, find
`TestFastTwoWaySignatureCollisionFallsBackToCopyDelete`:

```go
func TestFastTwoWaySignatureCollisionFallsBackToCopyDelete(t *testing.T) {
	prev := map[string]db.JobFileState{
		"old1.txt": {Path: "old1.txt", Size: 5, Mtime: 100},
		"old2.txt": {Path: "old2.txt", Size: 5, Mtime: 100},
	}
	left := map[string]db.JobFileState{
		"new.txt": {Path: "new.txt", Size: 5, Mtime: 100},
	}
	right := map[string]db.JobFileState{
		"old1.txt": {Path: "old1.txt", Size: 5, Mtime: 100},
		"old2.txt": {Path: "old2.txt", Size: 5, Mtime: 100},
	}

	for i := 0; i < 50; i++ {
		res := FastCompare(FastTwoWay, left, right, prev, SkipSet{})
		for _, task := range res.Tasks {
			if task.Type == MoveR || task.Type == MoveL {
				t.Fatalf("run %d: expected no move for colliding signatures, got %+v in %+v", i, task, res.Tasks)
			}
		}
		if len(res.Tasks) != 3 {
			t.Fatalf("run %d: expected exactly 3 tasks (1 copy + 2 deletes), got %d: %+v", i, len(res.Tasks), res.Tasks)
		}
		assertHasTask(t, res, FastTask{Type: CopyL2R, RelPath: "new.txt", Size: 5})
		assertHasTask(t, res, FastTask{Type: DeleteR, RelPath: "old1.txt"})
		assertHasTask(t, res, FastTask{Type: DeleteR, RelPath: "old2.txt"})
	}
}
```

Replace it with (adds `wantState` and the `NewState` check inside the loop):

```go
func TestFastTwoWaySignatureCollisionFallsBackToCopyDelete(t *testing.T) {
	prev := map[string]db.JobFileState{
		"old1.txt": {Path: "old1.txt", Size: 5, Mtime: 100},
		"old2.txt": {Path: "old2.txt", Size: 5, Mtime: 100},
	}
	left := map[string]db.JobFileState{
		"new.txt": {Path: "new.txt", Size: 5, Mtime: 100},
	}
	right := map[string]db.JobFileState{
		"old1.txt": {Path: "old1.txt", Size: 5, Mtime: 100},
		"old2.txt": {Path: "old2.txt", Size: 5, Mtime: 100},
	}
	wantState := map[string]db.JobFileState{
		"new.txt": {Path: "new.txt", Size: 5, Mtime: 100},
	}

	for i := 0; i < 50; i++ {
		res := FastCompare(FastTwoWay, left, right, prev, SkipSet{})
		for _, task := range res.Tasks {
			if task.Type == MoveR || task.Type == MoveL {
				t.Fatalf("run %d: expected no move for colliding signatures, got %+v in %+v", i, task, res.Tasks)
			}
		}
		if len(res.Tasks) != 3 {
			t.Fatalf("run %d: expected exactly 3 tasks (1 copy + 2 deletes), got %d: %+v", i, len(res.Tasks), res.Tasks)
		}
		assertHasTask(t, res, FastTask{Type: CopyL2R, RelPath: "new.txt", Size: 5})
		assertHasTask(t, res, FastTask{Type: DeleteR, RelPath: "old1.txt"})
		assertHasTask(t, res, FastTask{Type: DeleteR, RelPath: "old2.txt"})
		if !reflect.DeepEqual(res.NewState, wantState) {
			t.Fatalf("run %d: NewState not deterministic/correct: got %+v, want %+v", i, res.NewState, wantState)
		}
	}
}
```

- [ ] **Step 2: Run it to confirm the added assertion passes**

Run: `go test ./internal/reconcile/... -run TestFastTwoWaySignatureCollisionFallsBackToCopyDelete -v`
Expected: PASS

- [ ] **Step 3: Add the symmetric right-missing-direction test**

Add this new test directly after
`TestFastTwoWaySignatureCollisionFallsBackToCopyDelete` in the same file:

```go
// TestFastTwoWaySignatureCollisionFallsBackToCopyDelete_RightMissingDirection
// mirrors TestFastTwoWaySignatureCollisionFallsBackToCopyDelete on the other
// side: two files vanished from RIGHT (not left) since prev, colliding on
// signature. rightMissingSigs is keyed by signature alone exactly like
// leftMissingSigs, so the same "a collision means no match" safety rule must
// hold in this direction too - structurally identical to the left-missing
// case but previously untested (docs/follow-ups.md #5).
func TestFastTwoWaySignatureCollisionFallsBackToCopyDelete_RightMissingDirection(t *testing.T) {
	prev := map[string]db.JobFileState{
		"old1.txt": {Path: "old1.txt", Size: 5, Mtime: 100},
		"old2.txt": {Path: "old2.txt", Size: 5, Mtime: 100},
	}
	left := map[string]db.JobFileState{
		"old1.txt": {Path: "old1.txt", Size: 5, Mtime: 100},
		"old2.txt": {Path: "old2.txt", Size: 5, Mtime: 100},
	}
	right := map[string]db.JobFileState{
		"new.txt": {Path: "new.txt", Size: 5, Mtime: 100},
	}
	wantState := map[string]db.JobFileState{
		"new.txt": {Path: "new.txt", Size: 5, Mtime: 100},
	}

	for i := 0; i < 50; i++ {
		res := FastCompare(FastTwoWay, left, right, prev, SkipSet{})
		for _, task := range res.Tasks {
			if task.Type == MoveR || task.Type == MoveL {
				t.Fatalf("run %d: expected no move for colliding signatures, got %+v in %+v", i, task, res.Tasks)
			}
		}
		if len(res.Tasks) != 3 {
			t.Fatalf("run %d: expected exactly 3 tasks (1 copy + 2 deletes), got %d: %+v", i, len(res.Tasks), res.Tasks)
		}
		assertHasTask(t, res, FastTask{Type: CopyR2L, RelPath: "new.txt", Size: 5})
		assertHasTask(t, res, FastTask{Type: DeleteL, RelPath: "old1.txt"})
		assertHasTask(t, res, FastTask{Type: DeleteL, RelPath: "old2.txt"})
		if !reflect.DeepEqual(res.NewState, wantState) {
			t.Fatalf("run %d: NewState not deterministic/correct: got %+v, want %+v", i, res.NewState, wantState)
		}
	}
}
```

- [ ] **Step 4: Run the new test**

Run: `go test ./internal/reconcile/... -run TestFastTwoWaySignatureCollisionFallsBackToCopyDelete_RightMissingDirection -v`
Expected: PASS

- [ ] **Step 5: Run the full reconcile package suite**

Run: `go test ./internal/reconcile/... -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/reconcile/fast_test.go
git commit -m "$(cat <<'EOF'
test(reconcile): cover the symmetric signature-collision direction

TestFastTwoWaySignatureCollisionFallsBackToCopyDelete only covered
files vanishing from the left side. Add the structurally-identical
right-missing-direction case, and assert NewState (the next run's
baseline) in both, not just Tasks.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Assert the watch loop's ticker cadence

**Files:**
- Modify: `internal/watch/loop.go`
- Test: `internal/watch/loop_test.go`

**Interfaces:**
- Consumes: `time.NewTicker(d time.Duration) *time.Ticker` (stdlib)
- Produces: `newTicker` — a new package-level `var` in `internal/watch`
  of type `func(time.Duration) *time.Ticker`, defaulting to `time.NewTicker`.
  Tests in this package (and only this package) may swap it, following the
  exact same pattern `watcher.go`'s `fsAdd` already uses.

- [ ] **Step 1: Add the `newTicker` seam in loop.go**

In `internal/watch/loop.go`, find the `const` block:

```go
const (
	defaultRemotePoll = 30 * time.Second
	defaultDeepScan   = time.Hour
)
```

Add directly after it:

```go

// newTicker is time.NewTicker, indirected through a package-level variable
// purely so tests can observe which durations Run actually passes to it -
// e.g. confirming a non-positive RemotePoll/DeepScan really gets clamped to
// the documented 30s/1h defaults - without a full fake clock. Same pattern
// as watcher.go's fsAdd.
var newTicker = time.NewTicker
```

- [ ] **Step 2: Use it in `Run`**

In the same file, find:

```go
	pollTicker := time.NewTicker(remotePoll)
	defer pollTicker.Stop()
	deepTicker := time.NewTicker(deepScan)
	defer deepTicker.Stop()
```

Replace with:

```go
	pollTicker := newTicker(remotePoll)
	defer pollTicker.Stop()
	deepTicker := newTicker(deepScan)
	defer deepTicker.Stop()
```

- [ ] **Step 3: Build to confirm the seam compiles**

Run: `go build ./...`
Expected: succeeds with no errors

- [ ] **Step 4: Write the cadence-assertion test**

In `internal/watch/loop_test.go`, add `"sync"` to the import block (it is not
currently imported):

```go
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
```

Then add this test directly after `TestRunClampsNonPositiveDurationsToDefaults`:

```go
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
```

- [ ] **Step 5: Run the new test**

Run: `go test ./internal/watch/... -run TestRunClampsNonPositiveDurationsToDefaults_AssertsCadence -v -race`
Expected: PASS

- [ ] **Step 6: Run the full watch package suite with the race detector**

Run: `go test ./internal/watch/... -race -v`
Expected: PASS (including the pre-existing `TestLoopSyncsOnLocalEvent`,
`TestRunClampsNonPositiveDurationsToDefaults`, and
`TestRunLogsWatcherAddErrorsOnce`)

- [ ] **Step 7: Commit**

```bash
git add internal/watch/loop.go internal/watch/loop_test.go
git commit -m "$(cat <<'EOF'
test(watch): assert the watch loop's clamped ticker cadence

TestRunClampsNonPositiveDurationsToDefaults only asserted Run doesn't
panic or hang on a non-positive RemotePoll/DeepScan, not that the
clamp actually lands on the documented 30s/1h defaults. Add a newTicker
seam (same pattern as watcher.go's fsAdd) so a test can observe the
actual durations Run passes to time.NewTicker, without a full fake
clock.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Update `docs/follow-ups.md` and run final full verification

**Files:**
- Modify: `docs/follow-ups.md`

**Interfaces:** none — documentation and verification only.

- [ ] **Step 1: Remove items #1-#6 from the doc, keeping #7-#10 (renumbered) and the intro**

Replace the full contents of `docs/follow-ups.md` with:

```markdown
# Known follow-ups

Deferred items from the full-codebase review of 2026-07-29 (merged as `edb6285`).
Each was reviewed and consciously judged **safe to defer** — none is a known
correctness bug. They are recorded here rather than in git-ignored scratch so
they survive a fresh clone.

Items #1-#6 from the original review pass (the `SkipSet` dir-equals-own-path
invariant, the unsorted-map comment in two-way move detection, the `--dry-run`
state-dir decision, the `ScanTree` root-`ReadDir`-failure test, the
signature-collision symmetric test, and the watch-loop cadence assertion)
were resolved on 2026-07-30 — see the git history for that date. What
remains below are the items deliberately judged not worth doing unless they
actually bite.

## Deliberate, revisit only if it bites

### 1. Dry-run preview scans sequentially

`internal/app/sync_once.go` — the dry-run preview scans left and right in
sequence rather than concurrently like `fs.ScanPair`. Intentional: the preview is
not the hot path. Revisit only if previewing a large tree over SMB feels slow.

### 2. Dry-run cannot distinguish "not created yet" from "mount vanished"

`internal/app/sync_once.go` — the preview's "missing root → treat as empty"
fallback can't tell a destination that was never created from an SMB mount that
disappeared. Preview-only and mutation-free, and real runs are unaffected
(`ScanTree` still aborts on a missing root, which is how a vanished mount is
detected). Would matter only if dry-run output were ever trusted for automation.

### 3. Scan-issue logging is one `Warn` per side per cycle, forever

`internal/app/sync_loop.go` — per-path detail goes to the log file at `Info`, and
one aggregated `Warn` per side per cycle goes to stderr. For a tree with a
permanently unreadable path, that is one stderr line per side every
`--remote-poll` (30s default), indefinitely. Bounded (O(1) per cycle, not O(N)),
but not silent. Full cross-cycle `(path, reason)` dedup was considered and not
built.

### 4. `ScanPaths` does not populate `SkippedDirs`

`internal/fs/scan_paths.go` — by design. `ScanPaths` only ever processes explicit
paths from fsnotify events and never discovers descendants, so a file made
unreachable by a newly-unreadable ancestor fails its own `Lstat` and gets an
exact `Skipped` entry directly. The unreadable-directory case is caught by the
next full `RunRobustOnce` cycle. Worth a comment if anyone ever gives `ScanPaths`
directory-recursion.
```

- [ ] **Step 2: Run the full verification suite (per CLAUDE.md, required green before any commit)**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass, no errors

- [ ] **Step 3: Run the race detector on the concurrency-sensitive packages**

Run: `go test ./... -race`
Expected: all pass, no race warnings

- [ ] **Step 4: Cross-compile check for both target platforms**

Run:
```bash
GOOS=darwin GOARCH=arm64 go build -o /dev/null ./cmd/syncd
GOOS=linux  GOARCH=amd64 go build -o /dev/null ./cmd/syncd
```
Expected: both succeed

- [ ] **Step 5: Commit**

```bash
git add docs/follow-ups.md
git commit -m "$(cat <<'EOF'
docs: mark follow-ups #1-#6 resolved, renumber the remaining deferrals

Items #1-#6 (SkipSet dir invariant, sorted move-detection iteration,
--dry-run state-dir documentation, ScanTree root-ReadDir-failure test,
symmetric signature-collision test, watch-loop cadence assertion) are
implemented; only the deliberately-deferred items remain in the doc.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Self-Review Notes

- **Spec coverage:** Every actionable item in `docs/follow-ups.md`'s "Worth
  doing" (#1-#3) and "Test-coverage gaps" (#4-#6) sections has a task above.
  Items #7-#10 ("Deliberate, revisit only if it bites") are explicitly out of
  scope per the doc's own framing and are preserved, renumbered, in Task 7.
- **Type consistency:** Task 6's `newTicker` type (`func(time.Duration)
  *time.Ticker`) matches `time.NewTicker`'s actual signature exactly, and is
  the only new identifier introduced across all seven tasks — every other
  task is a comment, a doc string, a sort, or a test using pre-existing types
  (`db.JobFileState`, `FastCompare`, `ScanTree`, `SkipSet`).
- **No placeholders:** every step above contains literal, complete code —
  no "add appropriate test" or "similar to Task N" deferrals.
