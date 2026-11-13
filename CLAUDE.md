# syncd

One cross-platform Go binary that merges two directory-sync tools:

- **`syncd sync`** — fast, on-demand ad-hoc sync (was `file_sync_util`). Decides
  what to copy from **size + mtime only** — never reads file contents, so it's
  fast. Modes: `mirror`, `update`, `two-way` (with move detection). Interactive
  confirm; batch jobs from `~/.syncd.yaml`.
- **`syncd watch`** — always-on continuous sync against a mounted **SMB** share
  (was `drivesync`), OneDrive-style. Content-**hash** (BLAKE3) three-way
  reconciliation with a SQLite manifest — correct over an unreliable link.
  Event-driven: fsnotify watches the local tree and reconciles only changed
  paths; a periodic remote poll catches other machines' writes; a deep scan is
  the safety net.
- **`syncd init`** — bootstrap local/remote roots + the remote `.synclock` canary.

The fast/slow split is fundamentally **when you hash**: `sync` never hashes
(fast, can't see content-only changes); `watch` hashes (robust over SMB). One
scanner exposes this as a policy knob.

Design rationale and the full build plan: [docs/design.md](docs/design.md),
[docs/plan.md](docs/plan.md).

## Commands

```bash
go build -o syncd ./cmd/syncd      # build the binary
go build ./... && go vet ./...     # compile + vet everything
go test ./...                      # full suite
go test ./... -race                # race detector (watch/executor are concurrent)
GOOS=darwin GOARCH=arm64 go build -o /dev/null ./cmd/syncd   # cross-compile check
GOOS=linux  GOARCH=amd64 go build -o /dev/null ./cmd/syncd
```

Run:
```bash
./syncd init  --local /path/to/local --remote /Volumes/share
./syncd sync  --left /path/to/local --right /Volumes/share --mode mirror   # add --yes to skip confirm
./syncd watch --local /path/to/local --remote /Volumes/share --remote-poll 30s --deep-scan 1h
```

**Always** run `go build ./... && go vet ./... && go test ./...` (and `-race`
for anything touching `internal/watch` or `internal/execute`) green before
committing. This project was built test-first; keep that.

## Architecture

Import graph is a clean DAG: `cmd/syncd → cli → {app, watch}`; those →
`{fs, db, reconcile, execute, runtime}`. `db` and `runtime` are leaves.

| Package | Responsibility |
|---|---|
| `cmd/syncd` | thin entrypoint → `cli.Execute()` |
| `internal/cli` | cobra commands (`root`/`sync`/`watch`/`init`), `~/.syncd.yaml` loader, pterm UI |
| `internal/app` | orchestration: `SyncOnce` (fast + `--hash`), `RunRobustOnce` (full-tree robust cycle), `RunRobustScoped` (scoped cycle), `RunWatch`, `RunInit` |
| `internal/watch` | fsnotify `Watcher` (debounce/coalesce), the watch `Run` loop, scoped `ReconcilePaths` |
| `internal/fs` | one scanner with a **hash policy** (`ScanTree`/`ScanPair`), scoped `ScanPaths`/`ScanSubtree`, `.syncdignore` matcher, NFC normalization, atomic copy, BLAKE3 hash |
| `internal/reconcile` | `fast.go` = size+mtime `FastCompare` (mirror/update/two-way + moves); `engine.go` = three-way hash `BuildPlan` |
| `internal/execute` | `worker_pool.go` = robust action executor; `fast_exec.go` = fast executor (`RunFastTasks` + `ProgressSink`) |
| `internal/db` | SQLite: `file_manifest` (hash/three-way) + `job_state` (fast two-way, keyed `(job,path)`) |
| `internal/runtime` | lockfile, remote canary, JSON logging + rotation, path resolution, `Config` |

**Two reconcilers, one of everything else.** The fast (size+mtime) and robust
(hash three-way) comparison algorithms are separate on purpose — different
guarantees. They share the scanner, ignore rules, normalization, executor, and
state store.

**Scanner hash policy** (`internal/fs/scanner.go`): `HashNever` (fast/ad-hoc),
`HashIfChanged` (hash only when size+mtime differ from manifest — watch/default),
`HashAlways` (deep verify / `--hash`).

**Watch loop** (`internal/watch/loop.go`): a single-consumer `select` serializes
all cycles (no overlap). Local fsnotify batches → **scoped** `RunRobustScoped`
(stat/hash only the changed paths — no full-tree walk, the SMB speedup; a
path that is a directory on either side is expanded to just that subtree, so
directory renames and recursive deletes propagate in one cycle). Editing
`.syncdignore` rebuilds the watcher with the new rules and rescans.
`--remote-poll` and `--deep-scan` tickers → full `RunRobustOnce`. Each cycle
re-checks the canary and skips (does not exit) if the mount vanished. Clean
shutdown on ctx cancel / SIGINT / SIGTERM. Batches are debounced but capped
at 10× the debounce window so sustained activity still flushes.

**Executor pre-checks** (`internal/execute/worker_pool.go`): every robust
upload/download/delete re-stats its destination and defers
(`ErrDestinationChanged`, no manifest update) unless it is exactly what the
scan saw. Keep this — it is what stops a slow SMB cycle from clobbering an
edit made mid-cycle.

**Manifest remote stat**: `file_manifest.remote_size/remote_mtime` (NULL =
same as local) key the remote side's `HashIfChanged` cache; scanners get the
remote view via `db.RemoteStatView`.

## Invariants & conventions (do not break)

- Module path `syncd`; imports `syncd/internal/...`. Go **1.26.4**. Must build
  darwin **and** linux.
- **Canary filename is `.synclock`** — a wire-protocol name already deployed on
  remote shares. Never rename it.
- Ignore file: `.syncdignore` (gitignore-style; lives in the **local** root).
  Batch config: `~/.syncd.yaml`. State dir: `~/.local/state/syncd/`
  (watch: `manifest-<pair>.db`, `sync-<pair>.lock`, `sync-<pair>.log`, named
  per root pair; `sync`: `jobs.db`/`hash-<id>.db`).
- **Two-way (fast) conflicts keep both edits**: newer mtime wins via
  `ConflictL2R`/`ConflictR2L`, which rename the loser to `_conflict_<ts>`
  first. `update` mode never overwrites a newer destination (rsync `-u`).
- **Data-loss guard**: `mirror`/`update` and `--hash` require the **source** to
  exist (`app.prepareRoots`); only the destination is auto-created. `two-way`
  may create both. Never let an auto-created empty source drive deletions
  (mirror would wipe the destination). There's a regression test for this —
  keep it.
- `RunRobustOnce`'s `app.Run`-era error-path behavior is gone with `app.Run`;
  the live error contract lives in `SyncOnce`/`RunRobust*`.
- Two import cycles were deliberately broken and must stay broken: CLI config
  lives in `runtime.Config` (not `cli`); `app.RunWatch(ctx, cfg, runLoop
  WatchLoopFunc)` takes the loop as an injected callback so `cli` wires
  `watch.Run` (keeps `RunWatch`'s lifecycle defers alive across the loop).

## Running/verifying against SMB

The real target is a Synology NAS over `smbfs` (e.g. `/Volumes/dev`). The link
is fast (~76 MB/s in testing). When verifying by hand, use an **isolated probe
directory** on the share and clean it up — never point `mirror` at a real tree
without checking the source path first (see the data-loss guard).

## Gotchas / known follow-ups

- **fsnotify errors** are forwarded on `Watcher.Errors()` (best-effort, small
  buffer) and logged by `watch.Run`; `fsnotify.ErrEventOverflow` triggers an
  immediate full `RunRobustOnce` rescan, since the kernel dropped events.
- **SMB xattr writes are rejected** on the test NAS (a mount permission quirk).
  syncd doesn't copy extended attributes, so it's unaffected — but plain `cp`
  will hit `Permission denied` on xattrs there.
- Deferred minor items (test-coverage gaps, unstated invariants worth pinning,
  deliberate tradeoffs) live in [docs/follow-ups.md](docs/follow-ups.md) — each
  with why it was deferred and what "done" looks like. Checked in on purpose:
  the previous list lived in git-ignored scratch and did not survive cleanup.
- The originals (`../macOS/file_sync_util`, `../drivesync`) were intentionally
  **kept**, not deleted — syncd supersedes both.

## Git

Fresh repo; the finished work is on **`main`**. A `feat/syncd` branch may exist
pointing at an earlier commit. No remote is configured. End commit messages with
`Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
