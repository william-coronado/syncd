package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeleteApprovalIsOneShot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.approve-deletes")
	if ConsumeDeleteApproval(path, "fp") {
		t.Fatal("approved with no token")
	}
	if err := WriteDeleteApproval(path, "fp"); err != nil {
		t.Fatal(err)
	}
	if !ConsumeDeleteApproval(path, "fp") {
		t.Fatal("fresh token was not honored")
	}
	if ConsumeDeleteApproval(path, "fp") {
		t.Fatal("token honored twice")
	}
}

// An approval is for the plan the user was shown, not whatever plan comes
// next - but it must survive a different plan's cycle to be used by its own.
func TestDeleteApprovalIsBoundToItsPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.approve-deletes")
	if err := WriteDeleteApproval(path, "plan-a"); err != nil {
		t.Fatal(err)
	}
	if ConsumeDeleteApproval(path, "plan-b") {
		t.Fatal("approval for plan-a honored for plan-b")
	}
	if ConsumeDeleteApproval(path, "") {
		t.Fatal("approval honored for an empty fingerprint")
	}
	if !ConsumeDeleteApproval(path, "plan-a") {
		t.Fatal("approval was used up by a different plan's cycle")
	}
}

func TestDeleteApprovalExpires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.approve-deletes")
	if err := WriteDeleteApproval(path, "fp"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * DeleteApprovalTTL)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if ConsumeDeleteApproval(path, "fp") {
		t.Fatal("expired token was honored")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("expired token was left behind")
	}
}

// A token that can't be removed would approve every later cycle as well.
func TestDeleteApprovalNotHonoredIfItCannotBeUsedUp(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can remove files from a read-only directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "x.approve-deletes")
	if err := WriteDeleteApproval(path, "fp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if ConsumeDeleteApproval(path, "fp") {
		t.Fatal("honored a token that could not be removed")
	}
}

func TestDeleteRefusalRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.refused-deletes")
	if _, err := ReadDeleteRefusal(path); !errors.Is(err, ErrNoDeleteRefusal) {
		t.Fatalf("no record: got %v", err)
	}
	want := DeleteRefusal{Time: time.Now().UTC().Truncate(time.Second), Fingerprint: "fp", Remote: 26, Tracked: 27, SampleRemote: []string{"a", "b"}}
	if err := WriteDeleteRefusal(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDeleteRefusal(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != want.Fingerprint || got.Remote != 26 || got.Tracked != 27 || len(got.SampleRemote) != 2 || !got.Time.Equal(want.Time) {
		t.Fatalf("round trip: %+v", got)
	}
}
