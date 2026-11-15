package fs

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameExclusive(src *os.File, old string, dst *os.File, new string) error {
	return unix.Renameat2(fd(src), old, fd(dst), new, unix.RENAME_NOREPLACE)
}
