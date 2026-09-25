package launch

// local_serve_registry_test.go — pins the readers of ~/.oaica/local_servers.json,
// the registry `oaica serve` writes (round-4 audit, 2026-09-26).
//
// Three defects lived here, all three invisible from the command line:
//
//   1. oaicaLocalServerEntries' /health probe sent no Authorization header, so
//      `oaica serve <model> --host 0.0.0.0 --api-key K` — the off-loopback
//      form ServeHandler's own refusal message recommends — answered 401 and
//      was dropped as dead although it was up and reachable.
//   2. The recorded pid was written by every serve and read by nothing, so an
//      entry whose process had died kept its row as long as SOMEONE answered
//      200 on its old port.
//   3. ResolveAgentModel resolved a "<model>:local" tag to the local origin
//      but paired it with the ROUTER's credential, which the local proxy
//      rejects with 401 — `oaica agent` could not talk to a keyed serve while
//      `oaica launch` could.
//
// Every server here is the real launch.RunNormalizingProxyOn on a loopback
// port; the registry is a temp HOME. Nothing leaves the machine.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// invServeBackend is a llama-server stand-in for one model: /health 200 (its
// own liveness endpoint) and a /v1/chat/completions that reports WHICH model
// it loaded, so a misroute shows in the body instead of being inferred.
func invServeBackend(t *testing.T, servedModel string) (port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"role":"assistant","content":"ok"}}]}`, servedModel)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("backend url: %v", err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("backend port: %v", err)
	}
	return p
}

// invServeStart starts the real serve-side proxy (RunNormalizingProxyOn, the
// same call cmd/oaica_pull_serve.go makes) and waits until it listens.
func invServeStart(t *testing.T, bindHost, apiKey string, backendPort int) (port int) {
	t.Helper()
	ln, err := net.Listen("tcp", bindHost+":0")
	if err != nil {
		t.Fatalf("cannot reserve a port on %s: %v", bindHost, err)
	}
	port = ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close reserved port: %v", err)
	}
	go func() { _ = RunNormalizingProxyOn(bindHost, port, backendPort, apiKey) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return port
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the proxy on %s:%d never started listening", bindHost, port)
	return 0
}

// invServeGet is the request oaicaLocalServerEntries sends.
func invServeGet(t *testing.T, rawURL, bearer string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, strings.TrimSpace(string(buf[:n]))
}

func invServeSetup(t *testing.T) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[]}`)
	stubBareIndex(t, map[string][]string{})
	stubUserRemoteModels(t, nil, nil)
	stubCloudFetch(t, []oaicaModelEntry{{ID: "cloud-a"}}, nil)
	stubDaemon(t)
	t.Setenv("OAICA_API_KEY", "")
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")
}

// A keyed serve on a network-facing bind is discovered by the picker. The
// registry entry names an origin on 127.0.0.1 (that is where the proxy is
// reachable from this machine), whose /health is gated on the bearer for a
// non-loopback bind — so the probe has to present the registered key.
func TestLocalServeRegistryProbesAKeyedServerWithItsKey(t *testing.T) {
	invServeSetup(t)
	const key = "sk-serve-lan-key"
	backendPort := invServeBackend(t, "bonsai")

	lanPort := invServeStart(t, "0.0.0.0", key, backendPort)
	origin := fmt.Sprintf("http://127.0.0.1:%d", lanPort)

	// The mechanism, observed: authenticated 200, unauthenticated 401.
	if code, body := invServeGet(t, origin+"/health", key); code != 200 {
		t.Fatalf("the serve is not live: authenticated GET /health = %d %q", code, body)
	}
	if code, _ := invServeGet(t, origin+"/health", ""); code != 401 {
		t.Fatalf("premise changed: unauthenticated GET /health = %d, want 401 (that is what the probe sends when keyless)", code)
	}

	writeLocalRegistry(t, []oaicaLocalServersRegistryEntry{{
		Model: "bonsai", Origin: origin, PID: os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339), APIKey: key,
	}})

	if live := oaicaLocalServerEntries(); len(live) != 1 {
		code, body := invServeGet(t, origin+"/health", "")
		t.Errorf("`oaica serve bonsai --host 0.0.0.0 --api-key <key>` is running and answers /health 200 to an "+
			"authenticated caller, but oaicaLocalServerEntries returned %d live entries, not 1: its probe sent no "+
			"Authorization header, the proxy gates /health on the bearer for every non-loopback bind, so the probe "+
			"saw %d %q and the server was treated as dead.\norigin=%s", len(live), code, body, origin)
	}
	// The two consumers of that verdict, end to end.
	if host := oaicaResolveHostForModel("bonsai:local"); host != origin {
		t.Errorf("oaicaResolveHostForModel(\"bonsai:local\") = %q, want the local origin %q — a :local tag forces "+
			"local, but the entry was filtered out and the resolver fell through to the CLOUD host", host, origin)
	}
	if _, err := resolveLaunchEndpoint("bonsai:local"); err != nil {
		t.Errorf("resolveLaunchEndpoint(\"bonsai:local\") failed for a running, healthy serve: %v", err)
	}
}

// The pid every serve records is a liveness signal: an entry whose process is
// gone must not keep its row. The port it named may since have been taken over
// — by another serve or by any local HTTP server answering 200 on /health —
// and the row would then route the dead model's name to whatever answers.
func TestLocalServeRegistryDropsAnEntryWhoseProcessIsGone(t *testing.T) {
	invServeSetup(t)

	// A live serve of a DIFFERENT model on the port the entry names.
	mapleBackend := invServeBackend(t, "maple")
	takenPort := invServeStart(t, "127.0.0.1", "", mapleBackend)
	origin := fmt.Sprintf("http://127.0.0.1:%d", takenPort)

	// The stale entry: `oaica serve bonsai` died on this port, no cleanup ran.
	// 1<<30-1 exceeds /proc/sys/kernel/pid_max, i.e. no process can hold it.
	writeLocalRegistry(t, []oaicaLocalServersRegistryEntry{{
		Model: "bonsai", Origin: origin, PID: 1<<30 - 1,
		StartedAt: "2020-01-01T00:00:00Z",
	}})

	if live := oaicaLocalServerEntries(); len(live) != 0 {
		t.Errorf("an entry whose recorded pid (%d) cannot name a live process is still reported live: %+v. Its "+
			"origin answers /health 200 — because a DIFFERENT serve took the port over — so the row for the dead "+
			"model now resolves to that server's weights.", live[0].PID, live)
	}
	if _, err := resolveLaunchEndpoint("bonsai:local"); err == nil {
		t.Error("resolveLaunchEndpoint(\"bonsai:local\") succeeded for a server that is gone; the request would have " +
			"been answered by whichever process holds the port")
	}
}

// `oaica agent --model <m>:local` must send the key the serve registered — the
// local proxy 401s any other bearer — not the router credential that
// oaicaLaunchAPIKeyForEnv() returns.
func TestAgentLaunchSendsTheLocalServesOwnKey(t *testing.T) {
	invServeSetup(t)
	const localKey = "sk-local-serve-key"
	t.Setenv("OAICA_API_KEY", "sk-cloud-router-key")

	backendPort := invServeBackend(t, "bonsai")
	servePort := invServeStart(t, "127.0.0.1", localKey, backendPort)
	origin := fmt.Sprintf("http://127.0.0.1:%d", servePort)
	writeLocalRegistry(t, []oaicaLocalServersRegistryEntry{{
		Model: "bonsai", Origin: origin, PID: os.Getpid(), APIKey: localKey,
	}})

	// What the server itself accepts, so the assertion below is the server's
	// verdict and not this test's reading of the code.
	post := func(bearer string) int {
		req, _ := http.NewRequest(http.MethodPost, origin+"/v1/chat/completions",
			strings.NewReader(`{"model":"bonsai","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("POST %s with %q: %v", origin, bearer, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(localKey); code != 200 {
		t.Fatalf("premise changed: the serve rejected its own registered key (%d)", code)
	}
	if code := post("sk-cloud-router-key"); code != 401 {
		t.Fatalf("premise changed: the serve accepted the router's key (%d), want 401", code)
	}

	baseURL, token, upstreamModel, _, err := ResolveAgentModel(context.Background(), "bonsai:local")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") {
		t.Errorf("baseURL = %q, want the loopback logging proxy in front of %q", baseURL, origin)
	}
	if token != localKey {
		t.Errorf("ResolveAgentModel(\"bonsai:local\") returned token %q; the serve registered %q and its proxy "+
			"rejects any other bearer with 401, so `oaica agent` could not authenticate to a server started with "+
			"--api-key while `oaica launch` (which reads the registry) could", token, localKey)
	}
	if code := post(token); code != 200 {
		t.Errorf("the bearer ResolveAgentModel returned (%q) is rejected by the very server it resolved to (HTTP %d)", token, code)
	}
	if upstreamModel != "bonsai" {
		t.Errorf("upstreamModel = %q, want \"bonsai\"", upstreamModel)
	}
	// The launch path agrees — this is the contract both launchers share.
	ep, err := resolveLaunchEndpoint("bonsai:local")
	if err != nil {
		t.Fatalf("resolveLaunchEndpoint: %v", err)
	}
	if ep.Token != localKey {
		t.Errorf("resolveLaunchEndpoint token = %q, want the registered %q", ep.Token, localKey)
	}
}
