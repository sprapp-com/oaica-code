package launch

// auth_remote_login_name_test.go — `oaica auth login <remote>` must store the
// key under a name the read path actually looks up (round-4 audit,
// 2026-09-26).
//
// AuthLogin lowercased an unknown-but-configured provider name before writing
// the store, while storedAuthKey / userRemote.key() look the name up
// case-sensitively. A remote named with any uppercase letter therefore
// accepted a login, printed success, and stayed unauthenticated: the very
// next launch asked for the key again, and no error appeared anywhere.

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestAuthLoginStoresUnderTheRemotesOwnName(t *testing.T) {
	dir := t.TempDir()
	setLaunchTestHome(t, dir)
	t.Setenv("OAICA_AUTH_FILE", filepath.Join(dir, "auth.json"))

	if _, err := RemoteAdd(RemoteAddOptions{Name: "MyBox", BaseURL: "https://api.example.com"}); err != nil {
		t.Fatalf("remote add: %v", err)
	}
	var out bytes.Buffer
	if err := AuthLogin(&out, "MyBox", "sk-secret-0123456789"); err != nil {
		t.Fatalf("auth login: %v", err)
	}

	// The production resolution path every request's bearer comes from.
	ep, ok := resolveRemoteEndpoint("MyBox/some-model")
	if !ok {
		t.Fatalf("premise changed: MyBox/some-model did not resolve to a user remote")
	}
	if ep.Token != "sk-secret-0123456789" {
		store, _, _ := loadAuthStore()
		keys := make([]string, 0, len(store.Providers))
		for k := range store.Providers {
			keys = append(keys, k)
		}
		t.Errorf("`oaica auth login MyBox` reported success (%s) and stored %v, but the remote's own key lookup "+
			"(userRemote.key -> storedAuthKey(\"MyBox\")) resolves to %q — the login is unusable, with no error "+
			"anywhere.", out.String(), keys, ep.Token)
	}
}

// The same defect through the launch-time prompt: a key already stored through
// `oaica auth login` must satisfy ensureRemoteAPIKeyForModel, or the user is
// asked twice for the same credential.
func TestAuthLoginSatisfiesTheLaunchTimeKeyCheck(t *testing.T) {
	dir := t.TempDir()
	setLaunchTestHome(t, dir)
	t.Setenv("OAICA_AUTH_FILE", filepath.Join(dir, "auth.json"))

	if _, err := RemoteAdd(RemoteAddOptions{Name: "MyBox", BaseURL: "https://api.example.com", APIKeyEnv: "MYBOX_KEY"}); err != nil {
		t.Fatalf("remote add: %v", err)
	}
	var out bytes.Buffer
	if err := AuthLogin(&out, "MyBox", "sk-secret-0123456789"); err != nil {
		t.Fatalf("auth login: %v", err)
	}
	if err := ensureRemoteAPIKeyForModel("MyBox/some-model"); err != nil {
		t.Errorf("after `oaica auth login MyBox` reported success, the next launch still demands a key: %v", err)
	}

	// A lowercase remote keeps working (the control the fix was measured
	// against), and an unknown provider name still fails loudly.
	if _, err := RemoteAdd(RemoteAddOptions{Name: "mybox2", BaseURL: "https://api.example.com"}); err != nil {
		t.Fatalf("remote add: %v", err)
	}
	if err := AuthLogin(&out, "MYBOX2", "sk-secret-0123456789"); err != nil {
		t.Fatalf("auth login with a differently-cased name of a known remote: %v", err)
	}
	if ep, ok := resolveRemoteEndpoint("mybox2/some-model"); !ok || ep.Token == "" {
		t.Errorf("a known remote named in another case did not receive its key: ok=%v token=%q", ok, ep.Token)
	}
	if err := AuthLogin(&out, "nosuchremote", "sk-secret-0123456789"); err == nil {
		t.Error("an unknown provider name was accepted")
	}
}
