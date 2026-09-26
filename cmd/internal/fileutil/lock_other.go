//go:build !unix

package fileutil

import "os"

// No flock on this platform. The store is still written atomically
// (WriteFileAtomic), so a reader never sees a partial file; only the
// exclusion between two simultaneous writers is missing here.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) {}
