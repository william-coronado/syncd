package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureRemoteCanary_CreatesWhenMissing(t *testing.T) {
	remoteRoot := t.TempDir()

	created, err := EnsureRemoteCanary(remoteRoot)
	if err != nil {
		t.Fatalf("EnsureRemoteCanary: %v", err)
	}
	if !created {
		t.Fatal("expected created=true when canary was missing")
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, ".synclock")); err != nil {
		t.Fatalf("expected canary file to exist: %v", err)
	}
}

func TestEnsureRemoteCanary_NoOpWhenPresent(t *testing.T) {
	remoteRoot := t.TempDir()
	canaryPath := filepath.Join(remoteRoot, ".synclock")
	if err := os.WriteFile(canaryPath, []byte("existing"), 0o644); err != nil {
		t.Fatalf("seed canary: %v", err)
	}

	created, err := EnsureRemoteCanary(remoteRoot)
	if err != nil {
		t.Fatalf("EnsureRemoteCanary: %v", err)
	}
	if created {
		t.Fatal("expected created=false when canary already present")
	}
	content, err := os.ReadFile(canaryPath)
	if err != nil {
		t.Fatalf("read canary: %v", err)
	}
	if string(content) != "existing" {
		t.Fatalf("expected existing canary content to be left untouched, got %q", content)
	}
}

func TestCheckRemoteCanary_ErrorsWhenMissing(t *testing.T) {
	remoteRoot := t.TempDir()
	if err := CheckRemoteCanary(remoteRoot); err == nil {
		t.Fatal("expected error when canary is missing")
	}
}

func TestCheckRemoteCanary_PassesAfterEnsure(t *testing.T) {
	remoteRoot := t.TempDir()
	if _, err := EnsureRemoteCanary(remoteRoot); err != nil {
		t.Fatalf("EnsureRemoteCanary: %v", err)
	}
	if err := CheckRemoteCanary(remoteRoot); err != nil {
		t.Fatalf("expected canary check to pass after EnsureRemoteCanary, got %v", err)
	}
}
