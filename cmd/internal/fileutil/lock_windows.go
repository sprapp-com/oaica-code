//go:build windows

package fileutil

// lock_windows.go — the Windows half of lock.go.
//
// Without it Windows fell to lock_other.go's no-op, so every store under
// ~/.oaica lost updates there exactly as the Unix side did before its flock: a
// release artifact (oaica-windows-amd64.zip) ships, and the cross-process
// exclusion lock.go's own doc comment promises was simply absent — eleven
// concurrent `oaica auth login` would leave one credential, with no error on
// any of them (2026-09-26 audit).

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes Windows' equivalent of flock(LOCK_EX): exclusive and
// blocking, over one byte at offset 0 — enough for mutual exclusion, and no
// content is required of the file.
//
// LOCKFILE_FAIL_IMMEDIATELY is deliberately NOT set. A caller that cannot wait
// is the caller that loses the update, which is the failure this file exists to
// remove; the lock is released when the range is unlocked or the handle closed,
// and lock.go closes it on every path.
func lockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol)
}

func unlockFile(f *os.File) {
	var ol windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
