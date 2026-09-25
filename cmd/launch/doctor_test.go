package launch

// doctor_test.go — `oaica doctor`'s exit contract: it is the command CI and
// cron are told to gate on, so a FAILing probe MUST set a non-zero exit even
// though every line of output still prints.

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// captureDoctorStdout captures fmt.Println output (doctor writes to stdout,
// unlike the launch path's os.Stderr).
func captureDoctorStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

// A dead remote used to print "FAIL ..." and then "all checks passed", exit
// 0 — the header comment and docs/CLAUDE_TIERS.md both promised exit 1, and
// only a malformed remotes.json ever set `failed` (2026-09-26 audit).
func TestDoctor_FailingProbeFailsTheCommand(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"dead","base_url":"http://127.0.0.1:1/v1","api_key":"k","tool_format":"tool_calls"}]}`)

	var err error
	out := captureDoctorStdout(t, func() { err = DoctorCmd().RunE(DoctorCmd(), nil) })
	if !strings.Contains(out, "FAIL") {
		t.Fatalf("the dead remote should print FAIL:\n%s", out)
	}
	if err == nil {
		t.Errorf("a FAILing probe must fail the command (scripts grep the exit code):\n%s", out)
	}
	if !strings.Contains(out, "weighted") {
		t.Errorf("the policies line must name every accepted value:\n%s", out)
	}
}

// A healthy setup must still exit 0 and say so.
func TestDoctor_NoRemotesPasses(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	stubDaemon(t)
	var err error
	out := captureDoctorStdout(t, func() { err = DoctorCmd().RunE(DoctorCmd(), nil) })
	if err != nil {
		t.Fatalf("no remotes + a reachable daemon is a passing setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "all checks passed") {
		t.Errorf("want the all-clear line:\n%s", out)
	}
}

// A malformed route_policy is invalid even when the probe answers — and it
// must not be reported as reachable-and-fine.
func TestDoctor_InvalidPolicyFails(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://127.0.0.1:1/v1","api_key":"k","tool_format":"tool_calls","route_policy":"nonsense"}]}`)
	var err error
	out := captureDoctorStdout(t, func() { err = DoctorCmd().RunE(DoctorCmd(), nil) })
	if err == nil || !strings.Contains(out, "INVALID route_policy") {
		t.Fatalf("err=%v out:\n%s", err, out)
	}
}
