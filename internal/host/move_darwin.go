package host

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(from, to string) error {
	err := unix.RenamexNp(from, to, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) {
		// The filesystem lacks an exclusive rename.
		return linkRename(from, to, err)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
