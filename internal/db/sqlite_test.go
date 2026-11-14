package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpen_PragmasTakeEffect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.db")
	sqlDB, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	var journalMode string
	if err := sqlDB.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := sqlDB.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}

	if got, want := sqlDB.Stats().MaxOpenConnections, 1; got != want {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, want)
	}
}

// TestOpen_DegradesWhenWALUnavailable forces the WAL attempt to fail (a
// read-only file/directory can't have -wal/-shm companion files created
// next to it) and asserts Open still succeeds, falling back to the
// default journal mode while keeping the busy_timeout.
func TestOpen_DegradesWhenWALUnavailable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses file permission checks")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.db")

	// Create the schema first with a plain connection that never requests
	// WAL, so the db file itself is left in the default journal mode. (If
	// we instead created it via our own Open, WAL would already be the
	// persisted journal mode, and simply reconnecting to a WAL database
	// inherently needs write access regardless of which pragmas the new
	// connection requests — that would defeat this test's setup.)
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("seed sql.Open: %v", err)
	}
	if _, err := seed.Exec(`
		CREATE TABLE IF NOT EXISTS file_manifest (
			path TEXT PRIMARY KEY,
			size INTEGER NOT NULL,
			mtime INTEGER NOT NULL,
			hash TEXT NOT NULL,
			last_synced INTEGER NOT NULL,
			remote_size INTEGER,
			remote_mtime INTEGER
		);
	`); err != nil {
		t.Fatalf("seed create file_manifest: %v", err)
	}
	if _, err := seed.Exec(`
		CREATE TABLE IF NOT EXISTS job_state (
			job   TEXT NOT NULL,
			path  TEXT NOT NULL,
			size  INTEGER NOT NULL,
			mtime INTEGER NOT NULL,
			PRIMARY KEY (job, path)
		);
	`); err != nil {
		t.Fatalf("seed create job_state: %v", err)
	}
	if _, err := seed.Exec(`
		CREATE TABLE IF NOT EXISTS dir_manifest (
			path TEXT PRIMARY KEY
		);
	`); err != nil {
		t.Fatalf("seed create dir_manifest: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	// Make the db file and its directory read-only so WAL's -wal/-shm
	// sidecar files cannot be created.
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod db file: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
		_ = os.Chmod(path, 0o644)
	})

	sqlDB2, err := Open(path)
	if err != nil {
		t.Fatalf("Open should degrade instead of failing when WAL is unavailable, got: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB2.Close() })

	var journalMode string
	if err := sqlDB2.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal_mode = %q, want a non-WAL fallback mode since WAL was forced to be unavailable", journalMode)
	}

	var busyTimeout int
	if err := sqlDB2.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000 even in the WAL-unavailable fallback", busyTimeout)
	}
}
