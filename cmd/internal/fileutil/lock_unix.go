//go:build unix

package fileutil

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock, blocking until it is granted.
// Flock is per open-file-description, which is exactly the scope needed here:
// each process opens "<path>.lock" itself, so a second process blocks while
// the first holds it, and the kernel releases the lock if the holder dies
// (no stale lock to clean up after a crash).
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
