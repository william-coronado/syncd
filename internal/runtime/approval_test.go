package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeleteApprovalIsOneShot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.approve-deletes")
	if ConsumeDeleteApproval(path) {
		t.Fatal("approved with no token")
	}
	if err := WriteDeleteApproval(path); err != nil {
		t.Fatal(err)
	}
	if !ConsumeDeleteApproval(path) {
		t.Fatal("fresh token was not honored")
	}
	if ConsumeDeleteApproval(path) {
		t.Fatal("token honored twice")
	}
}

func TestDeleteApprovalExpires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.approve-deletes")
	if err := WriteDeleteApproval(path); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * DeleteApprovalTTL)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if ConsumeDeleteApproval(path) {
		t.Fatal("expired token was honored")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("expired token was left behind")
	}
}
