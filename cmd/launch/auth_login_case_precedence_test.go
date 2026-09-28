package launch

// auth_login_case_precedence_test.go — the other half of the round-4
// auth-login fix, which handled only the branch where the catalog does NOT
// know the name.
//
// When it DOES know it, the catalog spelling used to win outright:
// `oaica auth login deepseek` with a remote added as "DeepSeek" matched
// catalog "deepseek", stored the key under "deepseek", and the remote's own
// case-SENSITIVE lookup (userRemote.key -> storedAuthKey("DeepSeek")) found
// nothing — a reported success, an unusable credential, and no error anywhere
// (2026-09-26 audit). A configured remote's own spelling has to win over the
// catalog's, in both directions.

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestAuthLoginKeepsTheRemotesSpellingWhenTheCatalogKnowsTheName(t *testing.T) {
	dir := t.TempDir()
	setLaunchTestHome(t, dir)
	t.Setenv("OAICA_AUTH_FILE", filepath.Join(dir, "auth.json"))

	// "deepseek" IS in the provider catalog (providers/oaica.json), so
	// knownAuthProvider resolves it case-insensitively — which is exactly the
	// path that used to overwrite the remote's own spelling.
	if _, known := knownAuthProvider("deepseek"); !known {
		t.Fatal("premise changed: the catalog no longer carries a provider named \"deepseek\" — this test needs a name the catalog knows and the remote spells differently")
	}

	if _, err := RemoteAdd(RemoteAddOptions{Name: "DeepSeek", BaseURL: "https://api.example.com"}); err != nil {
		t.Fatalf("remote add: %v", err)
	}

	const key = "sk-secret-casefold-0123456789"
	var out bytes.Buffer
	if err := AuthLogin(&out, "deepseek", key); err != nil {
		t.Fatalf("auth login: %v", err)
	}

	// The read path every request's bearer comes from.
	ep, ok := resolveRemoteEndpoint("DeepSeek/some-model")
	if !ok {
		t.Fatal("premise changed: DeepSeek/some-model did not resolve to a user remote")
	}
	if ep.Token != key {
		store, _, _ := loadAuthStore()
		stored := make([]string, 0, len(store.Providers))
		for name := range store.Providers {
			stored = append(stored, name)
		}
		t.Errorf("`oaica auth login deepseek` reported success (%s) and stored %v, but the remote named \"DeepSeek\" "+
			"resolves to %q — the catalog's spelling won over the configured remote's, so the credential nothing "+
			"looks up is the one that was written.", out.String(), stored, ep.Token)
	}
}
