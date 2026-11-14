package fs

import "syscall"

// mntLocal is MNT_LOCAL from <sys/mount.h>; package syscall does not export it.
const mntLocal = 0x00001000

// isNetworkFilesystem reports whether path is on a network mount (smbfs,
// nfs, afpfs, ...): macOS flags every local filesystem MNT_LOCAL. A path
// that cannot be statfs'd is treated as local, which errs toward using the
// trash.
func isNetworkFilesystem(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	return st.Flags&mntLocal == 0
}
