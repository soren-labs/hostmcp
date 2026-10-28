package host

import (
	"errors"
	"os"
	"syscall"
)

// reserveSpace allocates the blocks an in-place patch will grow into without
// changing the file size, so a full disk is reported before any byte changes.
// Filesystems without fallocate support are written without a reservation.
func reserveSpace(f *os.File, size int64) error {
	const keepSize = 0x01 // FALLOC_FL_KEEP_SIZE
	err := syscall.Fallocate(int(f.Fd()), keepSize, 0, size)
	if err == nil || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EINVAL) {
		return nil
	}
	return err
}
