# DriveSync

DriveSync is a cross-platform Go CLI that performs **state-based three-way synchronization** between:

- a local directory (`--local`)
- a mounted SMB directory (`--remote`)
- a local SQLite manifest database (`file_manifest`)

It is designed for macOS and Ubuntu environments where direct SMB development can cause lock or write instability.

## Features

- Three-way reconciliation (`local`, `remote`, `manifest`)
- `--dry-run` action planning with no filesystem mutation
- `--silent` JSON logging for unattended runs, with automatic log rotation
- Exclusive lockfile to prevent overlapping sync runs
- Remote canary validation (`.synclock`), with `--init` to bootstrap it
- NFC path normalization for macOS/Ubuntu interoperability
- SMB-invalid local filename detection and skip logging
- Atomic writes via temp-file + rename (including SMB targets)
- Preserve-both conflict pipeline with timestamped conflict files
- Empty directory sync (create on the missing side; clean up directories
  this run's deletes just emptied)
- `.drivesyncignore`, gitignore-style, for excluding files/directories from
  sync entirely, with sensible defaults built in
- Non-zero exit code whenever any action fails or the run is interrupted, so
  cron/systemd/launchd can actually alert on it
- Periodic scan progress logging, so a large tree over a slow SMB mount stays
  visibly alive instead of printing nothing until it's entirely done

## Build

```bash
go build ./cmd/drivesync
```

## First-time setup

Before the first real sync, bootstrap both roots and the remote canary file:

```bash
./drivesync --init --local /path/to/local --remote /path/to/mounted/smb
```

This creates `--local` and `--remote` if they don't exist yet and writes
`.synclock` on the remote if it's missing, then exits without syncing.
Safe to run again later; it's a no-op if everything is already in place.

## Usage

```bash
./drivesync --local /path/to/local --remote /path/to/mounted/smb
```

### Dry run

```bash
./drivesync --local /path/to/local --remote /path/to/mounted/smb --dry-run
```

Both `--dry-run` and normal runs scan and hash the full local and remote
trees before any plan is printed or any action executes, so the first sync
against a large pre-existing tree (or a slow SMB link) can take a while
before producing output. Every few seconds each side logs how many files
it's scanned so far, so a long scan reads as "still working," not "hung."
Excluding heavy, regenerable content you don't need synced (`.git/`,
`node_modules/`, `__pycache__/`, build output, etc.) via `.drivesyncignore`
(see below) is the main way to cut that first-scan time down.

### Silent mode

```bash
./drivesync --local /path/to/local --remote /path/to/mounted/smb --silent
```

`sync.log` is capped at 20MB; once it hits that size, the existing log is
rotated to `sync.log.old` (overwriting any previous one) before the next
run's log lines are written.

By default, state artifacts are written under:

```text
~/.local/state/drivesync/
```

- `manifest.db`
- `sync.lock`
- `sync.log`

Override with:

- `--state-dir`
- `--db-path`
- `--lock-path`
- `--log-path`

## Conflict Strategy

When both sides changed since last manifest and content differs:

1. Rename local file to `*_conflict_YYYYMMDD_HHMMSS.ext` (truncating basename if needed).
2. Download remote canonical file to local canonical path.
3. Upload conflict copy to remote conflict path.
4. Update manifest for both canonical and conflict paths.

## Directory Sync

File reconciliation drives directory handling; there's no separate directory
entry in the manifest. After file actions run each sync:

- Any directory that exists on one side but not the other is created on the
  missing side (handles creating an empty folder ahead of adding files to it).
- A directory is removed, on both sides, only if one of *this run's* delete
  actions just emptied it. A directory that was already empty before the run
  - and that the run didn't touch - is always left alone, so intentionally
  empty folders are never swept away as a side effect.

This is not previewed in `--dry-run` output; only file-level actions are.

## Ignoring Files and Directories

DriveSync always excludes a small set of built-in defaults, regardless of
configuration:

```text
.synclock   # the remote canary file itself
*.tmp       # atomic-write staging files
#recycle    # Synology recycle bin
.venv       # Python virtualenvs
```

To exclude anything else, add a `.drivesyncignore` file to the **local**
root, one pattern per line, using gitignore syntax:

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
were already synced before being added to `.drivesyncignore` are left alone
on both sides; ignoring only stops *future* syncing, it doesn't retroactively
delete existing copies.

`.drivesyncignore` is itself an ordinary file — like `.gitignore` in a repo,
it syncs like anything else, so once it's synced once, both machines share
the same rules. On a brand-new local root with nothing synced yet, only the
built-in defaults apply until the first run pulls the file down.

## Scheduling

Boilerplate launch configurations are included in:

- `deploy/systemd/drivesync.service`
- `deploy/systemd/drivesync.timer`
- `deploy/launchd/com.user.drivesync.plist`

Fill in the real `--local`/`--remote` paths for each machine before
installing. Cross-compile binaries for each target from macOS with:

```bash
GOOS=darwin GOARCH=arm64 go build -o dist/drivesync-darwin-arm64 ./cmd/drivesync
GOOS=linux  GOARCH=amd64 go build -o dist/drivesync-linux-amd64  ./cmd/drivesync
```

(swap `GOARCH=amd64` for `arm64` if the Ubuntu Server target is ARM).
