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
directory-recursion.

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

### 22. Confirm-prompt summaries enumerate a fixed list of task types

`internal/app/sync_once.go` (`fastSummary`/`robustSummary`) — both iterate a
hardcoded slice of task types rather than the actual keys present. A new
action type added to either reconciler would execute without ever appearing
in the safety-gate summary the user approves before `--yes` is skipped.
Currently exhaustive (matches every type both reconcilers emit today); would
need to be revisited the next time a new task/action type is added, not
before.

### 23. `ResolvePaths`' default db/lock naming doesn't canonicalize roots

`internal/runtime/paths.go` — `defaultManifestDBName`/`defaultLockName` hash
`localRoot + "\x00" + remoteRoot` as given, without `filepath.Clean`. The CLI
always passes `filepath.Abs`-resolved roots first, so this isn't reachable
through `syncd sync`/`syncd watch` today, but a direct library caller passing
a trailing slash or relative form would silently get a different db/lock
path for what is really the same root pair. `jobStateKey`
(`internal/cli/sync_cmd.go`) already does the `filepath.Abs` canonicalization
this needs — same fix, if the library contract ever gets a caller other than
the CLI.

Items #24-#29 are from a third review pass (2026-11-13). That pass's real
defects were fixed directly (see git history for that date): the robust
executor overwriting/deleting destinations changed since the scan, the
remote-side hash cache never hitting for identical pairs with differing
mtimes, `sync --hash` executing a different plan than the one confirmed,
duplicate CLI error output with a usage dump, the scoped scan ignoring
directory-only ignore rules on ancestors, and fsnotify errors being dropped.
What follows was judged worth recording but not changing unilaterally.

### 24. Fast `update` mode overwrites a newer destination when sizes differ

`internal/reconcile/fast.go` — `update` copies L→R when
`lState.Size != rState.Size || lState.Mtime > rState.Mtime`, so a destination
file that is *newer* than the source but a different size is overwritten.
rsync's `--update` skips any destination newer than the source regardless
of size. This is a straight port of file_sync_util's behavior, so changing
it is a semantics decision for the owner, not a bug fix: decide whether
"update" means "never clobber a newer destination" and, if so, drop the
size clause for the `rState.Mtime > lState.Mtime` case.

### 25. Fast two-way "newer wins" conflicts keep no copy of the losing edit

`internal/reconcile/fast.go` — when both sides changed since the last
two-way run, the newer mtime wins and the other side's edit is overwritten
with no conflict copy (unlike the robust engine, which preserves both).
Documented file_sync_util behavior; the robust engine (`--hash`) is the
answer when that matters. Revisit if users mix fast two-way with real
concurrent editing.

### 26. Scoped cycle doesn't expand a deleted/renamed local directory

`internal/app/sync_loop.go` (`RunRobustScoped`) — fsnotify reports only the
top-level path of a recursive delete or a directory rename, and
`ScanPaths` skips directories, so the children wait for the next
`--remote-poll` (30s default). Expanding an absent-locally directory path to
its manifest children would propagate the delete immediately, but for a
rename the new side's files still need a subtree walk, so doing only the
delete half would briefly leave the remote with neither copy. Worth doing
both halves together (a scoped subtree `ScanTree`) if poll latency for
directory moves ever matters.

### 27. Concurrent `watch` instances share the default `sync.log`

`internal/runtime/paths.go` — the default db and lock are per root pair,
but the default log is a fixed `sync.log`, so two watch daemons for
different pairs interleave in one file, and one's in-place rotation renames
the file out from under the other (whose writes then land in `.old` until
it rotates itself). Fix by deriving the default log name from the root pair
like `defaultLockName`; deferred because it changes where operators look for
logs.

### 28. The watcher's ignore matcher is fixed at startup

`internal/app/watch_run.go` / `internal/watch/watcher.go` — scans reload
`.syncdignore` every cycle, but the fsnotify watcher keeps the matcher it
started with: a directory un-ignored mid-run is never added to the watch
set (its changes arrive only via polls), and a newly ignored one keeps
generating events (harmless: the scoped scan reloads the matcher and drops
them). Restart the daemon after editing `.syncdignore`, or rebuild the
watcher when the file changes.

### 29. The `--hash` confirm summary doesn't list directory operations

`internal/app/sync_once.go` (`robustSummary`) — the approved summary lists
file actions only; `reconcileDirectories` may also create empty
directories, or remove empty directories whose deletion it is propagating,
in the same cycle. Only ever empty directories (`os.Remove` is
non-recursive), so no file data is at risk, but the prompt is not a
complete list of what will change.
