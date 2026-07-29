# Known follow-ups

Deferred items from the full-codebase review of 2026-07-29 (merged as `edb6285`).
Each was reviewed and consciously judged **safe to defer** — none is a known
correctness bug. They are recorded here rather than in git-ignored scratch so
they survive a fresh clone.

Ordered roughly by value. Each entry says what it is, why it was deferred, and
what "done" looks like.

## Worth doing

### 1. Pin the `SkipSet` dir-equals-own-path invariant

`internal/reconcile/skip.go`, `internal/fs/scanner.go`

`dirPrefixes("secret")` returns nil (the path has no `/`), so a skipped
directory matching *its own* path relies on `ScanTree` always writing
`Skipped[relPath]` **before** the `IsDir()` check — see `scanner.go`'s per-entry
`walkErr` branch. That invariant is real and was verified, but it is unstated: an
edit that reorders those two lines would silently reopen the subtree-delete
data-loss bug that `SkipSet` exists to prevent.

**Done when:** a test asserts a skipped directory's own path is in both `Skipped`
and `SkippedDirs`, plus a comment at the `scanner.go` site saying the ordering is
load-bearing.

### 2. State (or remove) the unsorted-map invariant in two-way move detection

`internal/reconcile/fast.go`

The move pre-pass that builds `leftMissingSigs`/`rightMissingSigs` still iterates
`prev` as a bare map, while a comment a few blocks below says sorted iteration is
what makes `FastCompare` deterministic. It *is* order-invariant — collision
handling is symmetric, so any visit order converges on the same map — but that
reasoning is nowhere in the code.

**Done when:** either sort that loop too (costs nothing at these sizes) or add a
one-line comment naming the symmetry the determinism depends on.

### 3. `--dry-run` still touches the state directory

`internal/cli/sync_cmd.go`, `internal/app/sync_once.go`

A dry run acquires the per-job flock and opens/creates the state DB
(`jobs.db` or `hash-<id>.db`, plus `syncdStateDir()`'s `MkdirAll`). Harmless —
nothing under either sync root is modified, and the flock is arguably correct
since a dry run does read shared state — but inconsistent with the effort spent
making dry-run avoid creating the destination root.

**Done when:** decided deliberately either way and documented in the `--dry-run`
flag help. Skipping the flock is not obviously right; concurrent dry runs reading
the same job state are fine, but a dry run racing a real run is less clear.

## Test-coverage gaps

### 4. `ScanTree` root-`ReadDir`-failure abort shape

`internal/fs/scanner_test.go` — only the "root doesn't exist" (`Lstat` failure)
shape is pinned. The "root exists but its own `ReadDir` fails" shape (e.g.
`chmod 000` on the root) is handled by the same `path == root` check but is
untested. This is the crux abort/record distinction, so it deserves both shapes.
Guard with `if os.Geteuid() == 0 { t.Skip(...) }` — the idiom is already used in
this file.

### 5. Signature-collision test is one-directional

`internal/reconcile/fast_test.go` — covers only the left-missing
(`leftMissingSigs`) direction; the symmetric right-missing path is structurally
identical but untested. Same test also asserts `Tasks` but not `NewState`, even
though `NewState` becomes the next run's baseline.

### 6. Watch duration clamp doesn't assert cadence

`internal/watch/loop_test.go` — `TestRunClampsNonPositiveDurationsToDefaults`
only asserts `Run` returns without panicking or hanging, not that the 30s/1h
defaults were actually used. Asserting the real cadence needs clock injection,
which was judged not worth adding for this.

## Deliberate, revisit only if it bites

### 7. Dry-run preview scans sequentially

`internal/app/sync_once.go` — the dry-run preview scans left and right in
sequence rather than concurrently like `fs.ScanPair`. Intentional: the preview is
not the hot path. Revisit only if previewing a large tree over SMB feels slow.

### 8. Dry-run cannot distinguish "not created yet" from "mount vanished"

`internal/app/sync_once.go` — the preview's "missing root → treat as empty"
fallback can't tell a destination that was never created from an SMB mount that
disappeared. Preview-only and mutation-free, and real runs are unaffected
(`ScanTree` still aborts on a missing root, which is how a vanished mount is
detected). Would matter only if dry-run output were ever trusted for automation.

### 9. Scan-issue logging is one `Warn` per side per cycle, forever

`internal/app/sync_loop.go` — per-path detail goes to the log file at `Info`, and
one aggregated `Warn` per side per cycle goes to stderr. For a tree with a
permanently unreadable path, that is one stderr line per side every
`--remote-poll` (30s default), indefinitely. Bounded (O(1) per cycle, not O(N)),
but not silent. Full cross-cycle `(path, reason)` dedup was considered and not
built.

### 10. `ScanPaths` does not populate `SkippedDirs`

`internal/fs/scan_paths.go` — by design. `ScanPaths` only ever processes explicit
paths from fsnotify events and never discovers descendants, so a file made
unreachable by a newly-unreadable ancestor fails its own `Lstat` and gets an
exact `Skipped` entry directly. The unreadable-directory case is caught by the
next full `RunRobustOnce` cycle. Worth a comment if anyone ever gives `ScanPaths`
directory-recursion.
