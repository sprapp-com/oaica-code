package cmd

// oaica_cli_diagnosis_redaction_integrity_test.go — gateway-authored text
// reached CLI errors unredacted, and unbounded (2026-09-26 audit, tenth round).
//
// Three shapes, one hole. Every `client.Do` in oaica_client.go wraps its
// transport error with launch.RedactError, but the sites that build an error
// from the RESPONSE are outside that fence:
//
//   - A router that names the key it refused ("invalid api key sk-live-…") in
//     its JSON error message had that message printed verbatim — the proxy side
//     of the same leak was fixed long ago (redactUpstreamDiagnosis), this side
//     was missed. The key is one this CLI just sent, so the CLI is the one
//     process that can recognise it.
//   - The agent sidecar's address was printed straight from OAICA_AGENT_HOST,
//     whose userinfo may carry a credential, and its error body was echoed
//     whole.
//   - Every body echo read up to httpbody.DefaultMax (64 MiB): a non-JSON or
//     `{}` answer to a failed request put megabytes into a message a user
//     pastes into a ticket. cmd/site.go's truncateForError exists for exactly
//     this and was not used here.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The credential the CLI sends is scrubbed from a gateway's own wording.
func TestARouterNamingTheKeyItRefusedDoesNotReachStderr(t *testing.T) {
	const key = "sk-live-CLISENT-9876543210"
	t.Setenv("OAICA_API_KEY", key)
	// No saved key on disk may outrank the env var: the env var wins, and the
	// file must not exist for a clean premise.
	t.Setenv("OAICA_HOST", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+key {
			t.Errorf("the request carried %q, want the configured key", got)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key ` + key + `"}}`))
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)

	_, _, err := oaicaChatMeteredLive("m", nil)
	if err == nil {
		t.Fatalf("premise: a 401 answered without error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("the CLI error carries the key it sent: %q — a vendor refusal routinely quotes the rejected credential, and this is the one process that knows the string", err)
	}
	if !strings.Contains(err.Error(), oaicaAuthHint) {
		t.Errorf("the 401 hint was lost with the redaction: %q", err)
	}
}

// A failed request's body is bounded, whatever the gateway answers with.
func TestAFailedRequestsBodyIsNotEchoedWhole(t *testing.T) {
	t.Setenv("OAICA_API_KEY", "sk-live-CLISENT-9876543210")
	big := strings.Repeat("x", 3<<20) // 3 MiB, well past what a person can read
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)

	_, err := oaicaListLoras()
	if err == nil {
		t.Fatalf("premise: a 500 answered without error")
	}
	if len(err.Error()) > 1000 {
		t.Errorf("a 500 with a %d-byte body produced a %d-byte error message — this is the string a user pastes into a support ticket", len(big), len(err.Error()))
	}
}

// The agent sidecar's address and its body are sanitized the same way.
func TestTheAgentSidecarsCredentialDoesNotReachStderr(t *testing.T) {
	const key = "sk-agent-USERINFO-1234567890"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("sidecar failed: upstream refused " + key))
	}))
	defer srv.Close()
	t.Setenv("OAICA_AGENT_HOST", strings.Replace(srv.URL, "http://", "http://"+key+"@", 1))

	_, err := oaicaAgentRun("ping")
	if err == nil {
		t.Fatalf("premise: a 500 from the sidecar answered without error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("the sidecar error (or the address it was reached at) carries the credential: %q", err)
	}

	// And the failure to connect at all names the host, not the credential.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	t.Setenv("OAICA_AGENT_HOST", strings.Replace(deadURL, "http://", "http://"+key+"@", 1))
	if _, err := oaicaAgentRun("ping"); err != nil && strings.Contains(err.Error(), key) {
		t.Errorf("the transport error for the sidecar carries the credential: %q", err)
	}
}

// The saved key is the one that gets scrubbed when it is the one that was sent,
// so redaction cannot drift from authorization.
func TestTheScrubbedSecretIsTheOneThatWasSent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OAICA_API_KEY", "")
	t.Setenv("OAICA_HOST", "")
	dir := filepath.Join(home, ".oaica")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const saved = "sk-live-SAVED-1234567890"
	if err := os.WriteFile(filepath.Join(dir, "api_key"), []byte(saved+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+saved {
			t.Errorf("the request carried %q, want the key saved by `oaica signin`", got)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"key ` + saved + ` is not entitled to this model"}}`))
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)

	_, _, err := oaicaChatMeteredLive("m", nil)
	if err == nil {
		t.Fatalf("premise: a 403 answered without error")
	}
	if strings.Contains(err.Error(), saved) {
		t.Errorf("the error carries the credential the CLI actually sent: %q", err)
	}
}
