package fileutil

// lock.go — cross-process mutual exclusion for the read-modify-write stores
// under ~/.oaica.
//
// WriteFileAtomic makes each individual write torn-free, which is why this
// class of bug survived nine audit rounds: an atomic rename guarantees a
// reader never sees half a file, but it does NOT stop a LOST UPDATE. Every
// writer here is load → mutate in memory → write the whole file, so two
// writers that overlap in time both read the same snapshot and whichever
// renames last wins with the other's mutation absent — while both print
// success.
//
// Reproduced (2026-09-26 audit, fourth round) with eleven concurrent
// `oaica auth login <provider> --key ...`: eleven "Logged in to <provider>"
// lines, one credential in auth.json. Eight concurrent `oaica remote add`
// left one remote. A provisioning script that configures several providers
// or remotes in parallel reports complete success and leaves one configured,
// and the user discovers it at launch time as an unexplained "needs a key".
//
// The lock is advisory (flock) and taken on a sibling "<path>.lock" file
// rather than the store itself: the store is replaced by rename on every
// write, so a lock held on its inode would be held on a file that no longer
// exists by the time the second writer looks — the lock file is created once
// and never replaced.
//
// The platform half lives in lock_unix.go (flock) and lock_windows.go
// (LockFileEx). lock_other.go is the honest no-op for platforms that have
// neither; Windows is deliberately not in it, because oaica-windows-amd64 is a
// release artifact and a silent no-op there shipped the lost update until the
// 2026-09-26 audit.

import (
	"fmt"
	"os"
	"path/filepath"
)

// WithFileLock runs fn while holding an exclusive cross-process lock covering
// path, creating path's directory if needed. The lock is released even if fn
// panics. A caller whose path cannot be resolved (no home directory) gets fn
// run unlocked rather than an error — the alternative is refusing to save a
// credential the user just typed.
//
// Every branch that cannot place the lock runs fn anyway, and SAYS so: a lock
// that cannot be created (a read-only directory, a "<path>.lock" name already
// taken by a directory) or cannot be acquired is not a reason to refuse the
// write, but it is a reason the write may overwrite another command's — forty
// concurrent registrations over an unplaceable lock left one row and printed
// nothing, which is the lost update this file exists to prevent arriving
// silently (2026-09-27 audit, round 27, B-B).
//
// Do not nest: flock treats a second open of the same path as a different
// holder, so an inner WithFileLock on the same path would deadlock against
// the outer one. Callers take the lock at the level that owns the whole
// load-mutate-save.
func WithFileLock(path string, fn func() error) error {
	if path == "" {
		return fn()
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		// A lock we cannot even create is not a reason to refuse the write:
		// the store is still written atomically, just without exclusion.
		warnUnlocked(path, err)
		return fn()
	}
	defer f.Close()

	if err := lockFile(f); err != nil {
		warnUnlocked(path, err)
		return fn()
	}
	defer unlockFile(f)

	return fn()
}

// warnUnlocked says out loud that a write is proceeding without exclusion.
// Stderr, not a log file: this is a command the user is running, and the point
// is that they can see the failure to lock at the moment it happens.
func warnUnlocked(path string, err error) {
	fmt.Fprintf(os.Stderr, "Warning: could not lock %s (%v); continuing without the cross-process lock, so a concurrent oaica command could overwrite this change\n", path, err)
}
