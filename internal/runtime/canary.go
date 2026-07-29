package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnsureRemoteCanary creates the remote canary file if it's missing, for
// first-time setup (see --init). It reports whether it actually created the
// file so callers can log accordingly.
func EnsureRemoteCanary(remoteRoot string) (created bool, err error) {
	canaryPath := filepath.Join(remoteRoot, ".synclock")
	if _, statErr := os.Stat(canaryPath); statErr == nil {
		return false, nil
	} else if !os.IsNotExist(statErr) {
		return false, fmt.Errorf("stat remote canary: %w", statErr)
	}
	if err := os.WriteFile(canaryPath, []byte("drivesync canary\n"), 0o644); err != nil {
		return false, fmt.Errorf("create remote canary: %w", err)
	}
	return true, nil
}

func CheckRemoteCanary(remoteRoot string) error {
	canaryPath := filepath.Join(remoteRoot, ".synclock")
	info, err := os.Stat(canaryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("remote canary file missing: %s", canaryPath)
		}
		return fmt.Errorf("stat remote canary: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("remote canary path is a directory: %s", canaryPath)
	}
	return nil
}
