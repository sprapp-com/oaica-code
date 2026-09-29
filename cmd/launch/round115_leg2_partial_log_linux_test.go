// round115_leg2_partial_log_linux_test.go — F115-L2-1 (2026-09-29 audit, round 115): a row cut short
// by a full disk must not swallow the next healthy row. Linux-only: RLIMIT_FSIZE fault injection.

package launch

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestRound115PartialLogRowDoesNotFuseTheNext(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	path, _ := requestLogPath()
	signal.Ignore(syscall.SIGXFSZ)
	var old syscall.Rlimit
	syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old)
	// room for part of one row only (disk-full stand-in)
	lim := syscall.Rlimit{Cur: 40, Max: old.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Fatal(err)
	}
	appendRequestLog(requestLogEntry{Timestamp: time.Now().UTC().Format(time.RFC3339), Model: "first", Backend: "zai", StatusCode: 200})
	syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old) // disk freed
	appendRequestLog(requestLogEntry{Timestamp: time.Now().UTC().Format(time.RFC3339), Model: "second", Backend: "zai", StatusCode: 200})
	appendRequestLog(requestLogEntry{Timestamp: time.Now().UTC().Format(time.RFC3339), Model: "third", Backend: "zai", StatusCode: 200})
	b, _ := os.ReadFile(path)
	t.Logf("log bytes:\n%s", b)
	rows, unreadable, err := LoadUsageStatsCountingUnreadable(UsageStatsFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, r := range rows {
		got[r.Model] += r.Requests
	}
	t.Logf("rows=%v unreadable=%d", got, unreadable)
	if got["second"] != 1 || got["third"] != 1 {
		t.Fatalf("a row written AFTER the disk recovered was lost: %v (unreadable %d)", got, unreadable)
	}
}
