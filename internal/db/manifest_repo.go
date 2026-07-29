package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type ManifestRow struct {
	Path       string
	Size       int64
	Mtime      int64
	Hash       string
	LastSynced int64
}

type ManifestRepo struct {
	db *sql.DB
}

func NewManifestRepo(db *sql.DB) *ManifestRepo {
	return &ManifestRepo{db: db}
}

func (r *ManifestRepo) LoadAll(ctx context.Context) (map[string]ManifestRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT path, size, mtime, hash, last_synced FROM file_manifest`)
	if err != nil {
		return nil, fmt.Errorf("query manifest: %w", err)
	}
	defer rows.Close()

	out := make(map[string]ManifestRow)
	for rows.Next() {
		var row ManifestRow
		if err := rows.Scan(&row.Path, &row.Size, &row.Mtime, &row.Hash, &row.LastSynced); err != nil {
			return nil, fmt.Errorf("scan manifest row: %w", err)
		}
		out[row.Path] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate manifest rows: %w", err)
	}
	return out, nil
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
			INSERT INTO file_manifest(path, size, mtime, hash, last_synced)
			VALUES(?, ?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET
				size = excluded.size,
				mtime = excluded.mtime,
				hash = excluded.hash,
				last_synced = excluded.last_synced
		`)
		if err != nil {
			return fmt.Errorf("prepare upsert: %w", err)
		}
		defer stmt.Close()
		for _, row := range upserts {
			if row.LastSynced == 0 {
				row.LastSynced = time.Now().Unix()
			}
			if _, err = stmt.ExecContext(ctx, row.Path, row.Size, row.Mtime, row.Hash, row.LastSynced); err != nil {
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
