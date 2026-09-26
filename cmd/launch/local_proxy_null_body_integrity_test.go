package launch

// local_proxy_null_body_integrity_test.go — a literal `null` body panicked the
// serve proxy's handler (2026-09-26 audit, ninth round, auditor B).
//
// normalizeSystemMessages unmarshals the body into a map and then writes keys
// into it. Go's json.Unmarshal accepts the literal `null` for a map — it leaves
// the map nil and reports no error — so the write to parsed["system"] on
// /v1/messages was an assignment to an entry in a nil map, which panics. The
// path is reachable in production: `oaica serve` starts this proxy
// (cmd/oaica_pull_serve.go), so a single `curl -d null` against the local
// endpoint panicked inside the handler; net/http recovers per connection, so the
// client saw a dropped connection instead of a 400 and no usage row was written
// — a request that failed with nothing anywhere to say so.

import (
	"encoding/json"
	"testing"
)

// A body that is valid JSON but not an object must be forwarded, not rewritten
// and not crashed on.
func TestNormalizeSystemMessagesSurvivesNonObjectBodies(t *testing.T) {
	for _, body := range []string{"null", "{}", "12", `"a string"`, "[]", ""} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("normalizeSystemMessages(%q) panicked: %v — the serve proxy's handler answers with a dropped connection instead of an error, and writes no usage row", body, r)
				}
			}()
			out, err := normalizeSystemMessages("/v1/messages", []byte(body))
			if err != nil {
				// Forwarding as-is is the contract for anything it cannot
				// rewrite; an error is only a problem if it also stopped the
				// request from going anywhere.
				return
			}
			// Either it came back untouched (the documented "not JSON, forward
			// as-is" path) or what it produced is JSON. Anything else is a
			// body mangled between the client and the backend.
			if string(out) != body && !json.Valid(out) {
				t.Errorf("normalizeSystemMessages(%q) produced neither the input nor valid JSON: %q", body, out)
			}
		}()
	}
}

// The behavior the function exists for must survive the guard: an object body
// with a stray system message still comes back rewritten.
func TestNormalizeSystemMessagesStillRewritesObjects(t *testing.T) {
	out, err := normalizeSystemMessages("/v1/messages", []byte(
		`{"model":"m","messages":[{"role":"system","content":"SYS-A"},{"role":"user","content":"hi"}]}`,
	))
	if err != nil {
		t.Fatalf("normalizeSystemMessages: %v", err)
	}
	var parsed struct {
		System   string `json:"system"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if parsed.System != "SYS-A" {
		t.Errorf("system = %q, want the stray system message hoisted to the top level\n%s", parsed.System, out)
	}
	if len(parsed.Messages) != 1 || parsed.Messages[0].Role != "user" {
		t.Errorf("messages = %+v, want only the user turn left\n%s", parsed.Messages, out)
	}
}
