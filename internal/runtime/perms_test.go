package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// State files list every synced path (log, manifest), so they are the
// user's alone: no group or other access, for new files and for files an
// older syncd created world-readable.
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

func TestResolvePaths_CreatesOwnerOnlyStateDir(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if _, err := ResolvePaths(Config{StateDir: stateDir}); err != nil {
		t.Fatal(err)
	}
	assertOwnerOnly(t, stateDir)
}

func TestNewLogger_LogFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "new.log")
	old := filepath.Join(dir, "old.log")
	if err := os.WriteFile(old, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(old, 0o644); err != nil { // defeat the umask
		t.Fatal(err)
	}
	for _, p := range []string{fresh, old} {
		l, err := NewLogger(true, p)
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
		assertOwnerOnly(t, p)
	}
}

func TestAcquireLock_LockFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.lock")
	l, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	assertOwnerOnly(t, path)
}
