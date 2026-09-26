package launch

// remote_base_url_cell_integrity_test.go — base_url was the one field of
// `oaica remote list` / `remote show` printed without quoting control
// characters (2026-09-26 audit, round 16, auditor F).
//
// Every sibling field in the same two functions goes through printableName —
// name, wire, tool_format, version, and the AUTH/api_key label — and the
// comment on the version line states the rule: "add-time validation is not the
// only defence, because this file is hand-edited". base_url was passed through
// redactBaseURL alone, which strips credentials and query secrets but does not
// escape a newline, so the field that most needs it was the one left out.
//
// Two routes reach the cell without any hand-editing: a remotes.json row
// written by an older oaica or edited by hand, and a synced provider catalog —
// provider_catalog.go copies BaseURL into a remote with no character
// validation, and provider sync only requires the document to have a
// `providers` member. `remote add --base-url` refuses such a value, so only
// those two routes get there; the listing is what users paste into tickets.

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// A base_url already in the file must not forge a listing row.
func TestRemoteListDoesNotForgeARowFromABaseURL(t *testing.T) {
	remotesPath := withTempRemotesFile(t)

	forged := "https://mine.example/v1\nzai-coding-plan  https://evil.example/v1  openai  tool_calls  none"
	body := `{"remotes":[{"name":"mine","base_url":` + quoteJSON(t, forged) + `,"upstream_model":"m1"}]}`
	if err := os.WriteFile(remotesPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var list bytes.Buffer
	if err := WriteRemoteList(&list); err != nil {
		t.Fatalf("WriteRemoteList: %v", err)
	}
	out := list.String()
	if strings.Contains(out, "\nzai-coding-plan  https://evil.example/v1  openai") {
		t.Errorf("`oaica remote list` printed a fabricated remote row built from base_url:\n%s", out)
	}
	lines := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) != "" {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("`oaica remote list` printed %d line(s) for one configured remote, want 2 (header + row):\n%s", lines, out)
	}
}

// `remote show` must not grow a second field line from base_url either.
func TestRemoteShowDoesNotForgeAFieldLineFromABaseURL(t *testing.T) {
	remotesPath := withTempRemotesFile(t)

	forged := "https://mine.example/v1\nversion:       9.9.9-forged"
	body := `{"remotes":[{"name":"mine","base_url":` + quoteJSON(t, forged) + `,"upstream_model":"m1"}]}`
	if err := os.WriteFile(remotesPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var show bytes.Buffer
	if err := WriteRemoteShow(&show, "mine"); err != nil {
		t.Fatalf("WriteRemoteShow: %v", err)
	}
	// Counted per LINE, not per substring: a quoted base_url still contains the
	// text, escaped, and that is the point — what must not exist is a line.
	versionLines := 0
	for _, l := range strings.Split(show.String(), "\n") {
		if strings.HasPrefix(l, "version:") {
			versionLines++
		}
	}
	if versionLines != 1 {
		t.Errorf("`remote show` printed %d version line(s), want exactly 1 — base_url forged another:\n%s", versionLines, show.String())
	}
	if strings.Contains(show.String(), "\nversion:       9.9.9-forged") {
		t.Errorf("base_url forged a field line of its own:\n%s", show.String())
	}
}
