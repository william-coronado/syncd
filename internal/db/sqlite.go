package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// dsn builds the modernc.org/sqlite connection string for path. The driver
// treats each "_pragma" query parameter as a verbatim PRAGMA statement to run
// on every new physical connection (see modernc.org/sqlite's
// applyQueryParams), so this is how busy_timeout and journal_mode get set
// before any of our own SQL runs.
func dsn(path string, wal bool) string {
	q := "?_pragma=busy_timeout(5000)"
	if wal {
		q += "&_pragma=journal_mode(WAL)"
	}
	return path + q
}

// Open opens (creating if needed) the SQLite database at path.
//
// It sets a 5s busy_timeout so a contended writer waits instead of failing
// instantly with SQLITE_BUSY, and tries to enable WAL journaling for better
// concurrent read/write behavior. WAL isn't available on every filesystem
// (e.g. a read-only mount, or certain network mounts) — if it can't be
// enabled, Open degrades to the default journal mode rather than failing;
// busy_timeout still applies either way.
//
// The pool is capped at one connection (SetMaxOpenConns(1)): the manifest is
// a single-writer store, so there is never a legitimate reason for a second
// concurrent connection. Capping it means a future leaked transaction (one
// that checks out the only connection and never returns it) surfaces as an
// obvious blocked/timed-out caller on the very next call, rather than
// quietly growing the pool and masking the leak.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if pingErr := db.Ping(); pingErr != nil {
		_ = db.Close()
		db, err = sql.Open("sqlite", dsn(path, false))
		if err != nil {
			return nil, fmt.Errorf("open sqlite (WAL fallback): %w", err)
		}
		if pingErr := db.Ping(); pingErr != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open sqlite: %w", pingErr)
		}
	}
	db.SetMaxOpenConns(1)

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
	// SQLite has no ADD COLUMN IF NOT EXISTS, and a manifest created by an
	// older syncd must keep working in place, so each column is added only
	// when hasColumn says it is missing. Each ALTER TABLE is a literal at its
	// own Exec call: identifiers can't be bound as parameters, and no SQL
	// text here is ever built from or passed in as a variable.
	hasRemoteSize, err := hasColumn(db, "file_manifest", "remote_size")
	if err == nil && !hasRemoteSize {
		_, err = db.Exec(`ALTER TABLE file_manifest ADD COLUMN remote_size INTEGER`)
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate manifest schema (remote_size): %w", err)
	}
	hasRemoteMtime, err := hasColumn(db, "file_manifest", "remote_mtime")
	if err == nil && !hasRemoteMtime {
		_, err = db.Exec(`ALTER TABLE file_manifest ADD COLUMN remote_mtime INTEGER`)
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate manifest schema (remote_mtime): %w", err)
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
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS dir_manifest (
			path TEXT PRIMARY KEY
		);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate dir_manifest schema: %w", err)
	}
	return db, nil
}

// hasColumn reports whether table has a column named column. Both names
// are bound as parameters to the pragma_table_info table-valued function.
func hasColumn(db *sql.DB, table, column string) (bool, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	return n > 0, nil
}
