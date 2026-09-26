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

import (
	"os"
	"path/filepath"
)

// WithFileLock runs fn while holding an exclusive cross-process lock covering
// path, creating path's directory if needed. The lock is released even if fn
// panics. A caller whose path cannot be resolved (no home directory) gets fn
// run unlocked rather than an error — the alternative is refusing to save a
// credential the user just typed.
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
		return fn()
	}
	defer f.Close()

	if err := lockFile(f); err != nil {
		return fn()
	}
	defer unlockFile(f)

	return fn()
}
