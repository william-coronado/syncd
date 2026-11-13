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
directory-recursion. (As of 2026-11-13 the scoped cycle expands directory
paths via `fs.ScanSubtree`, which shares `ScanTree`'s walk and so does
populate `SkippedDirs` for an unreadable directory inside the subtree;
`ScanPaths` itself is unchanged.)

Items #15, #16, #18, #19 and #21 from the second pass (debounce max-latency
cap, `RunActions` ctx error on a completed batch, `cobra.CheckErr` in
`initSyncConfig`, the unbounded conflict-name loop, `Logger.Close` locking)
were resolved on 2026-11-13 alongside the third review pass's fixes - see
the git history for that date.

Items #11-#23 below are from a second full-codebase review pass (2026-07-31,
Codex + Gemini 3.6, consolidated together). #1 and #2 from that pass (the
`watch` command's missing root-overlap check, and `FastCompare` emitting
move tasks that fail when the "from" side no longer has the file) were real
correctness bugs and were fixed directly rather than deferred — see git
history around that date. What follows are the remaining items, judged safe
to defer.

### 11. `runConflict` leaves an orphaned local conflict file when the final remote-side upload fails

`internal/execute/conflict.go` — the local canonical is renamed aside, the
remote version is downloaded into the canonical slot, and *that* download
failure already rolls back (see the comment at the call site — a prior review
fix). But the final step, uploading the preserved local copy to
`remoteConflictPath`, has no equivalent rollback: on failure this returns an
error with no `Updates`, leaving `localConflictPath` (the preserved local
original) present but with no manifest row recording it as a conflict copy.
The next scan reads it as a brand-new local file and uploads it as a regular
action — the outcome is eventually correct, just via an unintended path
rather than the deliberate conflict-copy path. Deferred because it
self-heals and the eventual on-disk state is the same either way; revisit if
a user ever reports a `_conflict_` marker file "reappearing" as a plain
upload.

### 12. Watcher drops the in-flight `pending` batch on `Close()`

`internal/watch/watcher.go` — `Close()` makes `run()` take the `case
<-w.done` branch and return immediately, before the debounce timer would
have flushed `pending`. Any paths accumulated since the last flush (up to one
`debounce` window, 500ms by default) are silently dropped on graceful
shutdown. Not a correctness bug: the next deep scan or remote poll always
catches whatever was missed. Worth fixing only if fast-and-clean shutdown
(SIGTERM immediately followed by expecting the change to be synced) ever
becomes a real workflow.

### 13. Unbuffered `w.batches` can block the watcher's event-reader goroutine

`internal/watch/watcher.go` + `internal/watch/loop.go` — `w.batches` is
unbuffered, and `watch.Run`'s single-consumer `select` holds a mutex across a
full reconcile cycle before it goes back to receiving. A long reconcile (many
seconds over SMB) means the next `w.batches <-` send blocks `run()`, so
fsnotify events stop being read for that whole window and the OS event queue
can overflow. The deep scan is the backstop for anything dropped this way.
Deferred: bounding this needs either a buffered channel with an explicit
overflow policy or a separate drain goroutine, and it's only been a
theoretical gap so far — revisit if inotify/kqueue overflow ever shows up in
the logs (see the existing fsnotify-errors-dropped gotcha above, which is the
same class of "we'd only find out from the logs" problem). As of 2026-11-13
fsnotify errors are logged and an `ErrEventOverflow` triggers an immediate
full rescan (`watch.Run`), so an overflow caused by this is now both visible
and self-correcting.

### 14. `AtomicCopyFile` does not fsync the destination directory after rename

`internal/fs/atomic_copy.go` — the temp file's data is fsync'd before
`os.Rename`, which is correct for file content, but the parent directory
entry itself is never fsync'd after the rename. On a crash immediately after
`AtomicCopyFile` returns, the rename may not be durable even though the
file's bytes are (the classic fsync-rename gap, notably on ext4). Deferred:
the real target here is SMB (see "Running/verifying against SMB" above),
where the server-side filesystem's durability semantics are out of syncd's
control anyway, and a crash in that exact window is a narrow target. Revisit
if syncd ever writes to a local ext4 destination where this matters more.

### 17. Negating a directory under an ignored tree doesn't re-include its contents

`internal/fs/ignore.go` — a `dirOnly` negation rule is skipped for file
paths, so `build/` + `!build/keep/` still ignores `build/keep/x.txt` (gitignore
would re-include it). Deferred: no current `.syncdignore` in the wild
requires this pattern, and matching full gitignore precedence semantics for
directory re-inclusion is nontrivially more code for a case nobody's hit yet.

### 20. `ScanPaths` / `FileBLAKE3` ignore context cancellation mid-read

`internal/fs/scan_paths.go` (no `ctx` parameter at all) and
`internal/fs/hash.go` (`io.Copy` inside `FileBLAKE3` takes no `ctx`) — hashing
a large file can't be interrupted by cancelling the sync cycle; a cancelled
scoped reconcile can stall for the full read time of a multi-GB file already
in flight. Bounded by the file's read speed, not unbounded, and reconciles
are scoped (fsnotify-triggered) or already run to completion by design
elsewhere — revisit if large-file cancellation latency ever becomes a
complaint.

### 23. `ResolvePaths`' default db/lock naming doesn't canonicalize roots

`internal/runtime/paths.go` — `defaultManifestDBName`/`defaultLockName`/`defaultLogName` hash
`localRoot + "\x00" + remoteRoot` as given, without `filepath.Clean`. The CLI
always passes `filepath.Abs`-resolved roots first, so this isn't reachable
through `syncd sync`/`syncd watch` today, but a direct library caller passing
a trailing slash or relative form would silently get a different db/lock/log
path for what is really the same root pair. `jobStateKey`
(`internal/cli/sync_cmd.go`) already does the `filepath.Abs` canonicalization
this needs — same fix, if the library contract ever gets a caller other than
the CLI.

The third review pass (2026-11-13) fixed its defects directly and then
resolved the six items it had recorded (#24-#29: `update` overwriting a
newer destination, two-way conflicts keeping no copy of the losing edit,
the scoped cycle not expanding directories, the shared default log, the
watcher's startup-fixed ignore rules, and directory operations missing
from the `--hash` summary), along with #22 from the second pass (summaries enumerating a
fixed list of types). See the git history for that date.
