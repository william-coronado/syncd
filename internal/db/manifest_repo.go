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

func (r *ManifestRepo) ApplyUpdates(ctx context.Context, upserts []ManifestRow, deletes []string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin manifest tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if len(upserts) > 0 {
		stmt, prepErr := tx.PrepareContext(ctx, `
			INSERT INTO file_manifest(path, size, mtime, hash, last_synced)
			VALUES(?, ?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET
				size = excluded.size,
				mtime = excluded.mtime,
				hash = excluded.hash,
				last_synced = excluded.last_synced
		`)
		if prepErr != nil {
			return fmt.Errorf("prepare upsert: %w", prepErr)
		}
		defer stmt.Close()
		for _, row := range upserts {
			if row.LastSynced == 0 {
				row.LastSynced = time.Now().Unix()
			}
			if _, execErr := stmt.ExecContext(ctx, row.Path, row.Size, row.Mtime, row.Hash, row.LastSynced); execErr != nil {
				return fmt.Errorf("upsert manifest %s: %w", row.Path, execErr)
			}
		}
	}

	if len(deletes) > 0 {
		stmt, prepErr := tx.PrepareContext(ctx, `DELETE FROM file_manifest WHERE path = ?`)
		if prepErr != nil {
			return fmt.Errorf("prepare delete: %w", prepErr)
		}
		defer stmt.Close()
		for _, path := range deletes {
			if _, execErr := stmt.ExecContext(ctx, path); execErr != nil {
				return fmt.Errorf("delete manifest %s: %w", path, execErr)
			}
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit manifest tx: %w", err)
	}
	return nil
}
