package host

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		// The filesystem or kernel lacks RENAME_NOREPLACE.
		return linkRename(from, to, err)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
