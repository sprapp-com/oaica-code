package launch

// redaction_gap_invariants_test.go — pins the shapes a credential could reach
// the user in that the sanitizers did NOT cover (round-4 audit, 2026-09-26).
//
// All of them are the same root cause seen from different print sites: a
// credential in a URL is only hidden if the sanitizer RECOGNIZES the shape it
// is wearing.
//
//   - a newline inside the userinfo ("https://user:sk-…\n…@host/v1") defeated
//     unparseableUserinfo (its "." never matches "\n"), so url.Parse refused
//     the value, userinfoSecret returned nothing, and the leak scan had
//     neither a redacted text nor a known value: `oaica doctor --report`,
//     `oaica remote show` and `oaica remote list` printed the key.
//   - a query value beginning with whitespace ("?api_key= sk-…") captured as
//     the empty string, which redactQueryCredentials reads as "nothing to
//     hide", and querySecrets skipped it for the same reason.
//   - the Anthropic↔OpenAI translation proxy sanitized its error text with
//     redactCredentials alone, which handled neither a query-string key nor a
//     Basic password containing "/" — the response body carried the key back
//     to the launched client.
//   - ProviderSync's and CloudLimitsSync's report values (printed by
//     cmd/cmd.go) returned the raw --url, so `oaica provider sync --url
//     https://KEY@mirror/…` echoed the mirror credential, while the sibling
//     ModelSync returned the redacted display for the identical input.
//
// The marker below is embedded in every credential so a leak is greppable.

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const invLeakMarker = "AUDITF"

// invLeak fails the test and prints the exact bytes that leaked.
func invLeak(t *testing.T, what, text string) {
	t.Helper()
	t.Errorf("LEAK in %s — credential bytes reached the user-visible text:\n%s\n", what, invIndent(text))
}

func invIndent(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = "    | " + lines[i]
	}
	return strings.Join(lines, "\n")
}

// invWriteRemotes is the fixture for a base_url a user typed or pasted.
func invWriteRemotes(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".oaica")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "remotes.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OAICA_REMOTES_FILE", path)
}

// The two catalog syncs hand their caller the URL to print. The request keeps
// a mirror credential in its userinfo — that is how a private mirror
// authenticates — but the report line must not, which is what the sibling
// ModelSync report already promised.
func TestCatalogSyncReportsRedactAMirrorCredential(t *testing.T) {
	const key = "sk-live-" + invLeakMarker + "-0123456789"

	t.Run("provider sync", func(t *testing.T) {
		home := t.TempDir()
		setLaunchTestHome(t, home)
		// Seed the last good copy so the offline fallback takes the success
		// path the caller prints, without any network.
		cache := filepath.Join(home, ".oaica", "cache", "providers", "providers.json")
		if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cache, []byte(`{"version":1,"providers":[{"name":"mirror","base_url":"https://mirror.example.com"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// The offline fallback serves the cache only for the URL it came from
		// (catalogCacheSourcePath), so the seed has to name its source too.
		// The redacted form is what the sync writes.
		if err := os.WriteFile(catalogCacheSourcePath(cache), []byte(redactBaseURL("https://"+key+"@127.0.0.1:1/providers.json")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		rep, err := ProviderSync("https://" + key + "@127.0.0.1:1/providers.json")
		if err != nil {
			t.Fatalf("ProviderSync: %v", err)
		}
		printed := fmt.Sprintf("synced %d provider(s) from %s", rep.Count, rep.URL)
		if strings.Contains(rep.URL, key) {
			invLeak(t, "ProviderSyncReport.URL (printed by cmd/cmd.go)", printed)
		}
		if rep.URL == "" || !strings.Contains(rep.URL, "127.0.0.1:1") {
			t.Errorf("ProviderSyncReport.URL = %q — the report must still say which host it synced from", rep.URL)
		}
	})

	t.Run("cloud limits sync", func(t *testing.T) {
		home := t.TempDir()
		setLaunchTestHome(t, home)
		cache := filepath.Join(home, ".oaica", "cache", "cloud_limits", "cloud_limits.json")
		if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
			t.Fatal(err)
		}
		// The real catalog shape: limits is a MAP keyed by alias, not a list.
		// The old fixture was an array, which the sync used to cache anyway
		// because it discarded the parse error (2026-09-26 audit, third round
		// made the sync refuse an unreadable body, which is what exposed it).
		if err := os.WriteFile(cache, []byte(`{"version":1,"limits":{"x":{"context":1,"output":1}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(catalogCacheSourcePath(cache), []byte(redactBaseURL("https://"+key+"@127.0.0.1:1/cloud_limits.json")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		rep, err := CloudLimitsSync("https://" + key + "@127.0.0.1:1/cloud_limits.json")
		if err != nil {
			t.Fatalf("CloudLimitsSync: %v", err)
		}
		printed := fmt.Sprintf("synced %d cloud-alias limit(s) from %s", rep.Count, rep.URL)
		if strings.Contains(rep.URL, key) {
			invLeak(t, "CloudLimitsSyncReport.URL (printed by cmd/cmd.go)", printed)
		}
	})
}

// A newline in the userinfo is a shape url.Parse REFUSES, so the leak scan
// only knows the value if unparseableUserinfo — its fallback for exactly this
// — can see it. A value the scan does not know is a value a print site can
// add without the report noticing.
func TestDoctorReportRefusesANewlineUserinfoKey(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")

	// JSON "\n" decodes to a literal newline inside the userinfo. Nothing is
	// dialled: url.Parse rejects a control character first.
	const url = `https://user:sk-live-` + invLeakMarker + `-\n0123456789@127.0.0.1:1/v1`
	invWriteRemotes(t, home, `{"remotes":[{"name":"lan","base_url":"`+url+`","api_key_env":"LAN_KEY"}]}`)
	realURL := strings.ReplaceAll(url, `\n`, "\n")

	report, _ := buildDoctorReport()
	if strings.Contains(report, invLeakMarker) {
		invLeak(t, "`oaica doctor --report` output (gate did not refuse)", report)
	}
	// The gate needs a value to refuse; without it the scan is blind even
	// when this particular print site happens to be clean.
	if got := baseURLSecrets(realURL); len(got) == 0 {
		t.Errorf("baseURLSecrets found nothing for a newline-bearing userinfo, so the leak scan could not refuse " +
			"it: the report would print the key as soon as any print site touched the raw value")
	}
	if got := userinfoSecret(realURL); !strings.Contains(got, invLeakMarker) {
		t.Errorf("userinfoSecret = %q, want the userinfo including the pasted key", got)
	}
	if redacted := redactBaseURL(realURL); strings.Contains(redacted, invLeakMarker) {
		t.Errorf("redactBaseURL left the key in %q", redacted)
	}

	// The other print sites for the same value: `oaica remote show` / `list`.
	var show, list bytes.Buffer
	if err := WriteRemoteShow(&show, "lan"); err != nil {
		t.Fatalf("WriteRemoteShow: %v", err)
	}
	if err := WriteRemoteList(&list); err != nil {
		t.Fatalf("WriteRemoteList: %v", err)
	}
	if strings.Contains(show.String(), invLeakMarker) {
		invLeak(t, "`oaica remote show`", show.String())
	}
	if strings.Contains(list.String(), invLeakMarker) {
		invLeak(t, "`oaica remote list`", list.String())
	}
}

// The same blind spot for a query value that begins with whitespace: the
// value class excluded \s, so the value captured as "" and an empty value
// reads as "nothing to hide".
func TestDoctorReportRefusesALeadingSpaceQueryKey(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")

	// A space in the HOST as well, so url.Parse fails and nothing is dialled.
	const key = "sk-live-" + invLeakMarker + "-0123456789"
	const url = "https://ho st/v1?api_key= " + key
	invWriteRemotes(t, home, `{"remotes":[{"name":"lan","base_url":"`+url+`","api_key_env":"LAN_KEY"}]}`)

	report, _ := buildDoctorReport()
	if strings.Contains(report, invLeakMarker) {
		invLeak(t, "`oaica doctor --report` output (gate did not refuse)", report)
	}
	if got := querySecrets(url); len(got) == 0 {
		t.Errorf("querySecrets found nothing for %q, so the leak scan could not refuse the key", url)
	}
	if redacted := redactBaseURL(url); strings.Contains(redacted, invLeakMarker) {
		t.Errorf("redactBaseURL left the key in %q", redacted)
	}
}

// The translation proxy sanitizes upstream error text and hands it back to the
// launched client. These are the real shapes net/http produces for a request
// whose URL carries a credential, and redactCredentials — the general
// sanitizer every print site uses — has to cover all of them.
func TestRedactionCoversTheProxyErrorShapes(t *testing.T) {
	const key = "sk-live-" + invLeakMarker + "-0123456789"

	// net/http error text, produced by the same calls the proxy makes.
	raws := []string{
		"https://ho st/v1?api_key=" + key,          // key in the query string
		"https://user:" + key + "/tail@ho st/v1",   // Basic password containing "/"
		"https://user:" + key + "\n@ho st/v1",      // newline inside the userinfo
		"https://ho st/v1?api_key= " + key,         // query value starting with a space
		"https://ho st/v1?x=1&access_token=" + key, // another credential param name
	}
	for _, raw := range raws {
		req, err := http.NewRequest(http.MethodPost, raw, nil)
		if err != nil {
			// Not a parse error: the shape reached the transport. Use the
			// request's own error text either way.
			_ = req
		}
		text := raw
		if err != nil {
			text = err.Error()
		}
		if out := redactCredentials(text); strings.Contains(out, key) {
			invLeak(t, "redactCredentials (proxy error body)", "build upstream request: "+out)
		}
		if err := redactErr(fmt.Errorf("build upstream request: %s", text)); strings.Contains(err.Error(), key) {
			invLeak(t, "redactErr (proxy error body)", err.Error())
		}
	}
}

// The second input path for a credential-bearing URL: an OAICA_HOST value.
// splitRemoteUserinfo cannot parse it either, so the host is returned verbatim
// and every "rejected the API key" message is built from it.
func TestRedactionCoversTheEnvHostValue(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	const key = "sk-live-" + invLeakMarker + "-0123456789"
	t.Setenv("OAICA_HOST", "https://user:"+key+"-\n@127.0.0.1:1")

	host := oaicaLaunchHost()
	msg := fmt.Sprintf("%s rejected the API key — set OAICA_API_KEY or run `oaica signin`", redactBaseURL(host))
	if strings.Contains(msg, key) {
		invLeak(t, "error message built from the OAICA_HOST env value", msg)
	}
	if redacted := redactBaseURL("https://user:" + key + "-@127.0.0.1:1"); strings.Contains(redacted, key) {
		t.Errorf("redactBaseURL left an env host's key in %q", redacted)
	}
}
