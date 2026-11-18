package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// syncd init on a share that is not mounted would put the remote root and
// its .synclock canary on the local disk under the mount point; the canary
// would then let watch "sync to the share" while it is missing.
func TestRunInit_RefusesRemoteUnderUnmountedMountPoint(t *testing.T) {
	parent := t.TempDir()
	mountPoint := filepath.Join(parent, "nas")
	if err := os.Mkdir(mountPoint, 0o755); err != nil {
		t.Fatal(err)
	}
	remoteRoot := filepath.Join(mountPoint, "dev")
	localRoot := filepath.Join(parent, "local")

	orig := unmountedMountPoint
	t.Cleanup(func() { unmountedMountPoint = orig })
	unmountedMountPoint = func(path string) (string, error) {
		if strings.HasPrefix(path, mountPoint) {
			return mountPoint, nil
		}
		return "", nil
	}

	err := RunInit(localRoot, remoteRoot, newTestLogger(t))
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("RunInit error = %v, want a not-mounted refusal", err)
	}
	for _, p := range []string{remoteRoot, localRoot} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s was created despite the refusal (err=%v)", p, err)
		}
	}
}

// The share can go away between the first check and the canary write; the
// canary must still never be written to the local disk.
func TestRunInit_RechecksMountBeforeWritingCanary(t *testing.T) {
	parent := t.TempDir()
	remoteRoot := filepath.Join(parent, "nas", "dev")
	calls := 0
	orig := unmountedMountPoint
	t.Cleanup(func() { unmountedMountPoint = orig })
	unmountedMountPoint = func(string) (string, error) {
		calls++
		if calls == 1 {
			return "", nil // mounted at the first check
		}
		return filepath.Join(parent, "nas"), nil // gone by the canary write
	}

	err := RunInit(filepath.Join(parent, "local"), remoteRoot, newTestLogger(t))
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("RunInit error = %v, want a not-mounted refusal", err)
	}
	if _, err := os.Lstat(filepath.Join(remoteRoot, ".synclock")); !os.IsNotExist(err) {
		t.Fatalf("canary written despite the share going away (err=%v)", err)
	}
}
