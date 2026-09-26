package fileutil

// lock_platform_integrity_test.go — Windows took the no-op lock (2026-09-26
// audit).
//
// lockFile/unlockFile are the platform half of WithFileLock. Only unix had an
// implementation; lock_other.go was tagged `!unix`, so Windows — a release
// artifact, oaica-windows-amd64.zip — compiled the do-nothing pair and every
// store under ~/.oaica lost updates there exactly as the unix side did before
// its flock: eight concurrent `oaica auth login` reported success and left one
// credential. Nothing failed, on any platform, because a no-op lock is
// indistinguishable from a lock nobody contended.
//
// Windows now has lock_windows.go (LockFileEx). This test runs everywhere and
// pins the arrangement that made the defect possible, because the behaviour it
// guards can only be exercised on Windows itself — the executable assertions
// live in lock_integrity_test.go, which spawns real processes and is
// platform-neutral by construction.

import (
	"os"
	"strings"
	"testing"
)

func lockSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("no %s — re-read the platform lock files before trusting this test: %v", name, err)
	}
	return string(b)
}

// The no-op must not claim Windows.
func TestTheNoOpLockDoesNotCoverWindows(t *testing.T) {
	src := lockSource(t, "lock_other.go")
	tag := src[:strings.Index(src, "\n\n")]
	if !strings.Contains(tag, "!windows") {
		t.Errorf("lock_other.go (the do-nothing lockFile) is built for Windows again: its build constraint is %q. oaica-windows-amd64 is a shipped artifact, so this ships the lost update — every concurrent `oaica auth login` reports success and one credential survives", tag)
	}
}

// And Windows must have a real one.
func TestWindowsHasARealLock(t *testing.T) {
	src := lockSource(t, "lock_windows.go")
	tag := src[:strings.Index(src, "\n\n")]
	if !strings.Contains(tag, "go:build windows") {
		t.Errorf("lock_windows.go's build constraint is %q, want windows", tag)
	}
	if !strings.Contains(src, "LockFileEx") {
		t.Error("lock_windows.go does not call LockFileEx — it is not an exclusive cross-process lock")
	}
	for _, want := range []string{"func lockFile(", "func unlockFile("} {
		if !strings.Contains(src, want) {
			t.Errorf("lock_windows.go does not define %s", want)
		}
	}
	if strings.Contains(src, "func lockFile(*os.File) error { return nil }") {
		t.Error("lock_windows.go's lockFile is the no-op")
	}
	// Blocking, not fail-fast: a caller that cannot wait is the caller that
	// loses the update. Comments are stripped first — the file explains why it
	// does not use this flag, by name.
	if strings.Contains(codeOnly(src), "LOCKFILE_FAIL_IMMEDIATELY") {
		t.Errorf("lock_windows.go asks for LOCKFILE_FAIL_IMMEDIATELY — WithFileLock treats a lock failure as permission to write unlocked (lock.go), so a non-blocking lock turns contention into exactly the lost update it exists to prevent")
	}
}

// codeOnly drops comment lines, so a pin on an identifier the file's prose
// names (and explains avoiding) scans the code and not the explanation.
func codeOnly(src string) string {
	var out []string
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// The unix half keeps flock, and the two platform files describe the same pair
// of functions — a mismatch is a build failure on that platform and nothing
// here would say which file fell behind.
func TestBothPlatformsDefineTheSamePair(t *testing.T) {
	for _, name := range []string{"lock_unix.go", "lock_windows.go"} {
		src := lockSource(t, name)
		for _, want := range []string{"func lockFile(f *os.File) error", "func unlockFile(f *os.File)"} {
			if !strings.Contains(src, want) {
				t.Errorf("%s does not define %q", name, want)
			}
		}
	}
	// lock.go keeps one call site, unchanged in shape: the release-blocking
	// failure mode is a platform that quietly stops being called at all.
	lock := lockSource(t, "lock.go")
	for _, want := range []string{"if err := lockFile(f); err != nil {", "defer unlockFile(f)"} {
		if !strings.Contains(lock, want) {
			t.Errorf("lock.go no longer contains %q", want)
		}
	}
}
