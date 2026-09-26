package launch

// remote_credential_preserve_integrity_test.go — `oaica remote add` on an
// existing name overwrote the stored credential with the default (empty)
// whenever the key flags were not repeated (2026-09-26 audit).
//
// RemoteAdd follows "a field with a flag is whatever you passed, absent means
// cleared" — a rule that exists so a field can be returned to its default by
// omitting it. For the credential the default is not neutral: empty means the
// row has no key, every request against it fails, and the secret is gone from
// the only place it was written down. So a repoint —
//
//	oaica remote add box --base-url https://new
//
// said nothing about credentials and deleted one, while the confirmation line
// named only the new URL. Version already has the explicit "the reset has to
// be typed" treatment for the same class of reason (VersionSet); this is the
// same hazard with a secret in it.

import (
	"testing"
)

func remoteByName(t *testing.T, name string) userRemote {
	t.Helper()
	f, _, err := loadUserRemotesFileRaw()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range f.Remotes {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no remote %q in %+v", name, f.Remotes)
	return userRemote{}
}

// A repoint that says nothing about credentials must keep them.
func TestRemoteAddKeepsAStoredKeyWhenTheFlagIsAbsent(t *testing.T) {
	withTempRemotesFile(t)

	if _, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: "https://old", APIKey: "sk-secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: "https://new"}); err != nil {
		t.Fatal(err)
	}

	got := remoteByName(t, "box")
	if got.APIKey != "sk-secret" {
		t.Errorf("api_key = %q after a repoint that never mentioned credentials, want it preserved — the key was written in exactly one place and the confirmation line said nothing about dropping it", got.APIKey)
	}
	if got.BaseURL != "https://new" {
		t.Errorf("base_url = %q, want the new one (the point of the command)", got.BaseURL)
	}

	// Same for the environment-variable indirection, which is the shape the
	// file is supposed to keep secrets OUT of.
	if _, err := RemoteAdd(RemoteAddOptions{Name: "env-box", BaseURL: "https://old", APIKeyEnv: "BOX_KEY"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoteAdd(RemoteAddOptions{Name: "env-box", BaseURL: "https://new"}); err != nil {
		t.Fatal(err)
	}
	if got := remoteByName(t, "env-box"); got.APIKeyEnv != "BOX_KEY" {
		t.Errorf("api_key_env = %q after a repoint, want it preserved", got.APIKeyEnv)
	}
}

// The flag still does what it says when it IS passed.
func TestRemoteAddStillAppliesAndSwitchesAnExplicitCredential(t *testing.T) {
	withTempRemotesFile(t)

	if _, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: "https://a", APIKey: "sk-old", KeySet: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: "https://a", APIKey: "sk-new", KeySet: true}); err != nil {
		t.Fatal(err)
	}
	if got := remoteByName(t, "box").APIKey; got != "sk-new" {
		t.Fatalf("api_key = %q, want the rotated key", got)
	}

	// Switching to the env indirection must drop the literal — leaving both set
	// is the state the command itself calls mutually exclusive.
	if _, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: "https://a", APIKeyEnv: "BOX_KEY", KeySet: true}); err != nil {
		t.Fatal(err)
	}
	got := remoteByName(t, "box")
	if got.APIKeyEnv != "BOX_KEY" || got.APIKey != "" {
		t.Fatalf("switching to --api-key-env left %+v, want the env name set and the literal key gone", got)
	}
}

// Clearing is still possible, it just has to be typed: passing the flag empty
// is an instruction, omitting it is not.
func TestRemoteAddClearsACredentialOnlyWhenTheFlagIsPassedEmpty(t *testing.T) {
	withTempRemotesFile(t)

	if _, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: "https://a", APIKey: "sk-secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: "https://a", APIKey: "", KeySet: true}); err != nil {
		t.Fatal(err)
	}
	if got := remoteByName(t, "box").APIKey; got != "" {
		t.Fatalf("api_key = %q after an explicitly empty --api-key, want it cleared — the reset must stay possible, it just must not be the default", got)
	}
}
