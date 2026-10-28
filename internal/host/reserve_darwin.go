package host

import "os"

// macOS has no portable keep-size preallocation through the syscall package;
// rewriteInPlace still restores the original bytes if a write fails.
func reserveSpace(*os.File, int64) error { return nil }
