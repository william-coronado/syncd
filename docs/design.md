# syncd — Unified Directory Sync (design)

**Date:** 2026-07-29
**Status:** Approved design, pending implementation plan

## Summary

Merge two existing Go tools into one cross-platform binary named **`syncd`**:

- **`file_sync_util`** (macOS-only): fast, on-demand ad-hoc directory sync. Fast
  because it decides what to sync using **size + modification time only** — it
  never reads file contents.
- **`drivesync`** (macOS + Ubuntu): always-on continuous sync against a mounted
  SMB share, similar to OneDrive. Robust because it decides what to sync using a
  **BLAKE3 content hash** of every file plus a three-way SQLite manifest —
  correct over an unreliable SMB link, but slower.

`syncd` provides both behaviors from one codebase and makes the always-on SMB
path faster via event-driven local watching.

## Goals

1. **Unify** the two tools into one binary with two behaviors: a fast one-shot
   `sync` and a continuous `watch`.
2. **Speed up the always-on SMB path** using filesystem watching for the local
   side plus a smart periodic remote poll, instead of full rescans every cycle.
3. Retire duplicated code (two scanners, two executors, two state stores).

## Non-goals

- A "fast continuous" (mtime-only always-on) mode — unsafe for multi-writer SMB.
- Move detection in the robust three-way engine (future work; the fast engine
  keeps it).
- Rewriting the two reconciliation algorithms from scratch — both are proven.

## Confirmed context

- The always-on mode targets an **SMB / network mount**, where hashing, atomic
  writes, and the canary genuinely earn their place.
- The always-on topology is **multi-writer** (several machines sync through the
  same share, true OneDrive-style), so the remote can change without this
  machine acting — the watcher must poll the remote periodically.
- Foundation: build on **drivesync** (already cross-platform, SQLite manifest,
  atomic writes, conflict handling, ignore rules, worker pool, deploy configs,
  good tests). Fold file_sync_util's strengths into it.

## Architecture

### CLI (one binary, cobra subcommands)

Adopt `spf13/cobra` + `spf13/viper` (from file_sync_util), replacing drivesync's
hand-rolled flag parser, so subcommands and config live in one place.

```
syncd sync  <left> <right> [--mode mirror|update|two-way] [--hash] [--yes] [--job NAME] [--config FILE]
syncd watch --local PATH --remote PATH [--remote-poll 30s] [--deep-scan 1h] [--silent]
syncd init  --local PATH --remote PATH
```

- **`sync`** — ad-hoc path (file_sync_util behavior): fast, interactive pterm
  summary + confirm, `mirror`/`update`/`two-way`, batch jobs from
  `~/.syncd.yaml`. Defaults to the `HashNever` policy. `--hash` opts a single
  run into the robust hash engine (seeding / deep verify). `--yes` skips the
  confirm for scripting.
- **`watch`** — always-on path (drivesync behavior, upgraded): continuous,
  unattended, three-way hash-based, SMB-robust, JSON `--silent` logging.
- **`init`** — drivesync's bootstrap (create roots + `.synclock` canary).

### Unified concurrent scanner with a hash policy

The two scanners collapse into one goroutine-concurrent walker producing
drivesync's existing `FileEntry{Path, Size, Mtime, Hash}`:

- **`HashNever`** — stat-only; `Hash` left empty; reconciler compares size+mtime.
  file_sync_util's speed, now with drivesync's ignore + NFC normalization.
- **`HashIfChanged`** — hash only when size+mtime differ from the manifest
  (drivesync's warm-path). Default for `watch`.
- **`HashAlways`** — force full hash (deep verify / `sync --hash`).

Keeps drivesync's extras (SMB-forbidden-name validation, `Dirs` set for
empty-directory create/cleanup), gated by side/mode. Uses the concurrent walk
for throughput.

### Two reconcilers, shared everything else

The comparison algorithms stay separate because they give different guarantees;
they are the *only* thing that stays separate:

- **Fast reconciler** (file_sync_util `Compare`): `mirror`/`update`/`two-way` +
  **move detection**, on size+mtime. Used by `sync`.
- **Robust reconciler** (drivesync `BuildPlan`): three-way hash reconciliation
  with conflict preservation. Used by `watch` and `sync --hash`.

Shared by both:

- The unified scanner above.
- The gitignore-style **ignore matcher** (`.drivesyncignore` → renamed
  `.syncdignore`; macOS junk folded into built-in defaults), replacing
  file_sync_util's doublestar excludes.
- **NFC path normalization** (macOS/Ubuntu interop).
- The **atomic-write executor + worker pool** (temp-file + rename).
- A **single SQLite state store**; bbolt retired. Ad-hoc two-way state moves into
  SQLite under a job namespace, `hash` column nullable for fast-mode jobs.

### The `watch` engine (SMB speedup)

New `internal/watch` package (multi-writer SMB confirmed):

- **Local: fsnotify** (inotify on Linux, kqueue on macOS) on the local root →
  debounce/coalesce events → **incremental reconcile of only the changed
  paths** (no full local rescan). Main win over today's full-tree walk per cycle.
- **Remote: periodic poll** (`--remote-poll`, default 30s) → stat-walk the SMB
  tree with `HashIfChanged`, reconcile what changed. Required because a network
  mount can't be watched and other machines write to it.
- **Safety-net deep scan** (`--deep-scan`, default 1h) → occasional full
  `HashIfChanged` pass to catch anything fsnotify dropped.
- All cycles serialized through the existing lockfile + a run mutex; canary
  checked before each remote touch.

### Robustness, cross-platform, testing

- **Keep** drivesync's lockfile, canary, atomic temp-file+rename writes, non-zero
  exit on failure, log rotation, JSON silent mode. **Keep** file_sync_util's
  pterm summary + interactive confirm for `sync`.
- **Cross-platform**: fsnotify + existing `deploy/launchd` (macOS) and
  `deploy/systemd` (Ubuntu) units, renamed to `syncd`.
- **Testing**: port both suites; add tests for the hash-policy scanner, the
  debounce/coalesce logic (fake clock + temp dirs), and incremental reconcile.
  SMB is simulated by a second local dir in integration tests.
- **Layout**: build in the `drivesync/` tree (the base), rename module/binary to
  `syncd`, fold `sync`/`watch`/`init` under it, then retire
  `macOS/file_sync_util`.

## Naming

- Binary and module: **`syncd`**.
- Config file: `~/.syncd.yaml`.
- Ignore file: `.syncdignore` (built-in defaults preserved).
- State dir: `~/.local/state/syncd/` (`manifest.db`, `sync.lock`, `sync.log`).
- Canary file stays `.synclock` (already deployed on remotes; renaming it would
  break existing shares — keep as-is).
- Deploy units renamed to `syncd.service` / `syncd.timer` /
  `com.user.syncd.plist`.

## Open items / decisions deferred to implementation

- Default `--remote-poll` and `--deep-scan` intervals (start 30s / 1h; tune).
- Exact SQLite schema change for job-namespaced fast-mode state.
- Whether `watch` reuses the timer-based launchd/systemd units or runs as a
  long-lived daemon (leaning long-lived daemon, since fsnotify needs a resident
  process; keep timer units as a fallback for `sync --hash` cron-style verifies).
