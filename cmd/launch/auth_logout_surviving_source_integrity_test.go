package launch

// auth_logout_surviving_source_integrity_test.go — `oaica auth logout` reported
// a completed removal while the provider went on authenticating from another
// store (2026-09-26 audit, fifth round).
//
// AuthLogout deletes the key in ~/.oaica/auth.json and prints "Removed the
// stored key for <provider>." It already knew to warn about the environment —
// a key exported in the shell keeps working — but not about `auth_via`
// (provider_catalog.go), the declared reuse of another agent CLI's login. For a
// row like groq/deepseek/xai/zai-coding-plan, whose catalog entry names
// opencode, a user who ran `oaica auth login groq` and then `oaica auth logout
// groq` read a confirmation, saw the provider come back "ready" in `oaica auth
// list` and in the picker, and had no way to tell that a second store they
// never touched is what still authenticates it.
//
// The credential is not ours to delete — opencode owns that file, and its
// `auth logout` is the command that removes it. The fix is to say so.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuthLogoutNamesTheSurvivingExternalSource
//
// groq's catalog row declares auth_via: opencode. With a key in both stores,
// the removal message must not read as "groq is now unconfigured".
func TestAuthLogoutNamesTheSurvivingExternalSource(t *testing.T) {
	const (
		provider = "groq"
		external = "sk-OPENCODE-OWNED-77889900"
		stored   = "sk-OAICA-STORED-11223344"
	)
	dir := t.TempDir()
	ext := filepath.Join(dir, "opencode-auth.json")
	if err := os.WriteFile(ext, []byte(`{"`+provider+`":{"type":"api","key":"`+external+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_AUTH_FILE", ext)
	t.Setenv("OAICA_AUTH_FILE", filepath.Join(dir, "auth.json"))
	t.Setenv("GROQ_API_KEY", "")

	entry, known := knownAuthProvider(provider)
	if !known || entry.AuthVia == "" {
		t.Fatalf("the test needs a catalog row with auth_via; %s has %q (known=%v) — the defect it reproduces is unreachable without one", provider, entry.AuthVia, known)
	}

	var out bytes.Buffer
	if err := AuthLogin(&out, provider, stored); err != nil {
		t.Fatalf("auth login: %v", err)
	}
	out.Reset()
	if err := AuthLogout(&out, provider); err != nil {
		t.Fatalf("auth logout: %v", err)
	}
	msg := out.String()

	// The premise: the provider really does still authenticate afterwards,
	// from the store logout did not touch.
	if key := externalAuthKey(entry.AuthVia, provider); key == "" {
		t.Fatalf("the external store did not hold a usable credential, so the test proves nothing (msg %q)", msg)
	}

	if !strings.Contains(msg, entry.AuthVia) {
		t.Errorf("auth logout printed %q — the provider still authenticates through auth_via=%q (%s), so a confirmation that names only the store it deleted tells the user groq is unconfigured when it is not",
			msg, entry.AuthVia, ext)
	}
	if !strings.Contains(msg, provider) {
		t.Errorf("auth logout printed %q, which does not name the provider the surviving credential belongs to", msg)
	}
}

// The control: with no external credential for the provider, the message must
// stay a plain confirmation. A warning that fires when nothing survives it is
// noise the user learns to ignore.
func TestAuthLogoutStaysQuietWithoutASurvivingSource(t *testing.T) {
	const provider = "groq"
	dir := t.TempDir()
	ext := filepath.Join(dir, "opencode-auth.json")
	// A store that exists but has no entry for this provider.
	if err := os.WriteFile(ext, []byte(`{"some-other-provider":{"type":"api","key":"sk-OTHER-1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_AUTH_FILE", ext)
	t.Setenv("OAICA_AUTH_FILE", filepath.Join(dir, "auth.json"))
	t.Setenv("GROQ_API_KEY", "")

	var out bytes.Buffer
	if err := AuthLogin(&out, provider, "sk-OAICA-ONLY-55667788"); err != nil {
		t.Fatalf("auth login: %v", err)
	}
	out.Reset()
	if err := AuthLogout(&out, provider); err != nil {
		t.Fatalf("auth logout: %v", err)
	}
	msg := out.String()

	if !strings.Contains(msg, "Removed the stored key for "+provider) {
		t.Errorf("auth logout printed %q, want the plain confirmation when nothing else holds a credential", msg)
	}
	if strings.Contains(msg, "opencode") {
		t.Errorf("auth logout printed %q — naming opencode when it holds no credential for %s is a false alarm", msg, provider)
	}
}
