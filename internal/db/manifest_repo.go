package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type ManifestRow struct {
	Path string
	// Size and Mtime are the LOCAL side's stat as of the last sync. They
	// double as the remote side's stat unless RemoteSize/RemoteMtime say
	// otherwise (see RemoteStat).
	Size       int64
	Mtime      int64
	Hash       string
	LastSynced int64
	// RemoteSize and RemoteMtime record the remote side's stat when it
	// differs from the local one - e.g. an identical pair whose copies were
	// made without preserving mtime. Both zero means "same as Size/Mtime"
	// (stored as NULL), which is what every copy action produces, since
	// fs.AtomicCopyFile carries the source mtime across. Without a separate
	// remote stat, the HashIfChanged cache misses on the remote side for
	// every such pair on every --remote-poll, re-hashing it over SMB forever.
	RemoteSize  int64
	RemoteMtime int64
}

// RemoteStat returns the remote side's recorded size and mtime: the
// explicit RemoteSize/RemoteMtime when set, else the shared Size/Mtime.
func (r ManifestRow) RemoteStat() (size, mtime int64) {
	if r.RemoteSize == 0 && r.RemoteMtime == 0 {
		return r.Size, r.Mtime
	}
	return r.RemoteSize, r.RemoteMtime
}

// RemoteStatView returns manifest as the remote-side scanner should see it
// for its HashIfChanged cache: each row's Size/Mtime replaced by its
// RemoteStat. When no row records a distinct remote stat (the common case)
// manifest itself is returned, with no copy.
func RemoteStatView(manifest map[string]ManifestRow) map[string]ManifestRow {
	distinct := false
	for _, row := range manifest {
		if s, m := row.RemoteStat(); s != row.Size || m != row.Mtime {
			distinct = true
			break
		}
	}
	if !distinct {
		return manifest
	}
	view := make(map[string]ManifestRow, len(manifest))
	for path, row := range manifest {
		row.Size, row.Mtime = row.RemoteStat()
		row.RemoteSize, row.RemoteMtime = 0, 0
		view[path] = row
	}
	return view
}

type ManifestRepo struct {
	db *sql.DB
}

func NewManifestRepo(db *sql.DB) *ManifestRepo {
	return &ManifestRepo{db: db}
}

func (r *ManifestRepo) LoadAll(ctx context.Context) (map[string]ManifestRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+manifestColumns+` FROM file_manifest`)
	if err != nil {
		return nil, fmt.Errorf("query manifest: %w", err)
	}
	out := make(map[string]ManifestRow)
	if err := scanManifestRows(rows, out); err != nil {
		return nil, err
	}
	return out, nil
}

// manifestLoadChunk bounds how many paths LoadPaths binds per query, safely
// under SQLite's host-parameter limit.
const manifestLoadChunk = 500

// LoadPaths returns the manifest rows for just the given paths (absent ones
// are simply missing from the result). The scoped watch cycle reconciles a
// handful of fsnotify paths at a time; loading the whole manifest for each
// batch made its cost proportional to the tree size instead of the batch.
func (r *ManifestRepo) LoadPaths(ctx context.Context, paths map[string]struct{}) (map[string]ManifestRow, error) {
	out := make(map[string]ManifestRow, len(paths))
	batch := make([]any, 0, manifestLoadChunk)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		rows, err := r.db.QueryContext(ctx, `SELECT `+manifestColumns+` FROM file_manifest WHERE path IN (`+placeholders+`)`, batch...)
		if err != nil {
			return fmt.Errorf("query manifest paths: %w", err)
		}
		batch = batch[:0]
		return scanManifestRows(rows, out)
	}
	for p := range paths {
		batch = append(batch, p)
		if len(batch) == manifestLoadChunk {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadPrefix returns every manifest row strictly beneath directory dirRel
// ("a/b" -> "a/b/x", "a/b/c/y", but never "a/bc"). Matching is on whole
// path segments via a range scan on the primary key: '0' is the byte after
// '/', so [dirRel+"/", dirRel+"0") is exactly the paths starting with
// dirRel+"/".
func (r *ManifestRepo) LoadPrefix(ctx context.Context, dirRel string) (map[string]ManifestRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+manifestColumns+` FROM file_manifest WHERE path >= ? AND path < ?`, dirRel+"/", dirRel+"0")
	if err != nil {
		return nil, fmt.Errorf("query manifest prefix: %w", err)
	}
	out := make(map[string]ManifestRow)
	if err := scanManifestRows(rows, out); err != nil {
		return nil, err
	}
	return out, nil
}

const manifestColumns = `path, size, mtime, hash, last_synced, remote_size, remote_mtime`

// scanManifestRows drains rows (selected with manifestColumns) into out and
// closes rows. NULL remote_size/remote_mtime (no distinct remote stat, and
// every row written before those columns existed) scan as zero - see
// ManifestRow.RemoteStat.
func scanManifestRows(rows *sql.Rows, out map[string]ManifestRow) error {
	defer rows.Close()
	for rows.Next() {
		var row ManifestRow
		var remoteSize, remoteMtime sql.NullInt64
		if err := rows.Scan(&row.Path, &row.Size, &row.Mtime, &row.Hash, &row.LastSynced, &remoteSize, &remoteMtime); err != nil {
			return fmt.Errorf("scan manifest row: %w", err)
		}
		if remoteSize.Valid && remoteMtime.Valid {
			row.RemoteSize, row.RemoteMtime = remoteSize.Int64, remoteMtime.Int64
		}
		out[row.Path] = row
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate manifest rows: %w", err)
	}
	return nil
}

func (r *ManifestRepo) ApplyUpdates(ctx context.Context, upserts []ManifestRow, deletes []string) (err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin manifest tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Every assignment to err below (here and at the four other sites in
	// this function) uses plain `=`, never `:=` - err is this function's
	// NAMED RETURN, which the deferred rollback closure above inspects.
	// This isn't cosmetic: this function's history (commit 4ee0907) is
	// exactly this class of bug - err used to be unnamed and each failure
	// path assigned to a separately-named local (prepErr/execErr) that the
	// deferred rollback never looked at, so a failed transaction's
	// connection - and SQLite's write lock - leaked permanently. `var stmt
	// *sql.Stmt` is declared separately (rather than `stmt, err :=
	// tx.PrepareContext(...)`) purely so this line can assign to err with
	// `=` instead of accidentally declaring a new, block-scoped err that
	// shadows the named return. A future "simplify" pass collapsing these
	// two lines back into one `:=` would reintroduce the same class of bug
	// this fixed - keep all five sites using `=`.
	if len(upserts) > 0 {
		var stmt *sql.Stmt
		stmt, err = tx.PrepareContext(ctx, `
			INSERT INTO file_manifest(path, size, mtime, hash, last_synced, remote_size, remote_mtime)
			VALUES(?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET
				size = excluded.size,
				mtime = excluded.mtime,
				hash = excluded.hash,
				last_synced = excluded.last_synced,
				remote_size = excluded.remote_size,
				remote_mtime = excluded.remote_mtime
		`)
		if err != nil {
			return fmt.Errorf("prepare upsert: %w", err)
		}
		defer stmt.Close()
		for _, row := range upserts {
			if row.LastSynced == 0 {
				row.LastSynced = time.Now().Unix()
			}
			var remoteSize, remoteMtime any // NULL unless a distinct remote stat is recorded
			if row.RemoteSize != 0 || row.RemoteMtime != 0 {
				remoteSize, remoteMtime = row.RemoteSize, row.RemoteMtime
			}
			if _, err = stmt.ExecContext(ctx, row.Path, row.Size, row.Mtime, row.Hash, row.LastSynced, remoteSize, remoteMtime); err != nil {
				return fmt.Errorf("upsert manifest %s: %w", row.Path, err)
			}
		}
	}

	if len(deletes) > 0 {
		var stmt *sql.Stmt
		stmt, err = tx.PrepareContext(ctx, `DELETE FROM file_manifest WHERE path = ?`)
		if err != nil {
			return fmt.Errorf("prepare delete: %w", err)
		}
		defer stmt.Close()
		for _, path := range deletes {
			if _, err = stmt.ExecContext(ctx, path); err != nil {
				return fmt.Errorf("delete manifest %s: %w", path, err)
			}
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit manifest tx: %w", err)
	}
	return nil
}
