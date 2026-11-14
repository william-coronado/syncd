# syncd

`syncd` is a cross-platform Go CLI that synchronizes files between a local
directory and a remote directory (typically a mounted SMB/network share). It
is one tool with two complementary modes:

- **`syncd sync`** — a fast, ad-hoc or batch-scheduled sync: point it at two
  folders (or a config file of jobs) and it reconciles them once and exits.
- **`syncd watch`** — an always-on daemon: it reacts to local filesystem
  events almost instantly, polls the remote periodically to catch changes
  made by other machines, and runs a periodic full deep scan as a safety
  net.

Both modes share the same underlying pieces: ignore rules (`.syncdignore`),
path normalization, the atomic-write/conflict executor, and (for the robust
hash-based path) the same SQLite manifest and three-way reconciliation
engine. `syncd sync` additionally has its own fast, size+mtime-based
reconciler for day-to-day ad-hoc use, ported from the old `file_sync_util`
tool, whose functionality now lives entirely inside `syncd sync`.

It is designed for macOS and Ubuntu environments where direct SMB writes can
cause lock or write instability.

## Commands

### `syncd init`

Bootstraps a local/remote pair before the first real sync: creates `--local`
and `--remote` if they don't exist, and writes the remote canary file
(`.synclock`) if it isn't already there. Performs no reconciliation. Safe to
run again later — it's a no-op if everything is already in place.

```bash
syncd init --local /path/to/local --remote /path/to/mounted/smb
```

Run this once per local/remote pair before your first `syncd watch` or
`syncd sync --hash` run.

### `syncd sync` — fast ad-hoc / batch sync

Runs a fast, size+mtime-based sync job (`mirror`, `update`, or `two-way`),
either as a single one-off job or as a batch of named jobs from a config
file.

```bash
# One-off job
syncd sync --left /path/a --right /path/b --mode mirror

# Every job in ~/.syncd.yaml
syncd sync

# Just one named job from the config file
syncd sync --job photos
```

Flags:

- `--left`, `--right` — one-off job paths (must be given together; skips the
  config file entirely when set)
- `--mode` — `mirror` (default), `update`, or `two-way`
- `--job` — run only this named job from the config file
- `--config` — config file path (default `~/.syncd.yaml`)
- `--hash` — use the robust, hash-based engine (same three-way reconciler as
  `syncd watch`) instead of the fast size+mtime reconciler for this run
- `--yes` — skip the interactive confirmation prompt (needed for unattended/
  scripted runs)
- `--dry-run` — print the summary and exit without changing anything
- `--max-delete`, `--no-trash`, `--trash-days` — see "Delete safety" below

#### Batch config file (`~/.syncd.yaml`)

```yaml
jobs:
  - name: photos
    left: /Users/me/Pictures
    right: /Volumes/nas/Pictures
    mode: mirror
  - name: docs
    left: /Users/me/Documents
    right: /Volumes/nas/Documents
    mode: two-way
```

Running `syncd sync` with no `--left`/`--right` runs every job listed here
(or just the one named by `--job`).

#### Sync modes

- **`mirror`** — left is authoritative: right is made to match left exactly
  (copies left→right, deletes anything on right that isn't on left).
- **`update`** — left is authoritative but right is never deleted from: only
  copies left→right for new/changed/newer files.
- **`two-way`** — both sides can change between runs; persisted per-job state
  (keyed by job name) is used to detect which side changed, propagate that
  change to the other side, detect moves/renames, and apply "newer wins" if
  both sides changed the same file since the last run.

#### The `--hash` bridge

By default `syncd sync` uses the fast reconciler (size+mtime comparison,
no hashing — quick, but it can't detect a same-size/same-mtime edit or
reliably reconcile drift after external changes). Passing `--hash` runs the
same run through the robust, BLAKE3-hash-based three-way engine that
`syncd watch` uses instead, backed by the SQLite manifest rather than the
fast reconciler's own job-state table. Use it for an occasional, more
thorough verification pass, or when you don't trust size+mtime alone (e.g.
after an interrupted run, a restore, or clock skew between machines).

### `syncd watch` — always-on daemon

Runs `syncd` as a long-lived daemon combining three triggers, so that local
changes propagate almost instantly while remote-side and structural changes
are still eventually caught:

```bash
syncd watch --local /path/to/local --remote /path/to/mounted/smb
```

- **fsnotify watcher** — local filesystem events under `--local` are
  debounced/coalesced (500ms) and reconciled via a **scoped** reconcile:
  only the changed relative paths are scanned and planned, not the whole
  tree. This is what makes local edits propagate almost instantly even on a
  large tree.
- **`--remote-poll`** (default `30s`) — a full, whole-tree reconcile cycle
  (`HashIfChanged` policy) against both sides. This is the only way to catch
  writes made directly on the remote by another machine, since a network
  mount generally can't be watched by fsnotify.
- **`--deep-scan`** (default `1h`) — the same full, whole-tree reconcile,
  run periodically as a safety net. It is the *only* trigger that picks up
  newly created empty directories and directories that were deleted
  recursively on the local side — the fsnotify-driven scoped reconcile only
  sees the top-level path for those and can't walk into what's already
  gone. Do not disable this in production.

All three triggers are serialized so cycles never overlap, and each cycle
re-checks the remote canary file first; if the canary is missing (e.g. the
network mount vanished), that cycle is logged and skipped rather than
crashing the daemon — the next tick or event batch retries.

Other flags: `--silent` (structured JSON logs only, no stdout), `--workers`,
`--state-dir`/`--db-path`/`--log-path`/`--lock-path` overrides (see
"State directory" below).

- `--wait-for-remote` — if the remote root or its canary is missing at
  startup, wait for it (checking every 10s) instead of exiting. This is what
  lets the daemon be started at login, before the share is mounted;
  `syncd service install` always sets it.
- `--max-delete`, `--no-trash`, `--trash-days` — see "Delete safety" below.
- `--first-sync` — see "First sync" below.

#### First sync

The first time `watch` runs for a pair it has no history, so for a file that
exists on both sides with different content it cannot tell which side
changed. `--first-sync` decides what happens to those files, on that first
cycle only:

- `newer` (default) — the version with the later modification time takes
  the file's name on both sides; the other version is saved to the trash
  (see "Delete safety"), so nothing is lost and no extra files appear in
  either tree. Files whose times are equal fall back to `keep-both`, as does
  everything when `--no-trash` is set.
- `local` / `remote` — that side's version wins for every such file; the
  other version goes to the trash (or is overwritten with `--no-trash`).
- `keep-both` — the remote version takes the name and the local version is
  kept next to it as `name_conflict_<timestamp>.ext`, on both sides.

Files that exist on only one side are simply copied across, and identical
files are left alone, whatever the setting. Once the pair has history, a
file changed on both sides is always handled as a conflict (see "Conflict
strategy").

### `syncd service` — run `watch` in the background

```bash
syncd init --local ~/dev --remote /Volumes/dev        # once per pair
syncd service install --local ~/dev --remote /Volumes/dev
syncd service status
syncd service uninstall
```

`service install` runs `syncd watch` as a per-user background service that
starts at login and is restarted if it exits — a launchd agent on macOS
(`~/Library/LaunchAgents/com.syncd.watch.plist`), a systemd user unit on
Linux (`~/.config/systemd/user/syncd-watch.service`). No root is needed.

- It accepts every `syncd watch` flag and passes the ones you give through
  to the daemon; `--silent` and `--wait-for-remote` are always on.
- Running it again replaces the settings and restarts the service.
- `--name NAME` (on all three subcommands) runs an additional, separately
  named service — one per local/remote pair.
- `--bin PATH` selects the executable the service runs (default: the `syncd`
  that ran `service install`, so install from wherever the binary lives
  permanently).
- It refuses to install against a mounted share that has no `.synclock`
  (run `syncd init`); a share that simply isn't mounted right now is fine.

On macOS the service's stdout/stderr go to
`~/.local/state/syncd/service.{out,err}.log`; on Linux they go to the user
journal (`journalctl --user -u syncd-watch -f`). On Linux the service only
runs while you are logged in unless lingering is enabled
(`sudo loginctl enable-linger $USER`); `service install` says so when it
isn't. The first time the macOS service touches a network volume, macOS may
ask whether to allow it.

### Choosing a mode

Run `syncd watch` as your primary, continuous sync (via systemd/launchd —
see "Deployment" below). Use `syncd sync` for one-off jobs, folders you only
want synced occasionally or on demand, or as a periodic `--hash` integrity
verification pass alongside a running `syncd watch` daemon.

## Hash policies

The robust engine (used by `syncd watch` and `syncd sync --hash`) controls
when it computes a BLAKE3 content hash per scanned file:

- **`HashIfChanged`** (used for the watch daemon's remote poll and deep
  scan) — hashes only when a file's size+mtime differ from what's recorded
  in the manifest; unchanged files reuse the cached hash. This is the normal
  steady-state policy.
- **`HashAlways`** — hashes every scanned file regardless of manifest state.
  Used for a full-trust verification pass.
- **`HashNever`** — skips hashing entirely (`Hash` is left empty). Not used
  by the robust engine's normal paths; the fast reconciler doesn't hash at
  all regardless of policy, since it only compares size+mtime.

## `.syncdignore`

`syncd` always excludes a small set of built-in defaults, regardless of
configuration:

```text
.synclock                  # the remote canary file itself
*.tmp                      # atomic-write staging files
#recycle                   # Synology recycle bin
.venv                      # Python virtualenvs
.DS_Store, ._*, .fseventsd,
.DocumentRevisions-V100,
.Spotlight-V100, .TemporaryItems,
.Trashes, @eaDir           # macOS/Synology junk
```

To exclude anything else, add a `.syncdignore` file to the **local** root,
one pattern per line, using gitignore syntax:

- Blank lines and lines starting with `#` are comments (escape a literal
  leading `#` or `!` with a backslash: `\#weird-folder`).
- A pattern with no `/` (other than an optional trailing one) matches by
  name at any depth: `.venv` excludes every `.venv` directory anywhere in
  the tree, not just at the root.
- A pattern containing `/` is anchored to the root: `/only-at-root` matches
  only a top-level entry, never a nested one.
- A trailing `/` restricts the pattern to directories: `build/` won't match
  a plain file named `build`.
- `*` matches any run of characters within one path segment; `**` matches
  across segments (`cache/**/*.bin`); `?` matches one character.
- A line starting with `!` re-includes a path an earlier pattern (including
  a built-in default) would otherwise exclude.

Matching is symmetric: the same ruleset is applied to both the local and
remote scan, using paths relative to the local root, so an ignored path is
skipped no matter which side it happens to exist on. Ignored directories are
pruned outright — files inside them are never scanned or hashed. Files that
were already synced before being added to `.syncdignore` are left alone on
both sides; ignoring only stops *future* syncing, it doesn't retroactively
delete existing copies.

`.syncdignore` is itself an ordinary file — like `.gitignore` in a repo, it
syncs like anything else, so once it's synced once, both machines share the
same rules. On a brand-new local root with nothing synced yet, only the
built-in defaults apply until the first run pulls the file down.

## Delete safety

Two independent protections apply to deletions syncd makes on your behalf.

**Trash.** A file syncd deletes from a **local disk** is moved to
`~/.local/state/syncd/trash/<YYYY-MM-DD>/<root folder name>-<id>/<relative path>`
instead of being removed, so a delete propagated from the other side (or by
a `mirror`) can be undone by copying the file back. (`<id>` is a short hash
of the root's full path, so two roots with the same folder name stay
apart.) Trash older than `--trash-days` (default 30; 0 keeps it forever) is
pruned at startup, and daily by `watch`. A file deleted from a **network**
filesystem (SMB/NFS) is removed outright — trashing it would mean
downloading everything that gets deleted — so the recovery path there is
the server's own recycle bin (e.g. Synology's `#recycle`). If a file can't
be moved to the trash, the delete fails and is retried next run rather than
made unrecoverable. The trash is readable only by you. `--no-trash` turns
it off.

**`--max-delete`.** An unattended run that plans to delete more files from
one side than the limit allows stops before changing anything. The value is
a count (`100`; `0` forbids deleting), a percentage of the files on that
side (`50%`, the default — a percentage never trips on 10 or fewer
deletions), or `off`. A sudden mass delete is what a wiped local tree, or
the wrong share behind the mount point, looks like.

- `syncd sync --yes` fails the job with an error. Rerun without `--yes` to
  review the deletions, or raise the limit. Without `--yes` the limit does
  not apply: the confirmation prompt, which lists deletions first, is the
  gate.
- `syncd watch` applies the limit as a **rolling total over the last 10
  minutes**, not per cycle, so a large delete that reaches it in several
  small batches is still caught. A cycle that would go over is skipped and
  logged, and keeps being skipped (the rest of that cycle's changes wait
  too). If the deletions are intended, approve them:

  ```bash
  syncd approve-deletes --local ~/dev --remote /Volumes/dev
  ```

  It lists the deletions the daemon refused and approves exactly those. The
  daemon then carries them out on its next cycle. If the trees change in
  the meantime so that it plans something different, the approval is not
  used and you approve again. An approval is single-use and expires after
  an hour. Pass the daemon's `--state-dir`/`--lock-path` too if it was
  given them.

## Conflict strategy

When both sides changed since the last manifest record and their content
differs (robust/hash-based engine only):

1. Rename the local file to `*_conflict_YYYYMMDD_HHMMSS.ext` (truncating the
   basename if needed to stay under filesystem name-length limits).
2. Download the remote canonical file to the local canonical path.
3. Upload the local conflict copy to the remote conflict path.
4. Update the manifest for both the canonical and the conflict paths.

The fast reconciler's `two-way` mode instead applies "newer mtime wins" when
both sides changed the same file since the last run — no conflict copy is
created.

## Directory sync

File reconciliation drives directory handling; there's no separate directory
entry in the manifest. After file actions run each robust-engine cycle:

- Any directory that exists on one side but not the other is created on the
  missing side (handles creating an empty folder ahead of adding files to
  it).
- A directory is removed, on both sides, only if one of *that cycle's*
  delete actions just emptied it. A directory that was already empty before
  the cycle — and that the cycle didn't touch — is always left alone, so
  intentionally empty folders are never swept away as a side effect.

`syncd sync` has no dry-run/preview mode at all (for either file or
directory actions): before mutating anything it shows an interactive
confirmation summary of the planned file-level actions and waits for
approval, unless `--yes` is given to skip the prompt; directory create/
cleanup is never previewed in that summary either way. `syncd watch` has no
preview step of any kind — whole-tree directory reconciliation there only
happens on the `--remote-poll` and `--deep-scan` cycles, not on the
fsnotify-driven scoped reconcile — see "`syncd watch`" above.

## State directory

By default, state artifacts (SQLite manifest, lock file, JSON log) are
written under:

```text
~/.local/state/syncd/
```

- `manifest.db` — `syncd watch`'s manifest
- `jobs.db` — `syncd sync`'s own job state for fast (mirror/update/two-way)
  jobs, kept separate from the watch daemon's `manifest.db`
- `hash-<id>.db` — one file per `--hash` job, where `<id>` is a sanitized
  form of the job's name (or, for an ad-hoc `--left`/`--right --hash` run,
  the same short hash used as its job name). Each `--hash` job gets its own
  manifest db rather than sharing `jobs.db`, because its `file_manifest`
  table is keyed by relative path only (no job column) — sharing one db
  across jobs would let two jobs whose trees contain the same relative path
  corrupt each other's three-way baseline. The fast path doesn't need this:
  its `job_state` table is already namespaced per job.
- `sync.lock`
- `sync.log`
- `trash/` — files deleted from local disks (see "Delete safety")
- `service.out.log`, `service.err.log` — the macOS background service's
  stdout/stderr

`sync.log` is capped at 20MB; once it hits that size, the existing log is
rotated to `sync.log.old` (overwriting any previous one) before the next
run's log lines are written.

`syncd watch` accepts `--state-dir`, `--db-path`, `--log-path`, and
`--lock-path` to override any of the above. **`syncd sync` has no such
flags** — its db paths under `~/.local/state/syncd/` (`jobs.db`, and each
`--hash` job's own `hash-<id>.db`) are currently fixed and not
user-overridable.

`syncd` also maintains an exclusive lockfile so overlapping runs against the
same state directory can't corrupt each other, and validates a remote canary
file (`.synclock`) before touching the remote — `syncd init` bootstraps it,
and every sync/watch cycle re-checks it before making changes, so a network
mount that vanished mid-run is caught rather than silently writing nowhere
(or somewhere unexpected, if the mount point got reused by something else).

## Cross-compiling

Build for the current platform:

```bash
go build -o syncd ./cmd/syncd
```

Cross-compile binaries for other targets from macOS:

```bash
GOOS=darwin GOARCH=arm64 go build -o dist/syncd-darwin-arm64 ./cmd/syncd
GOOS=linux  GOARCH=amd64 go build -o dist/syncd-linux-amd64  ./cmd/syncd
```

(swap `GOARCH=amd64` for `arm64` if the Linux target is ARM).

## Deployment

Use `syncd service install` (see above) — it writes and loads the right
service definition for the machine it runs on.

`deploy/` holds hand-editable templates for setups it doesn't cover:

- `deploy/systemd/syncd.service` — a **system-wide** systemd unit for
  `syncd watch` (`service install` creates a per-user unit instead).
- `deploy/systemd/syncd.timer` + `deploy/systemd/syncd-verify.service` —
  **optional**, install both together: a periodic `syncd sync --hash`
  verification pass, independent of the watch daemon's own deep scan.
- `deploy/launchd/com.user.syncd.plist` — a launchd agent template.

## Build

```bash
go build ./cmd/syncd
```
