package cmd

import (
	"strings"
	"testing"
)

// An OAICA_HOST carrying Basic credentials must not put them in an error a
// user or a ticket can read. The bearer-in-URL case is promoted and stripped
// (see SplitUserinfoCredential); a `user:password@` value is deliberately kept,
// because rewriting it would change how the request authenticates — which
// leaves redaction at the print site as the only defence, and 2026-09-26's
// audit found these paths printing it in full.
func TestOaicaHostErrorText_RedactsBasicCredentials(t *testing.T) {
	const host = "https://audituser:s3cretpw@127.0.0.1:1"
	t.Setenv("OAICA_HOST", host)

	// Nothing listens on port 1, so this is a transport error — the path the
	// audit reproduced.
	_, err := oaicaChatLive("box/kat-awq", []oaicaChatMessage{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatalf("expected a connection error from %s", host)
	}
	text := err.Error()
	if strings.Contains(text, "s3cretpw") {
		t.Errorf("the password reached the error text: %q", text)
	}
	if !strings.Contains(text, "REDACTED@127.0.0.1:1") {
		t.Errorf("the error should name the host with a redacted credential, got %q", text)
	}
}

// The same for a value url.Parse rejects: it cannot be transmitted, but a
// user can still have pasted it, and both our own %s and the parse error text
// quoted it verbatim.
func TestOaicaHostErrorText_RedactsUnparseableCredential(t *testing.T) {
	const host = "https://sk-live-AUDITSECRET@ho st"
	t.Setenv("OAICA_HOST", host)

	_, err := oaicaChatLive("box/kat-awq", []oaicaChatMessage{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatalf("expected an error from %s", host)
	}
	if strings.Contains(err.Error(), "AUDITSECRET") {
		t.Errorf("the token reached the error text: %q", err.Error())
	}
}

// And the models-list path the picker and routing use, which is its own
// fetch with its own "couldn't reach %s".
func TestOaicaModelsErrorText_RedactsBasicCredentials(t *testing.T) {
	const host = "https://audituser:s3cretpw@127.0.0.1:1"
	t.Setenv("OAICA_HOST", host)

	_, err := oaicaListModelsDetailedLive()
	if err == nil {
		t.Fatalf("expected a connection error from %s", host)
	}
	if strings.Contains(err.Error(), "s3cretpw") {
		t.Errorf("the password reached the error text: %q", err.Error())
	}
}
