package db

import (
	"context"
	"database/sql"
	"fmt"
)

// DirRepo persists the set of directory paths the robust (hash-based)
// three-way engine has confirmed exist on both local and remote as of the
// last cycle. internal/app/directories.go's reconcileDirectories uses this
// baseline to distinguish a directory the user deliberately deleted (in the
// baseline, now missing on one side - propagate the delete) from a
// brand-new empty directory being created ahead of time (not in the
// baseline - mirror it to the missing side).
type DirRepo struct {
	db *sql.DB
}

func NewDirRepo(db *sql.DB) *DirRepo {
	return &DirRepo{db: db}
}

func (r *DirRepo) LoadAll(ctx context.Context) (map[string]struct{}, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT path FROM dir_manifest`)
	if err != nil {
		return nil, fmt.Errorf("query dir manifest: %w", err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan dir manifest row: %w", err)
		}
		out[path] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dir manifest rows: %w", err)
	}
	return out, nil
}

// ApplyUpdates adds and removes baseline rows in a single transaction. Every
// assignment to err below uses plain `=`, never `:=` - see ManifestRepo.
// ApplyUpdates' doc comment for why that matters (this function follows the
// identical named-return-and-deferred-rollback pattern).
func (r *DirRepo) ApplyUpdates(ctx context.Context, adds []string, removes []string) (err error) {
	if len(adds) == 0 && len(removes) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin dir manifest tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if len(adds) > 0 {
		var stmt *sql.Stmt
		stmt, err = tx.PrepareContext(ctx, `INSERT INTO dir_manifest(path) VALUES(?) ON CONFLICT(path) DO NOTHING`)
		if err != nil {
			return fmt.Errorf("prepare dir insert: %w", err)
		}
		defer stmt.Close()
		for _, path := range adds {
			if _, err = stmt.ExecContext(ctx, path); err != nil {
				return fmt.Errorf("insert dir %s: %w", path, err)
			}
		}
	}

	if len(removes) > 0 {
		var stmt *sql.Stmt
		stmt, err = tx.PrepareContext(ctx, `DELETE FROM dir_manifest WHERE path = ?`)
		if err != nil {
			return fmt.Errorf("prepare dir delete: %w", err)
		}
		defer stmt.Close()
		for _, path := range removes {
			if _, err = stmt.ExecContext(ctx, path); err != nil {
				return fmt.Errorf("delete dir %s: %w", path, err)
			}
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit dir manifest tx: %w", err)
	}
	return nil
}
