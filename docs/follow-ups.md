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
