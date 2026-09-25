package launch

import (
	"strings"
	"testing"
)

// The bug this file fixes: searching the picker for the plan a user pays for
// returned a bare "(no matches)", which reads as "oaica does not support it".
// The provider is known and one command away.
func TestPickerNoMatchHint_ExplainsGatedProvider(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	useOpencodeStore(t, `{}`)

	hint := PickerNoMatchHint("minimax")
	if hint == "" {
		t.Fatal("searching a known-but-unlockable provider gave no hint")
	}
	if !strings.Contains(hint, "minimax") {
		t.Fatalf("hint = %q, want it to name the provider the user searched for", hint)
	}
	if !strings.Contains(hint, "oaica auth login") && !strings.Contains(hint, "opencode auth login") {
		t.Fatalf("hint = %q, want the command that makes it usable", hint)
	}
	// Case and surrounding space are the user's, not ours.
	if got := PickerNoMatchHint("  MiniMax  "); got != hint {
		t.Fatalf("hint for a differently-cased query = %q, want %q", got, hint)
	}
}

// A provider oaica can already authenticate must not be described as needing a
// key — its models were in the list, so "(no matches)" means a typo.
func TestPickerNoMatchHint_SilentForUsableProviders(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	useOpencodeStore(t, `{}`)

	if got := PickerNoMatchHint("zai"); got == "" {
		t.Fatal("expected a hint while zai has no credential")
	}
	t.Setenv(zaiEnvKey, "env-key")
	if got := PickerNoMatchHint("zai"); got != "" {
		t.Fatalf("hint = %q for a provider supplied by its env var, want none", got)
	}

	// And the same for a credential reused from another tool.
	clearAllCatalogKeys(t)
	useOpencodeStore(t, `{"zai-coding-plan":{"type":"api","key":"oc-key"}}`)
	if got := PickerNoMatchHint("zai-coding-plan"); got != "" {
		t.Fatalf("hint = %q for a provider another tool already logged in, want none", got)
	}
}

// The instruction must point at whichever login actually serves the user: the
// tool already installed on this machine, else oaica's own.
func TestPickerNoMatchHint_InstructionFollowsInstalledTool(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)

	useOpencodeStore(t, `{}`) // store present, no entry for this provider
	if got := PickerNoMatchHint("minimax-coding-plan"); !strings.Contains(got, "opencode auth login minimax-coding-plan") {
		t.Fatalf("hint = %q, want opencode's own login while opencode is installed", got)
	}

	t.Setenv("OPENCODE_AUTH_FILE", "/nonexistent/opencode/auth.json")
	if got := PickerNoMatchHint("minimax-coding-plan"); !strings.Contains(got, "oaica auth login minimax-coding-plan") {
		t.Fatalf("hint = %q, want `oaica auth login` when no other tool is installed", got)
	}
}

// An expired reused credential is a different instruction from "log in": the
// user must re-authenticate where the credential lives.
func TestPickerNoMatchHint_ExpiredReusedCredentialSaysSo(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	useOpencodeStore(t, `{"zai-coding-plan":{"type":"oauth","access":"tok","refresh":"r","expires":1}}`)

	hint := PickerNoMatchHint("zai-coding-plan")
	if !strings.Contains(hint, "expired") {
		t.Fatalf("hint = %q, want it to say the stored credential expired rather than asking for a first login", hint)
	}
}

// One-character queries are the first keystroke of a typed name, not intent:
// flashing advice on every keystroke would be worse than the silence.
func TestPickerNoMatchHint_NeedsRealQuery(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	useOpencodeStore(t, `{}`)

	for _, q := range []string{"", " ", "z", "m"} {
		if got := PickerNoMatchHint(q); got != "" {
			t.Fatalf("PickerNoMatchHint(%q) = %q, want none", q, got)
		}
	}
}

// A query matching several gated providers gets one line, not one paragraph
// per provider.
func TestPickerNoMatchHint_MultipleMatchesStayOneLine(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	useOpencodeStore(t, `{}`)

	hint := PickerNoMatchHint("coding-plan")
	if hint == "" {
		t.Fatal("expected a hint for a query matching several plan providers")
	}
	if strings.Count(hint, "\n") != 0 {
		t.Fatalf("hint is multi-line: %q", hint)
	}
	if !strings.Contains(hint, "oaica auth login <name>") {
		t.Fatalf("hint = %q, want one actionable command shape for the whole family", hint)
	}
	if !strings.Contains(hint, "minimax-coding-plan") {
		t.Fatalf("hint = %q, want every matching provider named", hint)
	}
}

// A query that matches nothing at all stays a plain "(no matches)": inventing
// advice for a typo is noise.
func TestPickerNoMatchHint_NothingKnown(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	useOpencodeStore(t, `{}`)

	for _, q := range []string{"qqzzxx", "not-a-provider-at-all"} {
		if got := PickerNoMatchHint(q); got != "" {
			t.Fatalf("PickerNoMatchHint(%q) = %q, want none", q, got)
		}
	}
}

// The provider catalog is the single source of truth for "what oaica knows":
// a provider added to providers.json gets a hint with no code change here.
func TestPickerNoMatchHint_FollowsTheCatalog(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	useOpencodeStore(t, `{}`)

	n := 0
	for _, e := range providerCatalog() {
		if providerCatalogEntryUsable(e) {
			continue
		}
		if got := PickerNoMatchHint(e.Name); got == "" {
			t.Fatalf("catalog provider %q is unusable but yields no hint", e.Name)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no unusable providers in the catalog — this test verified nothing")
	}
}
