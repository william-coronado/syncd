package runtime

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
)

// Indirected for tests; both platforms' checks run on either.
var (
	mountGOOS     = goruntime.GOOS
	fstabPath     = "/etc/fstab"
	mountinfoPath = "/proc/self/mountinfo"
	volumesDir    = "/Volumes"
	deviceOf      = func(path string) (uint64, error) {
		info, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, fmt.Errorf("no device number for %s", path)
		}
		return uint64(st.Dev), nil
	}
)

// UnmountedMountPoint returns the mount point that should hold path but has
// nothing mounted on it right now, or "" if there is none (or none it can
// tell). It exists for `syncd init`: initializing a remote root on an
// unmounted share would create the directory and its .synclock canary on
// the local disk underneath the mount point, and the canary - the guard
// that makes every watch cycle skip while the share is missing - would then
// vouch for the local disk instead.
//
// Linux: a mount point listed in /etc/fstab that /proc/self/mountinfo does
// not show as mounted (on a system without mountinfo, one on the same
// device as its parent). macOS: /Volumes/<name> that is missing, or is a
// leftover directory on the boot volume rather than a mounted volume.
// Symlinks in path are resolved first, so a link to a mount point counts.
func UnmountedMountPoint(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = resolveExistingPrefix(abs)
	switch mountGOOS {
	case "linux":
		return unmountedFstabEntry(abs)
	case "darwin":
		return unmountedVolume(abs)
	}
	return "", nil
}

// resolveExistingPrefix resolves symlinks in the longest existing prefix of
// abs and re-appends the rest, which may not exist yet.
func resolveExistingPrefix(abs string) string {
	rest := ""
	for cur := abs; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func unmountedFstabEntry(abs string) (string, error) {
	expected, err := readMountPoints(fstabPath, 1)
	if err != nil || len(expected) == 0 {
		return "", nil // no fstab to say what should be mounted
	}
	mounted, infoErr := readMountPoints(mountinfoPath, 4)
	for dir := abs; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if !expected[dir] {
			continue
		}
		if infoErr == nil {
			if !mounted[dir] {
				return dir, nil
			}
			continue
		}
		same, err := sameDeviceAsParent(dir)
		if os.IsNotExist(err) || (err == nil && same) {
			return dir, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", nil
}

func unmountedVolume(abs string) (string, error) {
	prefix := volumesDir + string(filepath.Separator)
	if !strings.HasPrefix(abs, prefix) {
		return "", nil
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(abs, prefix), string(filepath.Separator))
	vol := filepath.Join(volumesDir, name)
	same, err := sameDeviceAsParent(vol)
	if os.IsNotExist(err) || (err == nil && same) {
		return vol, nil
	}
	return "", err
}

// sameDeviceAsParent reports whether dir is on the same device as its
// parent, i.e. is not a mount point.
func sameDeviceAsParent(dir string) (bool, error) {
	d, err := deviceOf(dir)
	if err != nil {
		return false, err
	}
	p, err := deviceOf(filepath.Dir(dir))
	if err != nil {
		return false, err
	}
	return d == p, nil
}

// readMountPoints returns the absolute mount points in whitespace-separated
// column col of the fstab-format file (col 1) or mountinfo file (col 4) at
// path. Non-path entries ("none", swap) are skipped.
func readMountPoints(path string, col int) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[string]bool)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) <= col {
			continue
		}
		mp := unescapeMountField(fields[col])
		if filepath.IsAbs(mp) {
			out[filepath.Clean(mp)] = true
		}
	}
	return out, sc.Err()
}

// unescapeMountField decodes the octal escapes (\040 for a space, ...) that
// fstab and mountinfo use for whitespace and backslashes in paths.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
