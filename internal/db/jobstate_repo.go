package db

import (
	"context"
	"database/sql"
	"fmt"
)

type JobFileState struct {
	Path  string
	Size  int64
	Mtime int64
}

type JobStateRepo struct{ db *sql.DB }

func NewJobStateRepo(db *sql.DB) *JobStateRepo { return &JobStateRepo{db: db} }

func (r *JobStateRepo) Load(ctx context.Context, job string) (map[string]JobFileState, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT path, size, mtime FROM job_state WHERE job = ?`, job)
	if err != nil {
		return nil, fmt.Errorf("query job_state: %w", err)
	}
	defer rows.Close()
	out := make(map[string]JobFileState)
	for rows.Next() {
		var s JobFileState
		if err := rows.Scan(&s.Path, &s.Size, &s.Mtime); err != nil {
			return nil, fmt.Errorf("scan job_state: %w", err)
		}
		out[s.Path] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate job_state rows: %w", err)
	}
	return out, nil
}

func (r *JobStateRepo) Replace(ctx context.Context, job string, states map[string]JobFileState) (err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin job_state tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `DELETE FROM job_state WHERE job = ?`, job); err != nil {
		return fmt.Errorf("clear job_state: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO job_state(job, path, size, mtime) VALUES(?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare job_state insert: %w", err)
	}
	defer stmt.Close()
	for _, s := range states {
		if _, err = stmt.ExecContext(ctx, job, s.Path, s.Size, s.Mtime); err != nil {
			return fmt.Errorf("insert job_state %s: %w", s.Path, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit job_state tx: %w", err)
	}
	return nil
}
