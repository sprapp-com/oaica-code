package launch

// doctor_test.go — `oaica doctor`'s exit contract: it is the command CI and
// cron are told to gate on, so a FAILing probe MUST set a non-zero exit even
// though every line of output still prints.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// The URL doctor prints beside "ok" is the URL a user curls while debugging,
// so it has to be the endpoint doctor actually probed. It used to print the
// configured base_url, which on a row with a version segment is a truncated
// PREFIX of the real endpoint — a v4 row showed ".../api/paas" next to an "ok"
// earned at ".../api/paas/v4/models", and following the printed link by hand
// answers 404.
func TestDoctor_PrintsTheEndpointItProbes(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var probedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	// A v4 row: the version belongs to the endpoint, not to the configured
	// base, so the two differ by exactly the segment under test.
	writeRemotes(t, fmt.Sprintf(
		`{"remotes":[{"name":"vfour","base_url":%q,"version":"v4","api_key":"sk-1"}]}`, srv.URL+"/api/paas"))

	var err error
	out := captureDoctorStdout(t, func() { err = DoctorCmd().RunE(DoctorCmd(), nil) })
	if err != nil {
		t.Fatalf("a remote the test server answers must pass: %v\n%s", err, out)
	}
	if probedPath != "/api/paas/v4/models" {
		t.Fatalf("the probe hit %q, want the resolved /api/paas/v4/models", probedPath)
	}

	printed := ""
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "vfour" {
			printed = f[1]
		}
	}
	if printed == "" {
		t.Fatalf("doctor printed no line for vfour:\n%s", out)
	}
	if want := srv.URL + "/api/paas/v4"; printed != want {
		t.Errorf("doctor prints %q, want the endpoint it probed (%s/models)", printed, want)
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

// anthropicWireModels answers like an Anthropic endpoint: it refuses a Bearer,
// and needs the version header alongside the key.
func anthropicWireModels(gotHeader *http.Header) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotHeader != nil {
			*gotHeader = r.Header.Clone()
		}
		if r.Header.Get("x-api-key") != "sk-anthropic-key" || r.Header.Get("anthropic-version") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5"}]}`))
	}))
}

// probeRemote asks the same /v1/models URL the picker does, so it must
// authenticate the same way. It sent a Bearer unconditionally, which an
// anthropic-wire remote answers with 401 — and since the probe became an exit-1
// gate (TestDoctor_FailingProbeFailsTheCommand), a healthy zai-coding-plan or
// minimax-coding-plan made `oaica doctor` itself fail (2026-09-26 audit).
func TestProbeRemote_AnthropicWireUsesXAPIKey(t *testing.T) {
	var header http.Header
	srv := anthropicWireModels(&header)
	defer srv.Close()

	status := probeRemote(userRemote{
		Name:    "zai-coding-plan",
		BaseURL: srv.URL,
		APIKey:  "sk-anthropic-key",
		Wire:    "anthropic",
	})
	if status != "ok" {
		t.Fatalf("probeRemote on a healthy anthropic-wire remote = %q, want ok", status)
	}
	if header.Get("x-api-key") != "sk-anthropic-key" {
		t.Errorf("probe did not send x-api-key: %v", header)
	}
	if header.Get("Authorization") != "" {
		t.Errorf("probe sent a Bearer to an anthropic-wire remote: %v", header)
	}
}

// The same endpoint probed as openai-wire must NOT be quietly accepted: the
// Bearer branch is a real difference, not a fallback that happens to work, and
// the wire field is what selects it.
func TestProbeRemote_OpenAIWireSendsBearerOnly(t *testing.T) {
	var header http.Header
	srv := anthropicWireModels(&header)
	defer srv.Close()

	status := probeRemote(userRemote{Name: "zai", BaseURL: srv.URL, APIKey: "sk-anthropic-key"})
	if status != "FAIL http 401" {
		t.Fatalf("openai-wire probe against an anthropic endpoint = %q, want FAIL http 401", status)
	}
	if header.Get("Authorization") != "Bearer sk-anthropic-key" {
		t.Errorf("probe did not send the Bearer: %v", header)
	}
}
