//go:build !unix && !windows

package fileutil

import "os"

// No flock and no LockFileEx on this platform (js/wasm, plan9, …). The store is
// still written atomically (WriteFileAtomic), so a reader never sees a partial
// file; only the exclusion between two simultaneous writers is missing here.
//
// Windows is NOT in this file — it has its own implementation, because
// oaica-windows-amd64 is a release artifact and a silent no-op there would ship
// the lost update (2026-09-26 audit). See lock_windows.go.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) {}
