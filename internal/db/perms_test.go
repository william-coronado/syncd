package db

import (
	"os"
	"path/filepath"
	"testing"
)

func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("%s has mode %v, want no group/other access", path, perm)
	}
}

// The manifest lists every synced path and its hash.
func TestOpen_DatabaseFilesAreOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	sqlDB, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewManifestRepo(sqlDB).Count(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			assertOwnerOnly(t, p)
		}
	}
	sqlDB.Close()
}

// A database an older syncd created world-readable is tightened on open.
func TestOpen_TightensExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	sqlDB, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	sqlDB, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	assertOwnerOnly(t, path)
}
