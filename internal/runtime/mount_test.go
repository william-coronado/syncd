package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// stubMounts points the mount checks at test files and restores them after.
func stubMounts(t *testing.T, goos, fstab, mountinfo, volumes string, dev func(string) (uint64, error)) {
	t.Helper()
	oldGOOS, oldFstab, oldInfo, oldVolumes, oldDev := mountGOOS, fstabPath, mountinfoPath, volumesDir, deviceOf
	t.Cleanup(func() {
		mountGOOS, fstabPath, mountinfoPath, volumesDir, deviceOf = oldGOOS, oldFstab, oldInfo, oldVolumes, oldDev
	})
	mountGOOS, fstabPath, mountinfoPath, volumesDir = goos, fstab, mountinfo, volumes
	if dev != nil {
		deviceOf = dev
	}
}

// realTempDir is t.TempDir with symlinks resolved (macOS's /var is one),
// so paths written into fake fstab/mountinfo files match resolved paths.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnmountedMountPoint_LinuxFstabEntryNotMounted(t *testing.T) {
	dir := realTempDir(t)
	nas := filepath.Join(dir, "my nas")
	if err := os.Mkdir(nas, 0o755); err != nil { // an unmounted mount point is an empty directory
		t.Fatal(err)
	}
	fstab := writeFile(t, filepath.Join(dir, "fstab"), "# comment\n"+
		"UUID=abc / ext4 defaults 0 1\n"+
		"//nas/share "+filepath.Join(dir, `my\040nas`)+" cifs credentials=/x 0 0\n"+
		"/swapfile none swap sw 0 0\n")
	mountinfo := writeFile(t, filepath.Join(dir, "mountinfo"), "22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n")
	stubMounts(t, "linux", fstab, mountinfo, "", nil)

	for _, p := range []string{nas, filepath.Join(nas, "sub", "dir")} {
		got, err := UnmountedMountPoint(p)
		if err != nil {
			t.Fatal(err)
		}
		if got != nas {
			t.Errorf("UnmountedMountPoint(%q) = %q, want %q", p, got, nas)
		}
	}
	if got, err := UnmountedMountPoint(filepath.Join(dir, "elsewhere")); err != nil || got != "" {
		t.Errorf("path outside any fstab mount point: got %q, %v", got, err)
	}
}

func TestUnmountedMountPoint_LinuxMountedEntryPasses(t *testing.T) {
	dir := realTempDir(t)
	nas := filepath.Join(dir, "my nas")
	fstab := writeFile(t, filepath.Join(dir, "fstab"), "//nas/share "+filepath.Join(dir, `my\040nas`)+" cifs defaults 0 0\n")
	mountinfo := writeFile(t, filepath.Join(dir, "mountinfo"),
		"22 1 8:1 / / rw - ext4 /dev/sda1 rw\n"+
			"90 22 0:50 / "+filepath.Join(dir, `my\040nas`)+" rw,relatime shared:5 - cifs //nas/share rw\n")
	stubMounts(t, "linux", fstab, mountinfo, "", nil)
	if got, err := UnmountedMountPoint(filepath.Join(nas, "sub")); err != nil || got != "" {
		t.Errorf("mounted share: got %q, %v", got, err)
	}
}

func TestUnmountedMountPoint_LinuxWithoutMountinfoComparesDevices(t *testing.T) {
	dir := realTempDir(t)
	nas := filepath.Join(dir, "nas")
	if err := os.Mkdir(nas, 0o755); err != nil {
		t.Fatal(err)
	}
	fstab := writeFile(t, filepath.Join(dir, "fstab"), "//nas/share "+nas+" cifs defaults 0 0\n")
	stubMounts(t, "linux", fstab, filepath.Join(dir, "no-mountinfo"), "", nil)
	if got, err := UnmountedMountPoint(nas); err != nil || got != nas {
		t.Errorf("same device as its parent: got %q, %v, want %q", got, err, nas)
	}
}

func TestUnmountedMountPoint_LinuxNoFstabChecksNothing(t *testing.T) {
	dir := realTempDir(t)
	stubMounts(t, "linux", filepath.Join(dir, "absent"), filepath.Join(dir, "absent"), "", nil)
	if got, err := UnmountedMountPoint(filepath.Join(dir, "x")); err != nil || got != "" {
		t.Errorf("got %q, %v", got, err)
	}
}

// A symlink to an unmounted mount point is caught through its target.
func TestUnmountedMountPoint_FollowsSymlinkToMountPoint(t *testing.T) {
	dir := realTempDir(t)
	nas := filepath.Join(dir, "nas")
	if err := os.Mkdir(nas, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(nas, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	fstab := writeFile(t, filepath.Join(dir, "fstab"), "//nas/share "+nas+" cifs defaults 0 0\n")
	mountinfo := writeFile(t, filepath.Join(dir, "mountinfo"), "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n")
	stubMounts(t, "linux", fstab, mountinfo, "", nil)
	if got, err := UnmountedMountPoint(filepath.Join(link, "sub")); err != nil || got != nas {
		t.Errorf("got %q, %v, want %q", got, err, nas)
	}
}

func TestUnmountedMountPoint_DarwinVolumes(t *testing.T) {
	dir := realTempDir(t)
	volumes := filepath.Join(dir, "Volumes")
	if err := os.Mkdir(volumes, 0o755); err != nil {
		t.Fatal(err)
	}
	stubMounts(t, "darwin", "", "", volumes, nil)

	// Not mounted: /Volumes/share does not exist.
	share := filepath.Join(volumes, "share")
	if got, err := UnmountedMountPoint(filepath.Join(share, "dev")); err != nil || got != share {
		t.Errorf("missing volume: got %q, %v, want %q", got, err, share)
	}
	// A stale directory left behind on the boot volume: same device.
	if err := os.Mkdir(share, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := UnmountedMountPoint(filepath.Join(share, "dev")); err != nil || got != share {
		t.Errorf("stale volume directory: got %q, %v, want %q", got, err, share)
	}
	// Mounted: its own device.
	real := deviceOf
	deviceOf = func(p string) (uint64, error) {
		if p == share {
			return 4242, nil
		}
		return real(p)
	}
	if got, err := UnmountedMountPoint(filepath.Join(share, "dev")); err != nil || got != "" {
		t.Errorf("mounted volume: got %q, %v", got, err)
	}
	// Outside /Volumes nothing is checked.
	if got, err := UnmountedMountPoint(filepath.Join(dir, "home", "x")); err != nil || got != "" {
		t.Errorf("outside volumes: got %q, %v", got, err)
	}
}

// An fstab that exists but cannot be read must not pass as "nothing is
// supposed to be mounted": init would then go ahead under an unmounted share.
func TestUnmountedMountPoint_LinuxUnreadableFstabIsAnError(t *testing.T) {
	dir := realTempDir(t)
	unreadable := filepath.Join(dir, "fstab") // a directory: opens, but reading fails
	if err := os.Mkdir(unreadable, 0o755); err != nil {
		t.Fatal(err)
	}
	stubMounts(t, "linux", unreadable, filepath.Join(dir, "absent"), "", nil)
	if got, err := UnmountedMountPoint(filepath.Join(dir, "x")); err == nil {
		t.Fatalf("got %q with no error, want the fstab read error", got)
	}
}
