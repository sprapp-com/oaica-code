package launch

// auth_store_unparseable_integrity_test.go — one stray byte made the credential
// store write-only-never (2026-09-26 audit).
//
// loadAuthStore returns a parse error for a file that is not the JSON it
// understands, and updateAuthStore — the ONLY mutation path — returned that
// error. So a truncated auth.json, a hand-edit with a trailing comma, or a file
// another tool wrote made `oaica auth login <provider> --key …` fail forever
// with "not valid JSON", while the credentials already in the file stayed
// unreadable. The only way out was deleting the file: refusing to write did not
// protect the user's keys, it just made them unreachable.
//
// The write path now moves such a file aside (`<path>.unreadable-<timestamp>`,
// same directory, same mode), starts from an empty store, and says on warn
// where the old file went. Nothing is deleted, so a key the user paid for is
// still on disk and still theirs to recover by hand.
//
// A file that cannot be READ — permissions, a device error — is still an error
// and is never moved: its bytes may not be recoverable by hand, and that is a
// different situation from "here are the bytes, they need a human".

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// corruptAuthStore writes body to a fresh auth.json and points OAICA_AUTH_FILE
// at it.
func corruptAuthStore(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OAICA_AUTH_FILE", path)
	return path
}

// quarantineOf returns the single `<path>.unreadable-*` file, or "".
func quarantineOf(t *testing.T, path string) string {
	t.Helper()
	matches, err := filepath.Glob(path + ".unreadable-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		return ""
	}
	if len(matches) > 1 {
		t.Fatalf("%d quarantined files for one store: %v", len(matches), matches)
	}
	return matches[0]
}

// The defect: a login over an unparseable store must succeed.
func TestALoginOverAnUnparseableStoreSucceeds(t *testing.T) {
	// A trailing comma: the shape a hand-edit takes, with credentials in it.
	body := `{
  "version": 1,
  "providers": {
    "zai-coding-plan": {"type": "api_key", "key": "sk-the-key-the-user-paid-for"},
  }
}`
	path := corruptAuthStore(t, body)

	var warn bytes.Buffer
	err := updateAuthStore(&warn, func(f *authStoreFile) error {
		f.Providers["minimax-coding-plan"] = authCredential{Type: authCredentialTypeAPIKey, Key: "sk-new", SavedAt: time.Now().UTC()}
		return nil
	})
	if err != nil {
		t.Fatalf("a login over an unparseable store failed (%v) — the store is write-only-never from then on, and the only way out is deleting credentials the user cannot read", err)
	}

	// The new credential landed.
	f, _, err := loadAuthStore()
	if err != nil {
		t.Fatalf("the store written after the quarantine is itself unreadable: %v", err)
	}
	if got := f.Providers["minimax-coding-plan"].Key; got != "sk-new" {
		t.Errorf("the new credential is %q, want sk-new — the login did not survive its own quarantine: %+v", got, f.Providers)
	}

	// And the old file is still there, byte for byte.
	moved := quarantineOf(t, path)
	if moved == "" {
		t.Fatal("the unparseable store was not kept — the credentials it held are gone, and refusing to write did less damage than this")
	}
	kept, err := os.ReadFile(moved)
	if err != nil {
		t.Fatalf("reading the quarantined store: %v", err)
	}
	if string(kept) != body {
		t.Errorf("the quarantined store is not the file that was there:\n got: %s\nwant: %s", kept, body)
	}
	if info, err := os.Stat(moved); err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("the quarantined store is mode %o, want 0600 — it holds credentials", info.Mode().Perm())
	}

	// And the user is told, with both paths named.
	out := warn.String()
	for _, want := range []string{path, moved} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning does not name %s:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "not valid JSON") {
		t.Errorf("the warning does not say why the store was moved aside:\n%s", out)
	}
}

// The store must be valid JSON afterwards, whatever the old bytes were.
func TestTheStoreIsValidJSONAfterAQuarantine(t *testing.T) {
	path := corruptAuthStore(t, "\x00\x01 not json at all")

	if err := updateAuthStore(io.Discard, func(f *authStoreFile) error {
		f.Providers["zai"] = authCredential{Type: authCredentialTypeAPIKey, Key: "sk-1"}
		return nil
	}); err != nil {
		t.Fatalf("updateAuthStore: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Errorf("the rewritten store is not valid JSON:\n%s", b)
	}
}

// A file that cannot be READ is not quarantined: moving it would guess at bytes
// nobody has seen.
func TestAnUnreadableStoreIsNotMovedAside(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, where mode 000 is still readable")
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"providers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OAICA_AUTH_FILE", path)

	var warn bytes.Buffer
	err := updateAuthStore(&warn, func(f *authStoreFile) error {
		f.Providers["zai"] = authCredential{Type: authCredentialTypeAPIKey, Key: "sk-1"}
		return nil
	})
	if err == nil {
		t.Fatal("a store that cannot be read did not fail the write")
	}
	if quarantineOf(t, path) != "" {
		t.Error("a store that cannot be READ was moved aside — its bytes were never seen, so nothing here can say the file deserved to move")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("the original file is gone: %v", statErr)
	}
}

// `oaica auth list` reports the state and names the way out, rather than
// reading as a dead end.
func TestAuthListOnAnUnparseableStoreNamesTheWayOut(t *testing.T) {
	path := corruptAuthStore(t, "{not json")

	err := AuthList(io.Discard)
	if err == nil {
		t.Fatal("auth list read an unparseable store without a word")
	}
	if !errors.Is(err, errAuthStoreUnparseable) {
		t.Errorf("auth list's error is %v, want the unparseable-store sentinel", err)
	}
	for _, want := range []string{path, "unreadable-"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q — the user is left with a parse error and no next step:\n%v", want, err)
		}
	}
}

// And the sentinel must not swallow the other read failure, or the quarantine
// would run for a store nobody could read.
func TestOnlyAParseErrorIsMarkedUnparseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OAICA_AUTH_FILE", path)
	_, _, err := loadAuthStore()
	if !errors.Is(err, errAuthStoreUnparseable) {
		t.Errorf("a parse failure returned %v, want the sentinel", err)
	}
}
