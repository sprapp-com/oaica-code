package launch

// remote_name_line_forgery_integrity_test.go — a newline in a remote NAME forged
// output in `oaica remote list` and `remote show` (2026-09-26 audit, ninth
// round, auditor B).
//
// RemoteAdd rejected only "/" in a name, so `remote add $'mine\nzai-coding-plan'
// --base-url https://evil.example.com` was accepted and the name was printed
// unescaped as a table cell: the listing grew an extra, entirely fabricated row
// that a reader takes as a real configured remote. `remote show` forged a second
// field line the same way. The printers quote such a name as well, because
// remotes.json is hand-editable and was written by versions with no check.

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// quoteJSON renders s as a JSON string literal, so a test can put a newline into
// a remotes.json value the way a hand-edit or an older oaica would have.
func quoteJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A name already in the file (hand-edited, or written by an older version) must
// not forge a listing row.
func TestRemoteListDoesNotForgeARowFromAName(t *testing.T) {
	remotesPath := withTempRemotesFile(t)

	forged := "mine\nzai-coding-plan  https://evil.example.com  openai  tool_calls  none"
	body := `{"remotes":[{"name":` + quoteJSON(t, forged) + `,"base_url":"https://api.example.com","upstream_model":"m1"}]}`
	if err := os.WriteFile(remotesPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var list bytes.Buffer
	if err := WriteRemoteList(&list); err != nil {
		t.Fatalf("WriteRemoteList: %v", err)
	}
	out := list.String()
	lines := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) != "" {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("`oaica remote list` printed %d line(s) for one configured remote, want 2 (header + row) — a newline in the name forged a fabricated remote:\n%s", lines, out)
	}
	if strings.Contains(out, "\nzai-coding-plan") {
		t.Errorf("the forged row is in the output:\n%s", out)
	}

	var show bytes.Buffer
	if err := WriteRemoteShow(&show, forged); err != nil {
		t.Fatalf("WriteRemoteShow: %v", err)
	}
	if strings.Contains(show.String(), "\nbase_url:      https://evil.example.com") {
		t.Errorf("`oaica remote show` forged a field line from the name:\n%s", show.String())
	}
	if n := strings.Count(show.String(), "base_url:"); n != 1 {
		t.Errorf("`remote show` printed %d base_url lines, want exactly 1 — the name forged another:\n%s", n, show.String())
	}
}

// And add-time validation now refuses the name outright, so the file cannot
// grow another one.
func TestRemoteAddRejectsControlCharactersInAName(t *testing.T) {
	withTempRemotesFile(t)

	for _, name := range []string{"mine\nzai", "mine\tzai", "mine\x1b[31mzai", "mine\rzai"} {
		if _, err := RemoteAdd(RemoteAddOptions{Name: name, BaseURL: "https://api.example.com"}); err == nil {
			t.Errorf("RemoteAdd accepted the name %q — it is printed as a table cell, so a control character in it forges a row there", name)
		}
	}
	// Control: an ordinary name, including one with a space, still works.
	if _, err := RemoteAdd(RemoteAddOptions{Name: "my box", BaseURL: "https://api.example.com"}); err != nil {
		t.Errorf("RemoteAdd rejected the ordinary name \"my box\": %v", err)
	}
}

// A control character in the two remaining fields these printers print verbatim
// — `wire` and `tool_format` — forges a listing row and a `remote show` field
// line the same way the name and the version did (2026-09-26 audit, fourteenth
// round). RemoteAdd checks both with oneOf, but add-time validation is not the
// only defence: remotes.json is hand-edited, and the file on disk is what these
// printers read.
func TestRemoteListAndShowDoNotForgeFromWireOrToolFormat(t *testing.T) {
	remotesPath := withTempRemotesFile(t)

	forgedWire := "openai\nzai-coding-plan  https://evil.example.com  openai  tool_calls  none"
	forgedTool := "tool_calls\nfake-remote     https://evil.example.com  anthropic  xml  none"
	// The AUTH column and `remote show`'s api_key line are the same shape: both
	// print a value derived from the file verbatim ("env:" + the api_key_env
	// name, or "via:" + auth_via) through authSourceProse's default branch.
	forgedEnv := "KAT_KEY\nzai-coding-plan  https://evil.example.com  openai  tool_calls  none"
	body := `{"remotes":[{"name":"mine","base_url":"https://api.example.com","upstream_model":"m1","wire":` +
		quoteJSON(t, forgedWire) + `,"tool_format":` + quoteJSON(t, forgedTool) + `,"api_key_env":` + quoteJSON(t, forgedEnv) + `}]}`
	if err := os.WriteFile(remotesPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var list bytes.Buffer
	if err := WriteRemoteList(&list); err != nil {
		t.Fatalf("WriteRemoteList: %v", err)
	}
	out := list.String()
	lines := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) != "" {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("`oaica remote list` printed %d line(s) for one configured remote, want 2 (header + row) — a newline in wire or tool_format forged a fabricated remote:\n%s", lines, out)
	}
	for _, forged := range []string{"\nzai-coding-plan", "\nfake-remote"} {
		if strings.Contains(out, forged) {
			t.Errorf("the forged row is in the listing output:\n%s", out)
		}
	}

	var show bytes.Buffer
	if err := WriteRemoteShow(&show, "mine"); err != nil {
		t.Fatalf("WriteRemoteShow: %v", err)
	}
	for _, field := range []string{"wire:", "tool_format:", "api_key:"} {
		if n := strings.Count(show.String(), field); n != 1 {
			t.Errorf("`remote show` printed %d %s line(s), want exactly 1 — the field value forged another:\n%s", n, field, show.String())
		}
	}
}
