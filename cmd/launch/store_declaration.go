package launch

// store_declaration.go — the drift question, asked in the store's own
// vocabulary (2026-09-27 audit, round 27).
//
// A launch skips rewriting an integration's config when the config already
// holds the selection, and that answer has to come from the store: the picker
// name a launch saves is a display label for some rows ("ollama/gpt-oss" is
// served as "gpt-oss:cloud"), so a flat comparison against the store's ids is
// false forever and every launch rewrites a config that did not change.
//
// Two rounds of flat vocabularies followed. Round 26 compared the store's ids
// against the ids the launch would write, but asked only that the store
// CONTAIN them — which made a store holding more than the selection read as
// current, so an editor whose writers own a second field (OpenClaw's
// agents.defaults.model.primary) kept pointing at a model the launch did not
// choose while the rewrite that would have fixed it was skipped (round 27,
// F1). Round 26 also read every store as a list of ids, and an id is not an
// identity: opencode keys a model by the provider BLOCK that declares it and
// Cline records the ENDPOINT beside it, so the same id under the daemon's
// block and under a remote's — or beside the daemon's base URL and beside a
// remote's — is a different model, and the launch left the config dialling the
// wrong one (round 27, F3 and F4).
//
// So the question is not "does the store contain these ids" but "does each of
// this editor's stores already hold what a write of these rows would leave",
// and only the editor can answer it: what its writers publish, whether they
// keep rows the selection does not name, and what half of a model's identity
// its store records are all facts about that integration.
type storeDeclarationEditor interface {
	// DeclaresSelection reports whether this editor's stores already hold what
	// Edit(models) would leave, read in the store's own vocabulary. It answers
	// false whenever it cannot tell — an unreadable store, an empty selection,
	// a selection this editor's stores cannot hold whole — because "cannot
	// tell" must mean "write", not "leave it".
	DeclaresSelection(models []LaunchModel) bool
}

// sameStoreStrings reports whether two store key lists are the same list:
// same length, same order, same keys. Store keys carry their own normalisation
// (the editor that builds them trims and joins), so this compares them as they
// come.
func sameStoreStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func selectionContains(held, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range held {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return len(want) > 0
}

// declaresPrefix reports whether held begins with want: the store's own
// entries, in order, are the selection's — anything after them is history the
// editor's writer keeps, which is not drift (opencode's recent list, OpenClaw's
// provider list, where the rows after the selection are the user's own models).
func declaresPrefix(held, want []string) bool {
	if len(held) < len(want) {
		return false
	}
	return sameStoreStrings(held[:len(want)], want)
}
