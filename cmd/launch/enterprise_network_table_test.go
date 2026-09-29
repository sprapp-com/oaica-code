package launch

// enterprise_network_table_test.go — regression pins for the document's own
// claims about what the client sends where: the behavioral half of each test
// drives the real code path with a recording transport (no socket ever opens),
// and the doc half pins the row in docs/ENTERPRISE.md's network table that has
// to disclose it. The pairing is the point — a destination the table omits is
// exactly the claim ("Nothing else in the client opens a socket") the client
// must not be able to break, and an omission cannot be observed behaviorally.
//
// Written as failing reproductions during the 2026-09-26 audit; the table rows
// were then corrected, and each test now pins the corrected claim. Two assert
// the FIXED direction of a premise the audit recorded as broken and say so.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Recording transport: answers every request locally (no real socket ever
// opens) and remembers what was asked for. Hermetic by construction.
// ---------------------------------------------------------------------------

type auditR3DTransport struct {
	mu      sync.Mutex
	visited []string // "METHOD host/uri"
	auths   []string
	respond func(req *http.Request) *http.Response
}

func (a *auditR3DTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	a.mu.Lock()
	a.visited = append(a.visited, req.Method+" "+req.URL.Host+req.URL.RequestURI())
	a.auths = append(a.auths, req.Header.Get("Authorization"))
	a.mu.Unlock()
	return a.respond(req), nil
}

func (a *auditR3DTransport) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.visited...)
}

func (a *auditR3DTransport) authsSeen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auths...)
}

func (a *auditR3DTransport) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.visited = nil
	a.auths = nil
}

func (a *auditR3DTransport) sawHost(host string) bool {
	for _, v := range a.seen() {
		if strings.Contains(v, " "+host) {
			return true
		}
	}
	return false
}

func auditR3DResponse(req *http.Request, status int, body string) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d status", status),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func auditR3DSwapTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = old })
}

// ---------------------------------------------------------------------------
// PIN 1
// ---------------------------------------------------------------------------

// CLAIM — docs/ENTERPRISE.md:36, network table row 6:
//
//	| 6 | `registry.npmjs.org` | Only when you run `oaica launch pi`, and you
//	confirm the prompt | A GET for the `@ollama/pi-web-search` package version,
//	and the `pi install`/`pi update` it then runs | Decline the prompt, or set
//	`PI_OFFLINE=1`. ... |
//
// The row's "When" and "Off switch" columns tell a reviewer that npm is
// reachable only through the pi integration, and that declining that one
// prompt — or setting PI_OFFLINE=1 — keeps npm out of the environment.
//
// The table's row 5 enumerates the agent-installer hosts and does not include
// registry.npmjs.org, even though row 5's own prose names `dsh` among the
// agents it covers.
//
// CODE — three other `oaica launch <agent>` paths shell out to
// `npm install -g ...` after their own prompt:
//
//	cmd/launch/cline.go:60         exec.Command("npm", "install", "-g", "cline@latest")
//	cmd/launch/openclaw.go:628     exec.Command("npm", "install", "-g", "openclaw@latest")
//	cmd/launch/deepseek_harness.go:114  deepSeekHarnessNpmCommand(npm, {"install","-g","@deepseek-ai/dsh@latest"})
//
// None of the three is gated on PI_OFFLINE, and none of them is pi.
func TestNpmInstallIsNotPiOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub installer is a POSIX shell script")
	}

	cases := []struct {
		agent string
		// pre = binaries the installer checks for BEFORE its prompt; they must
		// exist up front or the installer bails before reaching npm.
		pre []string
		// after = binaries the installer checks for AFTER npm runs; the stub
		// npm creates them so the installer reports success.
		after []string
		run   func() error
	}{
		{
			agent: "cline",
			after: []string{"cline"},
			run: func() error {
				_, err := ensureClineInstalled()
				return err
			},
		},
		{
			agent: "openclaw",
			pre:   []string{"git"},
			after: []string{"openclaw"},
			run: func() error {
				_, err := ensureOpenclawInstalled()
				return err
			},
		},
		{
			agent: "dsh",
			after: []string{"dsh"},
			run: func() error {
				_, err := ensureDeepSeekHarnessInstalled()
				return err
			},
		},
	}

	reached := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			dir := t.TempDir()
			setTestHome(t, dir)
			t.Setenv("PATH", dir)
			// The row's off switch, set in the "safe" position: if npm still
			// runs for a non-pi agent, PI_OFFLINE is not the switch row 6 says
			// it is.
			t.Setenv("PI_OFFLINE", "1")

			writeStub := func(name string) {
				p := filepath.Join(dir, name)
				body := "#!/bin/sh\nexit 0\n"
				if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, b := range tc.pre {
				writeStub(b)
			}

			create := ""
			for _, b := range tc.after {
				p := filepath.Join(dir, b)
				create += fmt.Sprintf(
					"/bin/cat > %q <<'EOF'\n#!/bin/sh\nexit 0\nEOF\n/bin/chmod +x %q\n", p, p)
			}
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HOME/npm-calls.log\"\n" + create + "exit 0\n"
			if err := os.WriteFile(filepath.Join(dir, "npm"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			oldConfirm := DefaultConfirmPrompt
			DefaultConfirmPrompt = func(string, ConfirmOptions) (bool, error) { return true, nil }
			t.Cleanup(func() { DefaultConfirmPrompt = oldConfirm })

			if err := tc.run(); err != nil {
				t.Fatalf("installer for %s did not get as far as npm: %v", tc.agent, err)
			}

			raw, err := os.ReadFile(filepath.Join(dir, "npm-calls.log"))
			if err != nil {
				t.Fatalf("`oaica launch %s` never invoked npm: %v", tc.agent, err)
			}
			got := strings.TrimSpace(string(raw))
			if !strings.Contains(got, "install -g") {
				t.Fatalf("npm argv for `oaica launch %s` = %q", tc.agent, got)
			}
			t.Logf("`oaica launch %s` ran npm %q with PI_OFFLINE=1 set", tc.agent, got)
			reached[tc.agent] = got
		})
	}

	// Doc half. The claim is about which agent reaches registry.npmjs.org, so
	// only the doc text can be pinned — the behavioral half just proved the
	// sockets. Row 6 still scopes npm to pi; if it ever stops doing that, this
	// row stops scoping npm to pi at all, the guard below says so instead.
	table := auditR3DEnterpriseNetworkTable(t)
	piRow := ""
	for _, line := range strings.Split(table, "\n") {
		if strings.Contains(line, "registry.npmjs.org") {
			piRow = line
		}
	}
	if piRow == "" {
		t.Fatal("the network table no longer mentions registry.npmjs.org at all")
	}
	// FIXED (inverted here): the row now scopes npm to every agent that ships
	// on npm, not to `pi` alone — and the behavioral half above proved all four
	// really do reach the registry (with PI_OFFLINE=1 set, which no longer
	// claims to stop them).
	if len(reached) != len(cases) {
		t.Fatalf("only %d of %d non-pi agents reached npm: %v", len(reached), len(cases), reached)
	}
	for _, agent := range []string{"`pi`", "`cline`", "`openclaw`", "`dsh`"} {
		if !strings.Contains(piRow, agent) {
			t.Errorf("row 6 still does not name %s, but `oaica launch %s` reached npm in the run above:\n%s",
				agent, strings.Trim(agent, "`"), piRow)
		}
	}
	for _, pkg := range []string{"cline@latest", "openclaw@latest", "dsh@latest"} {
		if !strings.Contains(piRow, pkg) {
			t.Errorf("row 6 does not name the package %q that this run installed", pkg)
		}
	}
}

// ---------------------------------------------------------------------------
// PIN 2
// ---------------------------------------------------------------------------

// CLAIM — docs/ENTERPRISE.md:25-27:
//
//	"Eight paths the client itself opens. Nothing else in the client opens a
//	 socket."
//
// and docs/ENTERPRISE.md:68-70 (Telemetry):
//
//	"Two outbound requests happen without you asking, and neither carries a
//	 payload: row 3's version GET, and row 7's licence revalidation ..."
//
// The table names `ollama.com` nowhere: row 2 is `oaica.com` (the install
// script host), row 3 is the GitHub release VERSION.txt, row 7 is
// the oaica-saas licence API. `ollama.com` is a different host from `oaica.com`.
//
// CODE — cmd/launch/ollama_cloud.go:35
//
//	ollamaCloudSearchURL = "https://ollama.com/search?c=cloud"
//
// is fetched by ollamaCloudModelIDsUncached (:104-105,
// `client.Get(ollamaCloudSearchURL)`) on a cache miss; ollamaCloudEntries
// (:140-153) wraps it and the package wires it in by default
// (`var ollamaCloudEntriesFn = ollamaCloudEntries`, :138). The picker
// inventory appends it at cmd/launch/oaica_models.go:457-459 inside
// oaicaLiveModelEntriesErr, which cmd/launch/model_inventory.go:184 calls on
// every `oaica launch` (and cmd/launch/model_refresh_cli.go:72 on `oaica model
// refresh`). It is skipped only when OAICA_HOST is pinned, so for the default
// user it is unprompted, has no off switch, and is not in the table.
func TestOllamaComScrapeIsASocketTheTableOmits(t *testing.T) {
	// setLaunchTestHome nils ollamaCloudEntriesFn (launch_test.go:210) and a
	// sibling test may have run before this one, so put the package-level
	// production wiring back explicitly. `var ollamaCloudEntriesFn =
	// ollamaCloudEntries` (ollama_cloud.go:138) is exactly this value, so
	// nothing about the reachability claim is weakened by restoring it.
	prevEntries := ollamaCloudEntriesFn
	prevFetch := oaicaFetchCloudModelEntries
	ollamaCloudEntriesFn = ollamaCloudEntries
	oaicaFetchCloudModelEntries = oaicaFetchCloudModelEntriesLive
	t.Cleanup(func() {
		ollamaCloudEntriesFn = prevEntries
		oaicaFetchCloudModelEntries = prevFetch
	})

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TMPDIR", home)
	t.Setenv("OAICA_HOST", "") // the default, unpinned install
	t.Setenv("OAICA_API_KEY", "")
	t.Setenv("OAICA_REMOTES_FILE", filepath.Join(home, "no-remotes.json"))

	rt := &auditR3DTransport{respond: func(req *http.Request) *http.Response {
		if req.URL.Host == "ollama.com" {
			return auditR3DResponse(req, 200,
				`<html><body><a href="/library/gpt-oss">gpt-oss</a></body></html>`)
		}
		// A healthy router. This matters: oaicaLiveModelEntriesErr returns
		// early (oaica_models.go:454-456) when the router gave nothing and
		// errored, so the ollama.com scrape below runs on the NORMAL path —
		// router reachable and answering.
		return auditR3DResponse(req, 200,
			`{"data":[{"id":"oaica-kat","description":"d","stars":1,"status":"ready"}]}`)
	}}
	auditR3DSwapTransport(t, rt)

	// 1) The fetch itself.
	ids, err := ollamaCloudModelIDsErr()
	if err != nil {
		t.Fatalf("ollamaCloudModelIDsErr() = %v, want the scraped ids", err)
	}
	if len(ids) == 0 {
		t.Fatal("no ids parsed from the stubbed ollama.com/search page")
	}
	if !rt.sawHost("ollama.com") {
		t.Fatalf("no request to ollama.com; recorded %v", rt.seen())
	}
	t.Logf("catalog fetch egress: %v", rt.seen())

	// 2) Reachability from the picker inventory path — this is what
	// `oaica launch` runs, with no prompt and no off switch. Drop the cache
	// step 1 just wrote so this is a cold fetch, the way a first launch and
	// every launch after the 1h TTL is.
	if cachePath, perr := ollamaCloudCachePath(); perr == nil {
		_ = os.Remove(cachePath)
	}
	rt.reset()
	if _, _ = oaicaLiveModelEntriesErr(); !rt.sawHost("ollama.com") {
		t.Fatalf("the picker inventory path did not reach ollama.com; recorded %v", rt.seen())
	}
	t.Logf("`oaica launch` inventory egress: %v", rt.seen())

	// Doc half: the claim is about the *set* of destinations the doc admits
	// to, so an omission can only be pinned as text. The behavioral half above
	// is what proves the socket is real.
	table := auditR3DEnterpriseNetworkTable(t)
	if !strings.Contains(table, "oaica.com") {
		t.Fatal("table shape changed; this guard is no longer anchored on row 2")
	}
	if !strings.Contains(table, "ollama.com") {
		t.Fatalf("docs/ENTERPRISE.md's network table never names ollama.com, yet the client opens a socket "+
			"to it on every `oaica launch` with OAICA_HOST unset (recorded above: %v):\n%s", rt.seen(), table)
	}
}

// ---------------------------------------------------------------------------
// PIN 3
// ---------------------------------------------------------------------------

// CLAIM — docs/ENTERPRISE.md:90 (files-on-disk table, requests.log row):
//
//	| `~/.oaica/requests.log` | ... model name, which backend served it (a
//	label, or the endpoint URL with any credential redacted), message *sizes*,
//	timing, status | No — sizes, not content |
//
// and :122-126:
//
//	"A credential must not reach a log, a cache, a support bundle, or a
//	 process argument list. Concretely, the client: - never writes an API key
//	 into `requests.log`, the picker cache, the catalog caches, or `doctor`
//	 output;"
//
// and :99 ("never message text, headers, or credentials"), and
// docs/CLAUDE_TIERS.md:127 ("model, backend label + redacted URL, sizes,
// status — never content").
//
// CODE — cmd/launch/request_log.go:185 writes
//
//	Backend: redactCredentials(targetBaseURL),
//
// and redactCredentials (cmd/launch/redact.go:143-149) rewrites URL *userinfo*
// only; the query-string credential redactor redactQueryCredentials is reached
// only from redactBaseURL and redactErr (redact.go:155-161, :260-283), neither
// of which is on this line. The same line shape recurs at
// cmd/launch/anthropic_openai_proxy.go:1406 for the routed proxy. A key *does*
// ride in a base URL's query string — redact.go:90-138
// (querySecrets/credentialQueryParam) and :203-223 (baseURLSecrets) exist
// precisely because it does.
//
// (Round 2 fixed this shape for `doctor --report` and for the three sync
// fetchers; `git show --stat 214fc6d7` lists neither request_log.go nor a
// change to the Backend field, so this site was left behind.)
func TestRequestsLogKeepsAQueryCredential(t *testing.T) {
	setTestHome(t, t.TempDir())

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer up.Close()

	// A base URL whose credential is a query parameter, not userinfo.
	target := up.URL + "/v1?api_key=" + redactKey

	// Premise check: the base-URL redactor does remove this, so the leak below
	// is a call-site choice, not a missing capability.
	if redacted := redactBaseURL(target); strings.Contains(redacted, redactKey) {
		t.Fatalf("premise wrong: redactBaseURL(%q) = %q", target, redacted)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunLocalLoggingProxy(ln, target) }()
	defer ln.Close()

	body := `{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status %d", resp.StatusCode)
	}

	path, err := RequestLogPath()
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	deadline := time.Now().Add(3 * time.Second)
	for {
		raw, err = os.ReadFile(path)
		if err == nil && strings.Contains(string(raw), `"backend"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no row appeared in %s: %v (%q)", path, err, raw)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if strings.Contains(string(raw), redactKey) {
		t.Fatalf("requests.log kept the query-string credential, though the claim is \"the endpoint URL with "+
			"any credential redacted\" and \"never writes an API key into `requests.log`\":\n%s", raw)
	}
}

// ---------------------------------------------------------------------------
// PIN 4
// ---------------------------------------------------------------------------

// CLAIM — docs/ENTERPRISE.md:68-70:
//
//	"Two outbound requests happen without you asking, and neither carries a
//	 payload: row 3's version GET, and row 7's licence revalidation for an
//	 install that is already activated"
//
// together with :25 ("Nothing else in the client opens a socket") and :99
// ("never message text, headers, or credentials"). `ollama.com` is named
// nowhere in the table.
//
// CODE — cmd/launch/claude_desktop.go:26
//
//	claudeDesktopGatewayBaseURL = "https://ollama.com"
//
// and validateClaudeDesktopAPIKey (:531-546) does
// `GET https://ollama.com/v1/models` with `Authorization: Bearer <the user's
// OLLAMA_API_KEY>` — a credential transmitted to a host the network table does
// not list. Reachable from `oaica launch claude-desktop` on the platforms the
// integration supports (claude_desktop.go:169-176: darwin and windows — the
// shipped release binaries, not this Linux test host, which is why the test
// calls the validator directly rather than driving the subcommand).
func TestOllamaAPIKeyBearerReachesOllamaCom(t *testing.T) {
	const key = "ollama-r3d-secret-key"

	// claudeDesktopHTTPClient is a package var initialized to http.DefaultClient
	// (claude_desktop.go:47); restore that, since a sibling test may have
	// replaced it.
	oldClient := claudeDesktopHTTPClient
	claudeDesktopHTTPClient = http.DefaultClient
	t.Cleanup(func() { claudeDesktopHTTPClient = oldClient })

	rt := &auditR3DTransport{respond: func(req *http.Request) *http.Response {
		return auditR3DResponse(req, 200, `{"data":[]}`)
	}}
	auditR3DSwapTransport(t, rt)

	if err := validateClaudeDesktopAPIKey(context.Background(), key); err != nil {
		t.Fatalf("validateClaudeDesktopAPIKey() = %v, want nil against a 200", err)
	}

	if !rt.sawHost("ollama.com") {
		t.Fatalf("no request to ollama.com; recorded %v", rt.seen())
	}
	carried := false
	for _, a := range rt.authsSeen() {
		if a == "Bearer "+key {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("no Bearer credential was transmitted; recorded auth headers %v", rt.authsSeen())
	}
	t.Logf("egress %v carried %q", rt.seen(), "Bearer "+key)

	table := auditR3DEnterpriseNetworkTable(t)
	if !strings.Contains(table, "ollama.com") {
		t.Fatalf("docs/ENTERPRISE.md's network table never names ollama.com, yet the client sends an "+
			"OLLAMA_API_KEY to it as a bearer header (recorded above: %v, %q):\n%s",
			rt.seen(), "Bearer "+key, table)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// auditR3DEnterpriseNetworkTable returns the text of the "Network connections"
// table in docs/ENTERPRISE.md — header row through the last row. Used only for
// the two findings whose claim is about the *set* of destinations the doc
// admits to, which no behavioral test can pin; the behavioral half of each of
// those findings sits above it.
func auditR3DEnterpriseNetworkTable(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "ENTERPRISE.md"))
	if err != nil {
		t.Fatalf("read docs/ENTERPRISE.md: %v", err)
	}
	doc := string(raw)
	start := strings.Index(doc, "| # | Destination |")
	if start < 0 {
		t.Fatal("docs/ENTERPRISE.md no longer has the network table header")
	}
	end := strings.Index(doc[start:], "Row 5 exists because")
	if end < 0 {
		t.Fatal("docs/ENTERPRISE.md network table has no trailing prose anchor")
	}
	return doc[start : start+end]
}
