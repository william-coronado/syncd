package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS file_manifest (
			path TEXT PRIMARY KEY,
			size INTEGER NOT NULL,
			mtime INTEGER NOT NULL,
			hash TEXT NOT NULL,
			last_synced INTEGER NOT NULL
		);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate manifest schema: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS job_state (
			job   TEXT NOT NULL,
			path  TEXT NOT NULL,
			size  INTEGER NOT NULL,
			mtime INTEGER NOT NULL,
			PRIMARY KEY (job, path)
		);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate job_state schema: %w", err)
	}
	return db, nil
}
