package launch

// picker_needs_key.go — what the picker says when a search finds nothing.
//
// The problem this solves: a user searches the picker for the plan they pay
// for ("minimax", "zai") and gets "(no matches)". The provider is known — its
// row is simply gated on a credential nothing supplies, so its models are not
// in the list (builtinRemotes, user_remotes.go). The gating is correct: with
// no credential there is nothing to route through, and a launch would fail
// upstream. Silence was the bug. The user concludes oaica does not support the
// provider, when the truth is one command away.
//
// Why not a picker ROW ("needs key: minimax-coding-plan")? Because a picker
// must not list rows that cannot be launched — that is a deliberate, tested
// property of this fork (picker_catalog_test.go: an install with no keys shows
// the local models and nothing pretending to be launchable). A list of thirty
// unusable rows on a fresh install is worse than the silence it replaces. The
// hint therefore belongs in the "(no matches)" state, where it costs nothing
// until the user has demonstrated intent by searching for a name.
//
// The wording and the login command come from the same helpers `oaica auth
// list` uses (auth_external.go, needsKeyInstruction), so a provider's fix is
// described identically everywhere it is surfaced.

import (
	"os"
	"strings"
)

// PickerNoMatchHint returns the extra line the picker shows under "(no
// matches)" for a query, or "" when the query means nothing to us. It is the
// hook cmd/cmd.go installs as tui.NoMatchHint, keeping the catalog knowledge in
// this package (the TUI renders text it is given, it does not read catalogs).
//
// A query that matches a provider we already HAVE a credential for returns "":
// its models were in the list, so "(no matches)" means the user mistyped, not
// that something needs unlocking.
func PickerNoMatchHint(query string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	if len(q) < 2 {
		// One character is not intent — it is the first keystroke of a typed
		// name, and answering it with advice would flash a hint on every query
		// the user types.
		return ""
	}
	var matches []string
	for _, e := range providerCatalog() {
		if !strings.Contains(strings.ToLower(e.Name), q) {
			continue
		}
		if providerCatalogEntryUsable(e) {
			continue
		}
		matches = append(matches, e.Name)
	}
	switch len(matches) {
	case 0:
		return ""
	case 1:
		e, _ := knownAuthProvider(matches[0])
		cred, hasExternal := externalAuthFor(e.AuthVia, e.Name)
		return matches[0] + " is a provider oaica knows — " + needsKeyInstruction(e, hasExternal, cred)
	default:
		// Several (a family like minimax/minimax-coding-plan/minimax-cn-*):
		// name them and give the command shape once, rather than a paragraph of
		// near-identical sentences the user cannot fit on one line anyway.
		return "known providers needing a key: " + strings.Join(matches, ", ") +
			" — run: oaica auth login <name> (see `oaica auth list` for each one's options)"
	}
}

// needsKeyInstruction is the actionable half of the hint, and the same text
// `oaica auth list` shows for the same provider. It prefers the specific
// reason when a credential exists but cannot be used ("expired"), because "log
// in again" and "log in" are different instructions.
func needsKeyInstruction(e providerCatalogEntry, hasExternal bool, cred externalCredential) string {
	if hasExternal && cred.Reason != "" {
		return cred.Reason
	}
	if e.AuthVia != "" && externalStoreExists(e.AuthVia) {
		if cmd := externalLoginArgvString(e.AuthVia, e.Name); cmd != "" {
			return "needs a key — run: " + cmd
		}
	}
	if e.APIKeyEnv == "" && e.AuthVia == "" {
		// Nothing key-shaped gates this row and no external store is declared,
		// so there is no command to name.
		return "not usable with this build's configuration"
	}
	return "needs a key — run: oaica auth login " + e.Name
}

// providerCatalogEntryUsable reports whether a catalog row can authenticate
// right now: an env var, an `oaica auth login` credential, or a login reused
// from another tool (auth_external.go). Mirrors builtinRemotes' gate — the
// hint must agree with the picker about what is missing, or it would advise a
// user whose provider already works.
func providerCatalogEntryUsable(e providerCatalogEntry) bool {
	if e.APIKeyEnv != "" && strings.TrimSpace(os.Getenv(e.APIKeyEnv)) != "" {
		return true
	}
	if hasStoredAuth(e.Name) {
		return true
	}
	if cred, ok := externalAuthFor(e.AuthVia, e.Name); ok && cred.Key != "" {
		return true
	}
	return false
}
