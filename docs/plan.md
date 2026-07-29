# syncd Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Merge `drivesync` (always-on SMB sync) and `file_sync_util` (fast ad-hoc sync) into one cross-platform binary, `syncd`, with a fast one-shot `sync` and an event-driven continuous `watch`.

**Architecture:** Build on the `drivesync` tree (renamed module `syncd`). One goroutine-concurrent scanner parameterized by a hash policy feeds two reconcilers (fast size+mtime; robust three-way hash). Ignore rules, NFC normalization, atomic-write executor, worker pool, and a single SQLite state store are shared. A new `internal/watch` package adds fsnotify local watching plus a periodic remote poll.

**Tech Stack:** Go, cobra+viper (CLI/config), pterm (interactive `sync` UI), fsnotify (watch), modernc.org/sqlite (state), blake3 (hashing). bbolt and doublestar are retired.

## Global Constraints

- Module path: `syncd`. All internal imports are `syncd/internal/...`.
- Binary name: `syncd`. Config file: `~/.syncd.yaml`. Ignore file: `.syncdignore`.
- State dir default: `~/.local/state/syncd/` (`manifest.db`, `sync.lock`, `sync.log`).
- Canary filename stays `.synclock` (already deployed on remote shares — do NOT rename).
- Cross-platform: every task must build and test on both macOS (darwin) and Ubuntu (linux). No OS-specific syscalls outside files already guarded.
- TDD: write the failing test first, watch it fail, implement, watch it pass, commit.
- Run `go build ./... && go vet ./... && go test ./...` green before every commit.
- Preserve drivesync's robustness invariants: atomic temp-file+rename writes, lockfile, canary check before remote touch, non-zero exit on any failed action, JSON `--silent` logging with rotation.

---

## File Structure

```
syncd/
  go.mod                          # module syncd
  cmd/syncd/main.go               # thin entrypoint -> cmd.Execute()
  internal/
    cli/
      root.go                     # cobra root + Execute()
      sync_cmd.go                 # `syncd sync`  (fast/ad-hoc; --hash bridge)
      watch_cmd.go                # `syncd watch` (continuous)
      init_cmd.go                 # `syncd init`
      config.go                   # viper batch-job config (~/.syncd.yaml)
    fs/
      scanner.go                  # unified scanner + HashPolicy (was drivesync)
      ignore.go                   # .syncdignore + macOS defaults
      normalize.go                # NFC (unchanged)
      atomic_copy.go  hash.go     # unchanged from drivesync
    reconcile/
      engine.go                   # robust three-way BuildPlan (unchanged)
      fast.go                     # fast size+mtime Compare + move detection (ported)
    execute/
      worker_pool.go              # robust action executor (unchanged)
      conflict.go                 # unchanged
      fast_exec.go                # fast-mode executor w/ pterm progress + moves
    db/
      sqlite.go                   # schema: file_manifest + job_state
      manifest_repo.go            # robust manifest (unchanged)
      jobstate_repo.go            # fast-mode namespaced size+mtime state
    reconcile_run/               # (in internal/app) orchestration
    app/
      sync_once.go                # orchestrates one fast (or --hash robust) run
      watch_run.go                # orchestrates the watch loop
      run.go                      # robust one-shot run (was app.Run, reused by watch cycles)
      directories.go              # empty-dir handling (unchanged)
    watch/
      watcher.go                  # fsnotify wrapper + debounce/coalesce
      loop.go                     # local-event + remote-poll + deep-scan scheduler
      incremental.go              # reconcile a restricted set of paths
    runtime/
      canary.go lock.go logging.go paths.go   # unchanged (paths dir -> syncd)
  deploy/
    systemd/syncd.service syncd.timer
    launchd/com.user.syncd.plist
  README.md
```

---

## Task 1: Scaffold the `syncd` module from the drivesync base

**Files:**
- Create: `syncd/` (new sibling of `drivesync/` and `macOS/`)
- Create: `syncd/cmd/syncd/main.go`
- Create: `syncd/internal/cli/root.go`
- Copy+rewrite: all of `drivesync/internal/**` into `syncd/internal/**`
- Create: `syncd/go.mod`

**Interfaces:**
- Produces: `cli.Execute()` — the cobra root entrypoint main() calls.

- [ ] **Step 1: Copy the drivesync tree and rewrite import paths**

```bash
cd /Users/william/remote/dev/tools
cp -R drivesync syncd
cd syncd
rm -f drivesync claude-analysis.md DriveSync_AI_Agent_Handover.pdf
rm -rf cmd/drivesync
mkdir -p cmd/syncd
# rewrite module path and all internal imports
sed -i '' 's#^module drivesync#module syncd#' go.mod
grep -rl 'drivesync/internal' --include='*.go' . | xargs sed -i '' 's#drivesync/internal#syncd/internal#g'
```
(On Linux use `sed -i` without the `''`.)

- [ ] **Step 2: Write the thin entrypoint**

`cmd/syncd/main.go`:
```go
package main

import "syncd/internal/cli"

func main() { cli.Execute() }
```

- [ ] **Step 3: Write a minimal cobra root (stub subcommands added in later tasks)**

`internal/cli/root.go`:
```go
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "syncd",
	Short: "Fast ad-hoc and always-on directory synchronization",
}

// Execute runs the root command; each subcommand registers itself in its own file's init().
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Add cobra dependency and tidy**

```bash
go get github.com/spf13/cobra@latest
go mod tidy
```

- [ ] **Step 5: Verify build**

Run: `go build ./... && go vet ./...`
Expected: builds clean; `syncd` binary via `go build -o syncd ./cmd/syncd`.

- [ ] **Step 6: Verify ported tests still pass**

Run: `go test ./...`
Expected: PASS (all drivesync tests, now under `syncd/...`).

- [ ] **Step 7: Commit**

```bash
git add -A && git commit -m "chore: scaffold syncd module from drivesync base"
```
(If not a git repo yet, run `git init` first — see plan note on version control.)

---

## Task 2: Rename ignore file to `.syncdignore` and fold in macOS defaults

**Files:**
- Modify: `internal/fs/ignore.go:16-23`
- Test: `internal/fs/ignore_test.go`

**Interfaces:**
- Produces: unchanged `LoadIgnoreMatcher(localRoot string) (*IgnoreMatcher, error)` and `(*IgnoreMatcher).Match(relPath string, isDir bool) bool`, now reading `.syncdignore` and excluding macOS junk by default.

- [ ] **Step 1: Write the failing test**

Add to `internal/fs/ignore_test.go`:
```go
func TestMacOSJunkExcludedByDefault(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadIgnoreMatcher(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a/.DS_Store", "b/._resourcefork", ".Spotlight-V100/x"} {
		if !m.Match(p, false) {
			t.Errorf("expected %q to be ignored by default", p)
		}
	}
}

func TestReadsSyncdignoreFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".syncdignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadIgnoreMatcher(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Match("build", true) {
		t.Error("expected build/ to be ignored from .syncdignore")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/fs/ -run 'MacOSJunk|Syncdignore' -v`
Expected: FAIL (still reads `.drivesyncignore`; no macOS defaults).

- [ ] **Step 3: Implement**

In `internal/fs/ignore.go` change the constant and defaults:
```go
const ignoreFileName = ".syncdignore"

var defaultIgnorePatterns = []string{
	".synclock", // remote canary (protocol file) — keep name as-is
	"*.tmp",     // atomic-write staging files
	"#recycle",  // Synology recycle bin
	".venv",     // Python virtualenvs
	// macOS junk (folded in from file_sync_util):
	".DS_Store",
	"._*",
	".fseventsd",
	".DocumentRevisions-V100",
	".Spotlight-V100",
	".TemporaryItems",
	".Trashes",
	"@eaDir",
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/fs/ -v`
Expected: PASS (new tests plus existing ignore tests).

- [ ] **Step 5: Commit**

```bash
git add internal/fs/ignore.go internal/fs/ignore_test.go
git commit -m "feat(fs): rename ignore file to .syncdignore and add macOS defaults"
```

---

## Task 3: Add a hash policy to the scanner

**Files:**
- Modify: `internal/fs/scanner.go`
- Test: `internal/fs/scanner_test.go`

**Interfaces:**
- Produces:
  ```go
  type HashPolicy int
  const (
      HashIfChanged HashPolicy = iota // hash only when size+mtime differ from manifest (default; == current behavior)
      HashNever                        // stat only; FileEntry.Hash == ""
      HashAlways                       // always hash
  )
  func ScanTree(ctx context.Context, root string, manifest map[string]db.ManifestRow, validateSMBName bool, matcher *IgnoreMatcher, policy HashPolicy, progress ScanProgressFunc) (ScanResult, error)
  func ScanPair(ctx context.Context, localRoot, remoteRoot string, manifest map[string]db.ManifestRow, policy HashPolicy, progress ScanPairProgressFunc) (ScanPairResult, error)
  ```
- Consumes: existing `db.ManifestRow`, `IgnoreMatcher`, `FileEntry`.
- NOTE: this changes two exported signatures. Update the one existing caller (`internal/app/run.go`, formerly `sync_loop.go`) to pass `HashIfChanged` so behavior is unchanged.

- [ ] **Step 1: Write the failing test**

Add to `internal/fs/scanner_test.go`:
```go
func TestScanTreeHashNeverSkipsHashing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := LoadIgnoreMatcher(dir)
	res, err := ScanTree(context.Background(), dir, nil, false, m, HashNever, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := res.Files["a.txt"]
	if !ok {
		t.Fatal("a.txt missing from scan")
	}
	if e.Hash != "" {
		t.Errorf("HashNever must not hash; got %q", e.Hash)
	}
	if e.Size != 5 {
		t.Errorf("size = %d, want 5", e.Size)
	}
}

func TestScanTreeHashAlwaysHashes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := LoadIgnoreMatcher(dir)
	res, err := ScanTree(context.Background(), dir, nil, false, m, HashAlways, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files["a.txt"].Hash == "" {
		t.Error("HashAlways must populate Hash")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/fs/ -run HashNever -v`
Expected: FAIL to compile (ScanTree has no policy parameter).

- [ ] **Step 3: Implement the policy**

In `internal/fs/scanner.go`, add the type and thread `policy` through `ScanTree`/`ScanPair`. Replace the hash-decision block (currently `scanner.go:170-178`) with:
```go
hash := ""
switch policy {
case HashNever:
	// leave hash empty; reconciler compares on size+mtime
case HashAlways:
	hash, err = FileBLAKE3(path)
	if err != nil {
		return fmt.Errorf("hash file %s: %w", path, err)
	}
default: // HashIfChanged
	if row, ok := manifest[relPath]; ok && row.Size == size && row.Mtime == mtime && mtime < scanStart {
		hash = row.Hash
	} else {
		hash, err = FileBLAKE3(path)
		if err != nil {
			return fmt.Errorf("hash file %s: %w", path, err)
		}
	}
}
```
Add the `HashPolicy` constants above `ScanTree`. Add the `policy HashPolicy` param to both `ScanTree` and `ScanPair` (pass it through to both goroutines in `ScanPair`).

- [ ] **Step 4: Update the existing caller**

In `internal/app/run.go` (the file formerly `sync_loop.go`), change the `fs.ScanPair(...)` call to pass `fs.HashIfChanged` in the new position.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/fs/ ./internal/app/ -v`
Expected: PASS (new policy tests + existing behavior preserved).

- [ ] **Step 6: Commit**

```bash
git add internal/fs/scanner.go internal/fs/scanner_test.go internal/app/run.go
git commit -m "feat(fs): parameterize scanner with never/if-changed/always hash policy"
```

---

## Task 4: Namespaced fast-mode state store in SQLite (retire bbolt)

**Files:**
- Modify: `internal/db/sqlite.go`
- Create: `internal/db/jobstate_repo.go`
- Test: `internal/db/jobstate_repo_test.go`

**Interfaces:**
- Produces:
  ```go
  type JobFileState struct { Path string; Size int64; Mtime int64 }
  type JobStateRepo struct { /* db *sql.DB */ }
  func NewJobStateRepo(db *sql.DB) *JobStateRepo
  func (r *JobStateRepo) Load(ctx context.Context, job string) (map[string]JobFileState, error)
  func (r *JobStateRepo) Replace(ctx context.Context, job string, states map[string]JobFileState) error
  ```
  This replaces file_sync_util's bbolt `LoadJobState`/`SaveJobState` with the same load-all / replace-all semantics, backed by SQLite so there is one state store.

- [ ] **Step 1: Write the failing test**

`internal/db/jobstate_repo_test.go`:
```go
package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestJobStateReplaceAndLoad(t *testing.T) {
	dbh, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
	repo := NewJobStateRepo(dbh)
	ctx := context.Background()

	in := map[string]JobFileState{"a.txt": {Path: "a.txt", Size: 5, Mtime: 100}}
	if err := repo.Replace(ctx, "docs", in); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Load(ctx, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if got["a.txt"].Size != 5 || got["a.txt"].Mtime != 100 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// Replace is total: removing a key drops it.
	if err := repo.Replace(ctx, "docs", map[string]JobFileState{}); err != nil {
		t.Fatal(err)
	}
	got, _ = repo.Load(ctx, "docs")
	if len(got) != 0 {
		t.Fatalf("expected empty after replace, got %d", len(got))
	}
}

func TestJobStateNamespacesAreIsolated(t *testing.T) {
	dbh, _ := Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	repo := NewJobStateRepo(dbh)
	ctx := context.Background()
	_ = repo.Replace(ctx, "job1", map[string]JobFileState{"x": {Path: "x", Size: 1, Mtime: 1}})
	_ = repo.Replace(ctx, "job2", map[string]JobFileState{"y": {Path: "y", Size: 2, Mtime: 2}})
	g1, _ := repo.Load(ctx, "job1")
	if _, ok := g1["y"]; ok {
		t.Error("job2 data leaked into job1")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run JobState -v`
Expected: FAIL (JobStateRepo undefined).

- [ ] **Step 3: Add the table to the migration**

In `internal/db/sqlite.go`, add after the `file_manifest` create:
```go
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS job_state (
			job   TEXT NOT NULL,
			path  TEXT NOT NULL,
			size  INTEGER NOT NULL,
			mtime INTEGER NOT NULL,
			PRIMARY KEY (job, path)
		);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate job_state schema: %w", err)
	}
```

- [ ] **Step 4: Implement the repo**

`internal/db/jobstate_repo.go`:
```go
package db

import (
	"context"
	"database/sql"
	"fmt"
)

type JobFileState struct {
	Path  string
	Size  int64
	Mtime int64
}

type JobStateRepo struct{ db *sql.DB }

func NewJobStateRepo(db *sql.DB) *JobStateRepo { return &JobStateRepo{db: db} }

func (r *JobStateRepo) Load(ctx context.Context, job string) (map[string]JobFileState, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT path, size, mtime FROM job_state WHERE job = ?`, job)
	if err != nil {
		return nil, fmt.Errorf("query job_state: %w", err)
	}
	defer rows.Close()
	out := make(map[string]JobFileState)
	for rows.Next() {
		var s JobFileState
		if err := rows.Scan(&s.Path, &s.Size, &s.Mtime); err != nil {
			return nil, fmt.Errorf("scan job_state: %w", err)
		}
		out[s.Path] = s
	}
	return out, rows.Err()
}

func (r *JobStateRepo) Replace(ctx context.Context, job string, states map[string]JobFileState) (err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin job_state tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `DELETE FROM job_state WHERE job = ?`, job); err != nil {
		return fmt.Errorf("clear job_state: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO job_state(job, path, size, mtime) VALUES(?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare job_state insert: %w", err)
	}
	defer stmt.Close()
	for _, s := range states {
		if _, err = stmt.ExecContext(ctx, job, s.Path, s.Size, s.Mtime); err != nil {
			return fmt.Errorf("insert job_state %s: %w", s.Path, err)
		}
	}
	return tx.Commit()
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/db/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/db/sqlite.go internal/db/jobstate_repo.go internal/db/jobstate_repo_test.go
git commit -m "feat(db): SQLite-backed namespaced job state (retires bbolt)"
```

---

## Task 5: Port the fast reconciler (size+mtime, with move detection)

**Files:**
- Create: `internal/reconcile/fast.go`
- Test: `internal/reconcile/fast_test.go`

**Interfaces:**
- Produces:
  ```go
  type FastMode string
  const ( FastMirror FastMode = "mirror"; FastUpdate FastMode = "update"; FastTwoWay FastMode = "two-way" )
  type FastTaskType string
  const (
      CopyL2R FastTaskType = "CopyL2R"; CopyR2L FastTaskType = "CopyR2L"
      DeleteL FastTaskType = "DeleteL"; DeleteR FastTaskType = "DeleteR"
      MoveL   FastTaskType = "MoveL";   MoveR   FastTaskType = "MoveR"
  )
  type FastTask struct { Type FastTaskType; RelPath string; OldRelPath string; Size int64 }
  type FastResult struct { Tasks []FastTask; NewState map[string]db.JobFileState }
  func FastCompare(mode FastMode, left, right, prev map[string]db.JobFileState) FastResult
  ```
- Consumes: `db.JobFileState` (from Task 4). `prev` is the previous two-way state (empty for mirror/update).
- This is a direct port of `file_sync_util/internal/sync/compare.go`'s `Compare`, with `FileState{Size,ModTime}` replaced by `db.JobFileState{Size,Mtime}` (mtime as unix int64). Comparisons `lState.ModTime.After(rState.ModTime)` become `lState.Mtime > rState.Mtime`; equality becomes `==`.

- [ ] **Step 1: Write the failing tests (port the source's cases)**

`internal/reconcile/fast_test.go` — port every case from `file_sync_util/internal/sync/compare_test.go`, translating `FileState{Size, ModTime: time.Unix(n,0)}` to `db.JobFileState{Size, Mtime: n}`. Include at minimum:
```go
func TestFastMirrorCopiesNewAndDeletesMissing(t *testing.T) {
	left := map[string]db.JobFileState{"a": {Path: "a", Size: 1, Mtime: 10}}
	right := map[string]db.JobFileState{"b": {Path: "b", Size: 1, Mtime: 10}}
	res := FastCompare(FastMirror, left, right, nil)
	// expect CopyL2R a, DeleteR b
	assertHasTask(t, res, FastTask{Type: CopyL2R, RelPath: "a", Size: 1})
	assertHasTask(t, res, FastTask{Type: DeleteR, RelPath: "b"})
}

func TestFastTwoWayDetectsMove(t *testing.T) {
	prev := map[string]db.JobFileState{"old": {Path: "old", Size: 3, Mtime: 5}}
	left := map[string]db.JobFileState{"new": {Path: "new", Size: 3, Mtime: 5}}
	right := map[string]db.JobFileState{"old": {Path: "old", Size: 3, Mtime: 5}}
	res := FastCompare(FastTwoWay, left, right, prev)
	assertHasTask(t, res, FastTask{Type: MoveR, RelPath: "new", OldRelPath: "old"})
}
```
Add a small `assertHasTask(t, res, want)` helper that scans `res.Tasks` ignoring order.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/reconcile/ -run Fast -v`
Expected: FAIL (FastCompare undefined).

- [ ] **Step 3: Implement `fast.go`**

Port `compare.go` verbatim into `internal/reconcile/fast.go`, applying the type swap described in Interfaces. Keep the three branches (mirror/update/two-way), the `signature{Size,Mtime}` move-detection maps, and the `processedMoves` guard exactly as in the source (`compare.go:22-141`).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/reconcile/ -v`
Expected: PASS (fast + existing robust engine tests).

- [ ] **Step 5: Commit**

```bash
git add internal/reconcile/fast.go internal/reconcile/fast_test.go
git commit -m "feat(reconcile): port fast size+mtime reconciler with move detection"
```

---

## Task 6: Fast-mode executor (atomic writes + moves + pterm progress)

**Files:**
- Create: `internal/execute/fast_exec.go`
- Test: `internal/execute/fast_exec_test.go`

**Interfaces:**
- Produces:
  ```go
  // RunFastTasks executes fast tasks against the two roots using atomic writes.
  // progress may be nil (non-interactive). Returns the first error encountered.
  func RunFastTasks(ctx context.Context, workers int, left, right string, tasks []reconcile.FastTask, progress ProgressSink) error
  // ProgressSink is satisfied by a pterm-backed sink in the cli layer and by a nop in tests.
  type ProgressSink interface { AddBytes(n int64); Done() }
  ```
- Consumes: `reconcile.FastTask` (Task 5), `fs.AtomicCopyFile` (existing).
- Rationale: reuse drivesync's atomic copy for safety instead of file_sync_util's plain `copyFile`. Move = `os.Rename` with cross-device copy+remove fallback (port `moveFile` from `file_sync_util/internal/sync/executor.go:115-128`, but write via `fs.AtomicCopyFile`). Deletes use `os.RemoveAll`. pterm wiring lives in the cli layer; this package stays UI-free behind `ProgressSink`.

- [ ] **Step 1: Write the failing test**

`internal/execute/fast_exec_test.go`:
```go
func TestRunFastTasksCopyAndDelete(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(right, "stale.txt"), []byte("x"), 0o644)
	tasks := []reconcile.FastTask{
		{Type: reconcile.CopyL2R, RelPath: "a.txt", Size: 2},
		{Type: reconcile.DeleteR, RelPath: "stale.txt"},
	}
	if err := RunFastTasks(context.Background(), 2, left, right, tasks, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(right, "a.txt")); string(b) != "hi" {
		t.Error("a.txt not copied to right")
	}
	if _, err := os.Stat(filepath.Join(right, "stale.txt")); !os.IsNotExist(err) {
		t.Error("stale.txt not deleted")
	}
}

func TestRunFastTasksMove(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(right, "old"), 0o755)
	os.WriteFile(filepath.Join(right, "old", "f.txt"), []byte("z"), 0o644)
	tasks := []reconcile.FastTask{{Type: reconcile.MoveR, RelPath: "new/f.txt", OldRelPath: "old/f.txt"}}
	if err := RunFastTasks(context.Background(), 1, left, right, tasks, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(right, "new", "f.txt")); string(b) != "z" {
		t.Error("file not moved to new path")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/execute/ -run Fast -v`
Expected: FAIL (RunFastTasks undefined).

- [ ] **Step 3: Implement `fast_exec.go`**

Implement a worker pool mirroring `worker_pool.go`'s feed/drain structure. For each task:
- `CopyL2R`: `fs.AtomicCopyFile(filepath.Join(left, rel), filepath.Join(right, rel))`, then `progress.AddBytes(task.Size)` if non-nil.
- `CopyR2L`: reverse.
- `DeleteL`/`DeleteR`: `os.RemoveAll(join(root, rel))`.
- `MoveL`/`MoveR`: `os.Rename(join(root, old), join(root, rel))`; on error fall back to `fs.AtomicCopyFile` + `os.RemoveAll(old)`. `os.MkdirAll(filepath.Dir(dst), 0o755)` first.
Collect the first error via a buffered channel (as `worker_pool.go` does), call `progress.Done()` at the end.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/execute/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/execute/fast_exec.go internal/execute/fast_exec_test.go
git commit -m "feat(execute): fast-mode executor with atomic writes and moves"
```

---

## Task 7: `syncd sync` command (ad-hoc orchestration + batch config)

**Files:**
- Create: `internal/cli/config.go`
- Create: `internal/cli/sync_cmd.go`
- Create: `internal/app/sync_once.go`
- Test: `internal/app/sync_once_test.go`

**Interfaces:**
- Produces:
  ```go
  // app.SyncOnce runs a single fast job (mirror/update/two-way). If useHash is
  // true it instead runs the robust three-way engine for this one job.
  type SyncOptions struct {
      Name, Left, Right string
      Mode reconcile.FastMode
      UseHash bool
      Assume bool // --yes: skip interactive confirm
      Workers int
      DBPath string
      Confirm func(summary string) bool // nil => auto-yes; cli passes a pterm confirm
      Progress execute.ProgressSink     // nil => no progress bar
  }
  func SyncOnce(ctx context.Context, opts SyncOptions) error
  ```
- Consumes: `fs.ScanPair`+`HashNever` (fast) or `HashAlways` (--hash), `reconcile.FastCompare`, `execute.RunFastTasks`, `db.JobStateRepo`, and for `--hash` the robust `reconcile.BuildPlan`+`execute.RunActions`.
- Config: `~/.syncd.yaml` with `jobs: [{name, left, right, mode}]` (port `Config`/`SyncJob` structs from `file_sync_util/cmd/root.go:13-23`, tag `mapstructure`).

- [ ] **Step 1: Write the failing test (fast two-way end-to-end)**

`internal/app/sync_once_test.go`:
```go
func TestSyncOnceMirrorEndToEnd(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644)
	err := SyncOnce(context.Background(), SyncOptions{
		Name: "t", Left: left, Right: right, Mode: reconcile.FastMirror,
		Assume: true, Workers: 2, DBPath: filepath.Join(t.TempDir(), "m.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(right, "a.txt")); string(b) != "hi" {
		t.Error("mirror did not copy file")
	}
}

func TestSyncOnceTwoWayPersistsState(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	dbp := filepath.Join(t.TempDir(), "m.db")
	os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644)
	opts := SyncOptions{Name: "j", Left: left, Right: right, Mode: reconcile.FastTwoWay, Assume: true, Workers: 1, DBPath: dbp}
	if err := SyncOnce(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	// Delete on left; second run should propagate delete to right (state proves prior sync).
	os.Remove(filepath.Join(left, "a.txt"))
	if err := SyncOnce(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(right, "a.txt")); !os.IsNotExist(err) {
		t.Error("two-way delete not propagated using persisted state")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run SyncOnce -v`
Expected: FAIL (SyncOnce undefined).

- [ ] **Step 3: Implement `app/sync_once.go`**

Orchestrate:
1. Open SQLite (`db.Open(opts.DBPath)`).
2. Load ignore matcher from `opts.Left`.
3. If `opts.UseHash`: `fs.ScanPair(ctx, left, right, manifest, fs.HashAlways, nil)` then robust `reconcile.BuildPlan` + `execute.RunActions` + manifest update (reuse the body of `app.Run`, factored into a shared helper).
4. Else (fast): scan both sides with `fs.HashNever` (via `ScanPair` or two `ScanTree` calls), convert `fs.FileEntry` → `db.JobFileState`, load prev state via `JobStateRepo.Load(opts.Name)` for two-way (nil otherwise), `reconcile.FastCompare`, build a summary string, call `opts.Confirm(summary)` unless `opts.Assume`, `execute.RunFastTasks`, then `JobStateRepo.Replace(opts.Name, res.NewState)` for two-way.

- [ ] **Step 4: Implement the cobra command + config**

`internal/cli/config.go`: viper loader for `~/.syncd.yaml` (port `initConfig`, `Config`, `SyncJob`).
`internal/cli/sync_cmd.go`: register a `sync` subcommand with flags `--mode --left --right --job --config --hash --yes`; when `--left`+`--right` given, run one job; else load jobs from config (all, or `--job NAME`). Build a pterm `Confirm` closure and a pterm-backed `execute.ProgressSink`. Call `app.SyncOnce` per job. `init()` adds it to `rootCmd`.

- [ ] **Step 5: Run tests + manual smoke**

Run: `go test ./internal/app/ ./internal/cli/ -v`
Then: `go build -o syncd ./cmd/syncd && ./syncd sync --mode mirror --left /tmp/s --right /tmp/d --yes` (create `/tmp/s` with a file first).
Expected: tests PASS; smoke copies the file.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/config.go internal/cli/sync_cmd.go internal/app/sync_once.go internal/app/sync_once_test.go
git commit -m "feat(cli): syncd sync command (fast ad-hoc + --hash bridge + batch config)"
```

---

## Task 8: `syncd init` command

**Files:**
- Create: `internal/cli/init_cmd.go`
- Modify: `internal/app/run.go` (export `RunInit` if currently unexported `runInit`)

**Interfaces:**
- Consumes: existing `runInit` logic (`app/run.go:156-174`) — export as `app.RunInit(localRoot, remoteRoot string, logger *runtime.Logger) error`.

- [ ] **Step 1: Export RunInit**

Rename `runInit(cfg cli.Config, ...)` to `RunInit(localRoot, remoteRoot string, logger *runtime.Logger) error`, taking explicit roots instead of the old `cli.Config` (which is being replaced by cobra). Update its one caller.

- [ ] **Step 2: Implement the command**

`internal/cli/init_cmd.go`: cobra `init` subcommand with `--local`/`--remote`, resolves abs paths, constructs a `runtime.Logger` (non-silent), calls `app.RunInit`. Register in `init()`.

- [ ] **Step 3: Verify**

Run: `go build -o syncd ./cmd/syncd && ./syncd init --local /tmp/il --remote /tmp/ir`
Expected: creates both dirs and `/tmp/ir/.synclock`; second run is a no-op.

- [ ] **Step 4: Commit**

```bash
git add internal/cli/init_cmd.go internal/app/run.go
git commit -m "feat(cli): syncd init command"
```

---

## Task 9: Extract a reusable robust one-shot run (`app.RunRobustOnce`)

**Files:**
- Modify: `internal/app/run.go`
- Create: `internal/app/run_test.go` (if not already covering it)

**Interfaces:**
- Produces:
  ```go
  type RobustParams struct {
      LocalRoot, RemoteRoot string
      Workers int
      Repo *db.ManifestRepo
      DB *sql.DB
      Logger *runtime.Logger
      Policy fs.HashPolicy    // HashIfChanged for polls, HashAlways for deep verify
      OnlyPaths map[string]struct{} // nil => whole tree; non-nil => restrict scan+plan to these rel paths
  }
  // RunRobustOnce performs one scan→plan→execute→manifest cycle and returns the executed plan + failure count.
  func RunRobustOnce(ctx context.Context, p RobustParams) (reconcile.Plan, int, error)
  ```
- Rationale: the watch loop (Task 12) and `sync --hash` (Task 7) both need drivesync's core cycle without the CLI/flag/lock scaffolding. Factor the middle of `app.Run` (scan → BuildPlan → RunActions → manifest update → empty-dir handling) into `RunRobustOnce`. `app.Run` becomes: resolve paths, logger, lock, canary, open db, then call `RunRobustOnce` with `Policy=HashIfChanged, OnlyPaths=nil`.
- `OnlyPaths` support: when non-nil, after `ScanPair`, filter `local`/`remote`/`manifest` maps to keys in `OnlyPaths` before `BuildPlan` (a changed-path incremental cycle).

- [ ] **Step 1: Write the failing test**

`internal/app/run_test.go`:
```go
func TestRunRobustOnceUploadsNewLocalFile(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(local, "a.txt"), []byte("hi"), 0o644)
	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	_, failures, err := RunRobustOnce(context.Background(), RobustParams{
		LocalRoot: local, RemoteRoot: remote, Workers: 2,
		Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger, Policy: fs.HashIfChanged,
	})
	if err != nil || failures != 0 {
		t.Fatalf("err=%v failures=%d", err, failures)
	}
	if b, _ := os.ReadFile(filepath.Join(remote, "a.txt")); string(b) != "hi" {
		t.Error("file not uploaded to remote")
	}
}

func TestRunRobustOnceOnlyPathsRestrictsScope(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(local, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(local, "b.txt"), []byte("b"), 0o644)
	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	_, _, err := RunRobustOnce(context.Background(), RobustParams{
		LocalRoot: local, RemoteRoot: remote, Workers: 1,
		Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger, Policy: fs.HashIfChanged,
		OnlyPaths: map[string]struct{}{"a.txt": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(remote, "a.txt")); err != nil {
		t.Error("a.txt should have synced")
	}
	if _, err := os.Stat(filepath.Join(remote, "b.txt")); !os.IsNotExist(err) {
		t.Error("b.txt was out of scope and must not sync")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run RunRobustOnce -v`
Expected: FAIL (RunRobustOnce undefined).

- [ ] **Step 3: Implement by factoring `app.Run`**

Move lines `app/run.go:60-139` (scan → plan → exec → manifest updates → empty-dir handling) into `RunRobustOnce`, parameterized by `RobustParams`. Add the `OnlyPaths` filter right after `ScanPair`. Have `Run` build a `RobustParams{Policy: fs.HashIfChanged}` and delegate. Return `(plan, failures, runErr)`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/app/ -v`
Expected: PASS (new tests + existing `sync_loop_test.go`/behavior).

- [ ] **Step 5: Commit**

```bash
git add internal/app/run.go internal/app/run_test.go
git commit -m "refactor(app): extract RunRobustOnce with scope + policy params"
```

---

## Task 10: fsnotify watcher with debounce/coalesce

**Files:**
- Create: `internal/watch/watcher.go`
- Test: `internal/watch/watcher_test.go`

**Interfaces:**
- Produces:
  ```go
  // Watcher recursively watches root and emits coalesced batches of changed,
  // root-relative, NFC-normalized paths. Events within `debounce` of each other
  // are merged into one batch. matcher-ignored paths are dropped.
  type Watcher struct { /* ... */ }
  func NewWatcher(root string, matcher *fs.IgnoreMatcher, debounce time.Duration) (*Watcher, error)
  func (w *Watcher) Batches() <-chan map[string]struct{}
  func (w *Watcher) Close() error
  ```
- Consumes: `github.com/fsnotify/fsnotify`, `fs.IgnoreMatcher`, `fs.NormalizeRelativePath`.
- Note: fsnotify is non-recursive; the watcher must add every existing subdir and add newly-created dirs on the fly.

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/fsnotify/fsnotify@latest && go mod tidy
```

- [ ] **Step 2: Write the failing test**

`internal/watch/watcher_test.go`:
```go
func TestWatcherCoalescesEventsIntoBatch(t *testing.T) {
	root := t.TempDir()
	m, _ := fs.LoadIgnoreMatcher(root)
	w, err := NewWatcher(root, m, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	os.WriteFile(filepath.Join(root, "a.txt"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("2"), 0o644)

	select {
	case batch := <-w.Batches():
		if _, ok := batch["a.txt"]; !ok {
			t.Error("missing a.txt in batch")
		}
		if _, ok := batch["b.txt"]; !ok {
			t.Error("missing b.txt in batch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no batch emitted")
	}
}

func TestWatcherDropsIgnoredPaths(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".syncdignore"), []byte("*.tmp\n"), 0o644)
	m, _ := fs.LoadIgnoreMatcher(root)
	w, _ := NewWatcher(root, m, 80*time.Millisecond)
	defer w.Close()
	os.WriteFile(filepath.Join(root, "x.tmp"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "keep.txt"), []byte("k"), 0o644)
	batch := <-w.Batches()
	if _, ok := batch["x.tmp"]; ok {
		t.Error("ignored *.tmp leaked into batch")
	}
	if _, ok := batch["keep.txt"]; !ok {
		t.Error("keep.txt missing")
	}
}
```

- [ ] **Step 3: Implement `watcher.go`**

- On `NewWatcher`: create an `fsnotify.Watcher`, walk `root` adding every directory. Start a goroutine that:
  - reads `w.fsw.Events`; for each, compute `rel` via `fs.NormalizeRelativePath(root, ev.Name)`; skip if `matcher.Match(rel, isDir)`.
  - if a new directory is created, `w.fsw.Add` it (so nested files are seen).
  - accumulate rel paths into a pending `map[string]struct{}`; (re)arm a `time.Timer` of `debounce`; on timer fire, send the pending map to `w.batches` and reset it.
- `Batches()` returns the receive-only channel; `Close()` stops the timer, closes the fsnotify watcher, and closes the batches channel.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/watch/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/watch/watcher.go internal/watch/watcher_test.go go.mod go.sum
git commit -m "feat(watch): fsnotify recursive watcher with debounce/coalesce"
```

---

## Task 11: Scoped incremental reconcile of a changed-path batch

> **DESIGN DECISION (2026-07-29, user-approved):** local-event cycles must NOT
> full-scan the trees. A local file change reconciles by stat/hashing ONLY the
> changed local paths and probing ONLY those paths on the remote — no full SMB
> tree walk per edit. This supersedes the original "RunRobustOnce + OnlyPaths"
> adapter (which still full-scanned). Full scans remain only for the remote poll
> and deep scan (Task 12), which call `app.RunRobustOnce`.

**Files:**
- Create: `internal/fs/scan_paths.go`
- Test: `internal/fs/scan_paths_test.go`
- Modify: `internal/app/sync_loop.go` (extract a small shared dedup helper; add `RunRobustScoped`)
- Create: `internal/watch/incremental.go`
- Test: `internal/watch/incremental_test.go`

**Interfaces:**
- Produces (fs):
  ```go
  // ScanPaths stat/hashes ONLY the given root-relative paths under root — it does
  // NOT walk the tree. For each rel path: Lstat root/rel; skip if missing (absent
  // side), a directory, or ignored by matcher; if validateSMBName and the basename
  // has an SMB-forbidden char, record it in Issues+Skipped; otherwise produce a
  // FileEntry with Hash per policy (HashIfChanged reuses the manifest cache exactly
  // as ScanTree does). Returns a ScanResult (Dirs is always empty here).
  func ScanPaths(root string, relPaths map[string]struct{}, manifest map[string]db.ManifestRow, policy HashPolicy, validateSMBName bool, matcher *IgnoreMatcher) (ScanResult, error)
  ```
- Produces (app):
  ```go
  // RunRobustScoped reconciles ONLY relPaths, scanning just those paths on both
  // sides via fs.ScanPaths (no tree walk). Same reconcile/execute/manifest-commit
  // machinery as RunRobustOnce, minus whole-tree empty-dir CREATION (new empty dirs
  // and recursively-deleted dirs are picked up by Task 12's deep scan). Emptied-dir
  // cleanup driven by this batch's deletes still runs.
  func RunRobustScoped(ctx context.Context, p RobustParams, relPaths map[string]struct{}) (reconcile.Plan, int, error)
  ```
- Produces (watch):
  ```go
  // ReconcilePaths runs one scoped cycle for a local-event batch (HashIfChanged).
  func ReconcilePaths(ctx context.Context, p app.RobustParams, paths map[string]struct{}) (int, error)
  ```
- Consumes: `reconcile.BuildPlan`, `execute.RunActions`, `db.ManifestRepo`, `fs.LoadIgnoreMatcher`, `runtime` — all existing.
- DRY: extract the results→(upserts, deletes, failures) dedup currently inline in `RunRobustOnce` into an unexported helper `collectManifestUpdates(results []execute.Result) ([]db.ManifestRow, []string, int)`, and have BOTH `RunRobustOnce` and `RunRobustScoped` call it. This keeps the gnarly dedup in one place; `RunRobustScoped` otherwise mirrors `RunRobustOnce`'s BuildPlan→RunActions→ApplyUpdates→cleanupEmptiedDirs flow but with `ScanPaths` inputs and no `createMissingEmptyDirs`. Do NOT change `RunRobustOnce`'s observable behavior when extracting the helper (Task 9's error-path fix must stay intact).

- [ ] **Step 1: Write the failing fs.ScanPaths test**

`internal/fs/scan_paths_test.go`:
```go
func TestScanPathsOnlyRequestedExistingFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("b"), 0o644)
	m, _ := LoadIgnoreMatcher(dir)
	// Request a.txt (exists), sub/b.txt (exists), gone.txt (missing), sub (a dir).
	req := map[string]struct{}{"a.txt": {}, "sub/b.txt": {}, "gone.txt": {}, "sub": {}}
	res, err := ScanPaths(dir, req, nil, HashAlways, false, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("want 2 files, got %d: %v", len(res.Files), res.Files)
	}
	if res.Files["a.txt"].Hash == "" || res.Files["sub/b.txt"].Hash == "" {
		t.Error("HashAlways should populate hashes for existing files")
	}
	if _, ok := res.Files["gone.txt"]; ok {
		t.Error("missing path must be absent from Files")
	}
	if _, ok := res.Files["sub"]; ok {
		t.Error("directory path must not be scanned as a file")
	}
}
```

- [ ] **Step 2: Run it — FAIL (ScanPaths undefined). Then implement `internal/fs/scan_paths.go`:**

```go
package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"syncd/internal/db"
)

func ScanPaths(root string, relPaths map[string]struct{}, manifest map[string]db.ManifestRow, policy HashPolicy, validateSMBName bool, matcher *IgnoreMatcher) (ScanResult, error) {
	result := ScanResult{
		Files:   make(map[string]FileEntry),
		Skipped: make(map[string]struct{}),
		Dirs:    make(map[string]struct{}),
	}
	scanStart := time.Now().Unix()
	for rel := range relPaths {
		if matcher.Match(rel, false) {
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue // absent on this side
			}
			return ScanResult{}, fmt.Errorf("lstat %s: %w", abs, err)
		}
		if info.IsDir() {
			continue // scoped incremental handles files; dirs via deep scan
		}
		if validateSMBName && hasSMBForbiddenChar(filepath.Base(abs)) {
			result.Issues = append(result.Issues, ScanIssue{Path: rel, Reason: "filename contains SMB-forbidden characters"})
			result.Skipped[rel] = struct{}{}
			continue
		}
		size := info.Size()
		mtime := info.ModTime().Unix()
		hash := ""
		switch policy {
		case HashNever:
		case HashAlways:
			hash, err = FileBLAKE3(abs)
			if err != nil {
				return ScanResult{}, fmt.Errorf("hash file %s: %w", abs, err)
			}
		default: // HashIfChanged
			if row, ok := manifest[rel]; ok && row.Size == size && row.Mtime == mtime && mtime < scanStart {
				hash = row.Hash
			} else {
				hash, err = FileBLAKE3(abs)
				if err != nil {
					return ScanResult{}, fmt.Errorf("hash file %s: %w", abs, err)
				}
			}
		}
		result.Files[rel] = FileEntry{Path: rel, AbsPath: abs, Size: size, Mtime: mtime, Hash: hash}
	}
	return result, nil
}
```

- [ ] **Step 3: Extract `collectManifestUpdates` from `RunRobustOnce`, then add `RunRobustScoped`**

In `internal/app/sync_loop.go`, move the inline results→(upserts, deletes, failures) dedup loop into:
```go
func collectManifestUpdates(results []execute.Result, logger *runtime.Logger) (upserts []db.ManifestRow, deletes []string, failures int) {
	seenUpserts := make(map[string]db.ManifestRow)
	seenDeletes := make(map[string]struct{})
	for _, res := range results {
		if res.Error != nil {
			failures++
			logger.Error("action failed", map[string]any{"action": res.Action.Type, "path": res.Action.Path, "error": res.Error.Error()})
			continue
		}
		for _, update := range res.Updates {
			if update.Delete {
				seenDeletes[update.Path] = struct{}{}
				delete(seenUpserts, update.Path)
				continue
			}
			seenUpserts[update.Path] = update.Row
			delete(seenDeletes, update.Path)
		}
	}
	for _, row := range seenUpserts {
		upserts = append(upserts, row)
	}
	for path := range seenDeletes {
		deletes = append(deletes, path)
	}
	sort.Slice(upserts, func(i, j int) bool { return upserts[i].Path < upserts[j].Path })
	sort.Strings(deletes)
	return upserts, deletes, failures
}
```
Have `RunRobustOnce` call it (behavior unchanged — same maps, same sorting). Then add:
```go
func RunRobustScoped(ctx context.Context, p RobustParams, relPaths map[string]struct{}) (reconcile.Plan, int, error) {
	matcher, err := fs.LoadIgnoreMatcher(p.LocalRoot)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	manifest, err := p.Repo.LoadAll(ctx)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	localScan, err := fs.ScanPaths(p.LocalRoot, relPaths, manifest, p.Policy, true, matcher)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	remoteScan, err := fs.ScanPaths(p.RemoteRoot, relPaths, manifest, p.Policy, false, matcher)
	if err != nil {
		return reconcile.Plan{}, 0, err
	}
	for _, issue := range localScan.Issues {
		p.Logger.Error("skipping local file", map[string]any{"path": issue.Path, "reason": issue.Reason})
	}
	scopedManifest := make(map[string]db.ManifestRow, len(relPaths))
	for rel := range relPaths {
		if row, ok := manifest[rel]; ok {
			scopedManifest[rel] = row
		}
	}
	skipped := copySkipped(localScan.Skipped, remoteScan.Skipped)
	plan := reconcile.BuildPlan(localScan.Files, remoteScan.Files, scopedManifest, skipped)
	execActions := make([]reconcile.Action, 0, len(plan.Actions))
	for _, a := range plan.Actions {
		if a.Type != reconcile.ActionNoOp {
			execActions = append(execActions, a)
		}
	}
	state := execute.SyncState{LocalRoot: p.LocalRoot, RemoteRoot: p.RemoteRoot, LocalFiles: localScan.Files, RemoteFiles: remoteScan.Files}
	results, runErr := execute.RunActions(ctx, p.Workers, state, execActions)
	upserts, deletes, failures := collectManifestUpdates(results, p.Logger)
	cleanupEmptiedDirs(p.LocalRoot, p.RemoteRoot, deletes)
	if len(upserts) > 0 || len(deletes) > 0 {
		if err := p.Repo.ApplyUpdates(ctx, upserts, deletes); err != nil {
			return plan, failures, err
		}
	}
	return plan, failures, runErr
}
```

- [ ] **Step 4: Write the failing watch test, then implement `internal/watch/incremental.go`**

`internal/watch/incremental_test.go`:
```go
func TestReconcilePathsSyncsOnlyGivenPaths(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(local, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(local, "b.txt"), []byte("b"), 0o644)
	dbh, _ := db.Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	logger, _ := runtime.NewLogger(true, filepath.Join(t.TempDir(), "l.log"))
	defer logger.Close()
	p := app.RobustParams{LocalRoot: local, RemoteRoot: remote, Workers: 1, Repo: db.NewManifestRepo(dbh), DB: dbh, Logger: logger}
	failures, err := ReconcilePaths(context.Background(), p, map[string]struct{}{"a.txt": {}})
	if err != nil || failures != 0 {
		t.Fatalf("err=%v failures=%d", err, failures)
	}
	if b, _ := os.ReadFile(filepath.Join(remote, "a.txt")); string(b) != "a" {
		t.Error("a.txt should have synced to remote")
	}
	if _, err := os.Stat(filepath.Join(remote, "b.txt")); !os.IsNotExist(err) {
		t.Error("b.txt was not in the batch and must not sync")
	}
}
```
`internal/watch/incremental.go`:
```go
package watch

import (
	"context"

	"syncd/internal/app"
	"syncd/internal/fs"
)

func ReconcilePaths(ctx context.Context, p app.RobustParams, paths map[string]struct{}) (int, error) {
	p.Policy = fs.HashIfChanged
	_, failures, err := app.RunRobustScoped(ctx, p, paths)
	return failures, err
}
```

- [ ] **Step 5: Run all tests**

Run: `go test ./internal/fs/ ./internal/app/ ./internal/watch/ -v` then `go test ./...`
Expected: PASS, including the unchanged `RunRobustOnce`/`app.Run` tests (the dedup extraction is behavior-preserving).

- [ ] **Step 6: Commit**

```bash
git add internal/fs/scan_paths.go internal/fs/scan_paths_test.go internal/app/sync_loop.go internal/watch/incremental.go internal/watch/incremental_test.go
git commit -m "feat(watch): scoped incremental reconcile (no full-tree walk per local event)"
```

---

## Task 12: Watch loop (local events + remote poll + deep scan) and `syncd watch`

**Files:**
- Create: `internal/watch/loop.go`
- Create: `internal/cli/watch_cmd.go`
- Create: `internal/app/watch_run.go`
- Test: `internal/watch/loop_test.go`

**Interfaces:**
- Produces:
  ```go
  type LoopConfig struct {
      RemotePoll time.Duration // full remote HashIfChanged cycle cadence (default 30s)
      DeepScan   time.Duration // full-tree HashIfChanged safety net (default 1h)
      Debounce   time.Duration // watcher batch window (default 500ms)
  }
  // Run blocks until ctx is cancelled. It serializes all reconcile work
  // (local batches, remote polls, deep scans) through a single mutex so cycles
  // never overlap. Each cycle re-checks the remote canary before touching it.
  func Run(ctx context.Context, p app.RobustParams, matcher *fs.IgnoreMatcher, cfg LoopConfig) error
  ```
- Consumes: `Watcher` (Task 10), `ReconcilePaths` (Task 11), `app.RunRobustOnce` (Task 9), `runtime.CheckRemoteCanary`.
- Behavior:
  - Start a `Watcher` on `p.LocalRoot`. On each batch → `ReconcilePaths(ctx, p, batch)`.
  - `time.Ticker(RemotePoll)` → full `RunRobustOnce(Policy=HashIfChanged, OnlyPaths=nil)` (catches other machines' remote writes).
  - `time.Ticker(DeepScan)` → same but a stronger safety-net pass (still `HashIfChanged`; the point is the full-tree scope even if fsnotify dropped local events).
  - A `sync.Mutex` guarantees only one cycle runs at a time; each cycle calls `runtime.CheckRemoteCanary(p.RemoteRoot)` first and logs+skips (does not exit) if the mount vanished, retrying next tick.
  - **Why the deep scan is load-bearing (scoped-scan design):** local-event cycles use the SCOPED `ReconcilePaths` (Task 11), which only reconciles the exact changed paths and never walks the tree. Two things scoped cycles therefore do NOT handle: (a) creation of empty directories, and (b) recursively-deleted local directories where fsnotify only surfaced the top dir (children never appear in a batch). The periodic `RunRobustOnce` deep scan (full-tree) is what reconciles those, so `DeepScan` must not be disabled in production. The remote poll (also full `RunRobustOnce`) is what catches other machines' remote writes, since a network mount can't be watched.
  - Batch paths from the `Watcher` are already NFC-normalized (Task 10), so they match the NFC-keyed manifest that `RunRobustScoped` consults — do not double-normalize.
  - The `watch` command wires a `runtime.Logger`; the fsnotify watcher currently discards its internal `Errors` (Task 10). If you thread the logger into the watcher to surface those, keep it optional so the watcher's existing tests (which construct it without a logger) still compile — otherwise leave as a noted follow-up.

- [ ] **Step 1: Write the failing test (local event triggers a sync)**

`internal/watch/loop_test.go`:
```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/watch/ -run Loop -v`
Expected: FAIL (Run undefined).

- [ ] **Step 3: Implement `loop.go`**

Wire the watcher + two tickers + mutex as described in Interfaces. Use a helper `guardedCycle(fn func() error)` that locks the mutex, runs `CheckRemoteCanary`, then `fn`, logging errors. Select over `ctx.Done()`, `watcher.Batches()`, `pollTicker.C`, `deepTicker.C`.

- [ ] **Step 4: Implement `app/watch_run.go` + `cli/watch_cmd.go`**

`app/watch_run.go`: `RunWatch(ctx, cfg)` — resolve paths, logger, acquire lockfile, initial `CheckRemoteCanary`, open db, build matcher, then `watch.Run(...)`. (Lock is held for the whole daemon lifetime — one watcher per local root.)
`cli/watch_cmd.go`: cobra `watch` subcommand with `--local --remote --remote-poll --deep-scan --silent --workers --state-dir` (+ db/log/lock overrides), defaults `--remote-poll=30s --deep-scan=1h`. Handle SIGINT/SIGTERM → cancel ctx for clean shutdown. Register in `init()`.

- [ ] **Step 5: Run tests + manual smoke**

Run: `go test ./internal/watch/ -v`
Then: `./syncd init --local /tmp/wl --remote /tmp/wr && ./syncd watch --local /tmp/wl --remote /tmp/wr --remote-poll 5s &` — create a file in `/tmp/wl`, confirm it appears in `/tmp/wr` within ~1s; `kill %1`.
Expected: tests PASS; file propagates.

- [ ] **Step 6: Commit**

```bash
git add internal/watch/loop.go internal/watch/loop_test.go internal/cli/watch_cmd.go internal/app/watch_run.go
git commit -m "feat(watch): event-driven loop with remote poll + deep scan; syncd watch command"
```

---

## Task 13: Deploy units, README, retire file_sync_util

**Files:**
- Rename: `deploy/systemd/drivesync.service` → `syncd.service`; `drivesync.timer` → `syncd.timer`; `deploy/launchd/com.user.drivesync.plist` → `com.user.syncd.plist`
- Modify: `internal/runtime/paths.go` (state dir `drivesync` → `syncd`)
- Rewrite: `README.md`
- Delete: `/Users/william/remote/dev/tools/macOS/file_sync_util`

**Interfaces:** none (packaging + docs).

- [ ] **Step 1: Update the state dir default**

In `internal/runtime/paths.go`, change the default state dir path segment from `drivesync` to `syncd` (→ `~/.local/state/syncd/`). Update any test asserting the old path.

- [ ] **Step 2: Rename and rewrite deploy units**

Rename the three files. Inside each, replace `drivesync` with `syncd` in `Description`, `ExecStart` binary name/path, and plist `Label`/program args. The systemd unit now runs `syncd watch ...` (long-lived `Type=simple`, `Restart=on-failure`) instead of the old timer-driven one-shot; keep `syncd.timer` only if you also want a periodic `syncd sync --hash` verify (document it as optional). The launchd plist uses `KeepAlive=true` for the watch daemon.

- [ ] **Step 3: Rewrite README**

Document the three subcommands, `~/.syncd.yaml`, `.syncdignore`, the hash policies, the watch tuning flags (`--remote-poll`, `--deep-scan`), cross-compile commands (`GOOS=darwin/linux ... ./cmd/syncd`), and the deploy units. Fold in the still-relevant conflict-strategy and directory-sync sections from the drivesync README.

- [ ] **Step 4: Retire file_sync_util**

Confirm nothing else references it, then remove `macOS/file_sync_util`. (Its behavior now lives in `syncd sync`.)

- [ ] **Step 5: Final verification**

Run: `go build ./... && go vet ./... && go test ./...`
Then cross-compile check:
```bash
GOOS=darwin GOARCH=arm64 go build -o /dev/null ./cmd/syncd
GOOS=linux  GOARCH=amd64 go build -o /dev/null ./cmd/syncd
```
Expected: all green on both platforms.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "chore: syncd deploy units, README, retire file_sync_util"
```

---

## Version control note

There is currently no git repository in `/Users/william/remote/dev/tools`. Before Task 1's first commit, initialize one:
```bash
cd /Users/william/remote/dev/tools && git init
```
Add a `.gitignore` for the `syncd` binary and `dist/`. If you'd rather not use git, drop the commit steps — but per-task commits are strongly recommended so each reviewer gate has a clean boundary.

## Self-review notes (author)

- **Spec coverage:** CLI (T7/T8/T12), hash-policy scanner (T3), two reconcilers (T5 fast, existing robust), shared ignore/normalize/executor (T2/T6), single SQLite store + bbolt retired (T4), watch engine fsnotify+poll+deep-scan (T10/T11/T12), robustness kept via RunRobustOnce reuse (T9), cross-platform + deploy + retire (T13). All spec sections map to a task.
- **Naming:** `syncd`, `~/.syncd.yaml`, `.syncdignore`, canary stays `.synclock` — consistent across T1/T2/T13.
- **Type consistency:** `db.JobFileState{Path,Size,Mtime}` used identically in T4/T5/T6/T7; `fs.HashPolicy` values in T3/T9/T11/T12; `app.RobustParams` fields in T9/T11/T12; `reconcile.FastTask` in T5/T6.
