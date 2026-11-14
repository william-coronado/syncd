package fs

import "syscall"

// Filesystem magic numbers (linux/magic.h) of the network filesystems a
// sync root is realistically mounted from.
const (
	cifsMagic = 0xFF534D42 // cifs (SMB1 superblock magic; also what cifs.ko reports)
	smb2Magic = 0xFE534D42 // cifs mounted with SMB2+
	smbMagic  = 0x517B     // legacy smbfs
	nfsMagic  = 0x6969
)

// isNetworkFilesystem reports whether path is on an SMB or NFS mount. A
// path that cannot be statfs'd is treated as local, which errs toward using
// the trash.
func isNetworkFilesystem(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	// Type's width and signedness differ by architecture.
	switch uint32(st.Type) {
	case cifsMagic, smb2Magic, smbMagic, nfsMagic:
		return true
	}
	return false
}
