package launch

// remote_auth_column_source_integrity_test.go — the AUTH column, and `remote
// show`'s api_key line, denied credentials that were really in use
// (2026-09-26 audit, ninth round, auditor B).
//
// Both answered "how does this remote authenticate" from two fields —
// api_key_env and api_key — while userRemote.key() resolves five sources. A
// remote logged in with `oaica auth login` (stored), one reusing another tool's
// login (`auth_via`), and one carrying the credential in base_url's userinfo
// all printed "none" / "none" while the proxy authenticated with a real
// credential. "Why is this remote not working" is exactly the question that
// column is read to answer, so it must not deny the source in use.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// storeAuthKeyFor writes an auth.json holding a stored credential for provider
// and points OAICA_AUTH_FILE at it.
func storeAuthKeyFor(t *testing.T, provider, key string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	store := map[string]any{
		"version": authStoreVersion,
		"providers": map[string]any{
			provider: map[string]any{"type": authCredentialTypeAPIKey, "key": key, "saved_at": time.Now().UTC()},
		},
	}
	b, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OAICA_AUTH_FILE", path)
}

// A remote whose only credential is a stored login must not read "none".
func TestRemoteAuthColumnSeesAStoredLogin(t *testing.T) {
	remotesPath := withTempRemotesFile(t)
	storeAuthKeyFor(t, "mystore", "sk-stored-secret-value")

	// No api_key, no api_key_env — the credential lives in the auth store.
	if err := os.WriteFile(remotesPath, []byte(`{"remotes":[{"name":"mystore","base_url":"https://api.example.com","upstream_model":"m1"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := storedAuthKey("mystore"); got == "" {
		t.Fatalf("setup: no stored credential resolves for mystore — this test no longer covers the source it names")
	}

	var list bytes.Buffer
	if err := WriteRemoteList(&list); err != nil {
		t.Fatalf("WriteRemoteList: %v", err)
	}
	if strings.Contains(list.String(), "none") {
		t.Errorf("`oaica remote list` shows AUTH none for a remote with a stored login:\n%s", list.String())
	}
	if strings.Contains(list.String(), "sk-stored-secret-value") {
		t.Errorf("the listing printed the credential itself:\n%s", list.String())
	}

	var show bytes.Buffer
	if err := WriteRemoteShow(&show, "mystore"); err != nil {
		t.Fatalf("WriteRemoteShow: %v", err)
	}
	if strings.Contains(show.String(), "api_key:       none") {
		t.Errorf("`oaica remote show` reports api_key none for a remote with a stored login — the proxy authenticates with it:\n%s", show.String())
	}
	if strings.Contains(show.String(), "sk-stored-secret-value") {
		t.Errorf("`remote show` printed the credential itself:\n%s", show.String())
	}
}

// A remote configured with a declared env var keeps reporting that variable
// name (the config is the answer, not this process's environment), and the
// value is never printed.
func TestRemoteAuthColumnStillNamesEnvVars(t *testing.T) {
	remotesPath := withTempRemotesFile(t)
	t.Setenv("MYBOX_KEY", "sk-env-secret-value")

	if err := os.WriteFile(remotesPath, []byte(`{"remotes":[{"name":"mybox","base_url":"https://api.example.com","upstream_model":"m1","api_key_env":"MYBOX_KEY"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var list bytes.Buffer
	if err := WriteRemoteList(&list); err != nil {
		t.Fatalf("WriteRemoteList: %v", err)
	}
	if !strings.Contains(list.String(), "env:MYBOX_KEY") {
		t.Errorf("`oaica remote list` no longer names the env var it authenticates from:\n%s", list.String())
	}
	if strings.Contains(list.String(), "sk-env-secret-value") {
		t.Errorf("the listing printed the credential itself:\n%s", list.String())
	}

	var show bytes.Buffer
	if err := WriteRemoteShow(&show, "mybox"); err != nil {
		t.Fatalf("WriteRemoteShow: %v", err)
	}
	if !strings.Contains(show.String(), "api_key:       env:MYBOX_KEY") {
		t.Errorf("`oaica remote show` no longer names the env var:\n%s", show.String())
	}
}

// A credential embedded in base_url's userinfo: the URL prints REDACTED, so a
// "none" beside it claimed the remote had no auth at all.
func TestRemoteAuthColumnSeesUserinfoCredentials(t *testing.T) {
	remotesPath := withTempRemotesFile(t)

	if err := os.WriteFile(remotesPath, []byte(`{"remotes":[{"name":"urlbox","base_url":"https://sk-url-secret-value@api.example.com","upstream_model":"m1"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, token := splitRemoteUserinfo("https://sk-url-secret-value@api.example.com"); token == "" {
		t.Fatalf("setup: userinfo no longer carries a credential — this test no longer covers the source it names")
	}

	var show bytes.Buffer
	if err := WriteRemoteShow(&show, "urlbox"); err != nil {
		t.Fatalf("WriteRemoteShow: %v", err)
	}
	if strings.Contains(show.String(), "api_key:       none") {
		t.Errorf("`oaica remote show` reports api_key none for a remote whose credential is in base_url:\n%s", show.String())
	}
	if strings.Contains(show.String(), "sk-url-secret-value") {
		t.Errorf("`remote show` printed the credential itself:\n%s", show.String())
	}
}
