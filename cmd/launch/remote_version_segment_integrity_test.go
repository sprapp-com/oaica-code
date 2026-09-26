package launch

// remote_version_segment_integrity_test.go — `--api-version` reached the URL
// every request is served from with no check at all (2026-09-26 audit, round
// 13).
//
// RemoteAdd applied strings.TrimSpace and nothing else, while --base-url one
// line over went through validateRemoteBaseURL. The version is concatenated
// onto the base by openAIBase (remoteBaseURL(r)+"/"+v) and modelsURL, so a
// version of "v1?x=1" made every request go to ".../v1?x=1/chat/completions",
// "../../../../admin" climbed out of the base path entirely, and a control
// character forged a line wherever the value is printed — `oaica remote show`
// grew a second, fabricated `base_url:` row.
//
// The rule, stated once in validateRemoteVersion: a version segment may carry
// only what belongs in a URL PATH segment — no query, no fragment, no "/", no
// dot-segment, no control character, no whitespace.

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestAVersionThatCannotBeAURLPathSegmentIsRefused(t *testing.T) {
	for _, bad := range []struct{ version, why string }{
		{"v1?x=1", "a query swallows the path oaica appends, so every request goes to the wrong URL"},
		{"v1#frag", "a fragment is not part of the endpoint"},
		{"../../../../admin", "the version is concatenated onto base_url, so a relative segment climbs out of the base path"},
		{"v1/v2", "the version is ONE path segment, and the URL oaica builds has no room for a second"},
		{"..", "a dot-segment is not a path segment"},
		{".", "a dot-segment is not a path segment"},
		{"v1 1", "a space"},
		{"v1\t1", "a control character"},
		{"v1\nEVIL", "a control character"},
		{"v1\nbase_url:      https://attacker.example/v1", "a control character forges a line in `oaica remote show`"},
	} {
		withTempRemotesFile(t)
		_, err := RemoteAdd(RemoteAddOptions{
			Name:       "evil",
			BaseURL:    "https://api.deepseek.com",
			Version:    bad.version,
			VersionSet: true,
		})
		if err == nil {
			t.Errorf("RemoteAdd accepted --api-version %q (%s) — the value is concatenated into the URL this remote serves every request from, and it is echoed by `oaica remote show`", bad.version, bad.why)
			continue
		}
		if !strings.Contains(err.Error(), "--api-version") {
			t.Errorf("RemoteAdd refused --api-version %q with %q, which does not name the flag the user typed", bad.version, err)
		}
	}
}

// A refused add writes nothing: the row must not exist to be used by a later
// launch even though the command reported a failure.
func TestARefusedVersionIsNotStored(t *testing.T) {
	path := withTempRemotesFile(t)
	if _, err := RemoteAdd(RemoteAddOptions{
		Name: "evil", BaseURL: "https://api.deepseek.com", Version: "v1/x", VersionSet: true,
	}); err == nil {
		t.Fatal("premise: the malformed version was accepted")
	}
	if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), "evil") {
		t.Errorf("a refused `remote add` still wrote the row into %s:\n%s", path, b)
	}
}

// The controls: every version shape a real endpoint uses is still accepted.
func TestOrdinaryVersionsAreStillAccepted(t *testing.T) {
	for _, ok := range []string{
		"", "v1", "v4", "1", "v1beta", "2024-02-01", "none", "None", "NONE", "/v1/", "v1.1", "v1~beta",
	} {
		withTempRemotesFile(t)
		if _, err := RemoteAdd(RemoteAddOptions{
			Name: "ok", BaseURL: "https://api.deepseek.com", Version: ok, VersionSet: true,
		}); err != nil {
			t.Errorf("RemoteAdd(%q) = %v, want it accepted", ok, err)
		}
	}
}

// And the second line of defence, for the file `remote add` does not own: a
// version that reached remotes.json before this rule existed (or by hand) is
// printed QUOTED, so it cannot forge a second field line in `remote show`.
func TestShowQuotesAVersionThatWouldForgeAFieldLine(t *testing.T) {
	path := withTempRemotesFile(t)
	body := `{"remotes":[{"name":"evil","base_url":"https://api.deepseek.com","version":"v1\nbase_url:      https://attacker.example/v1"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := WriteRemoteShow(&out, "evil"); err != nil {
		t.Fatal(err)
	}
	got := out.String()

	forged := "base_url:      https://attacker.example/v1"
	if strings.Contains(got, "\n"+forged) {
		t.Errorf("`remote show` printed a version carrying a newline as a second, fabricated field line — the reader takes it as the remote's real base_url:\n%s", got)
	}
	if n := strings.Count(got, "\nbase_url:"); n != 1 {
		t.Errorf("`remote show` printed %d base_url field lines, want exactly 1:\n%s", n, got)
	}
	if !strings.Contains(got, strconv.Quote("v1\nbase_url:      https://attacker.example/v1")) {
		t.Errorf("the version was not printed in quoted form, so nothing distinguishes it from a real field line:\n%s", got)
	}
}
