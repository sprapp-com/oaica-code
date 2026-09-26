package launch

// declared_window_integrity_test.go — providers.json declares each vendor's
// model windows ("gpt-5": 400000 context), and remoteLaunchModels consults
// that table for ids the SWEEP does not return. A vendor whose /v1/models
// answers (almost all of them) lists ids only — no window, ever — so the same
// id arriving from the sweep used to lose the declared window entirely: the
// row came out with ContextLength 0 and the launch fell back to
// defaultAgentContextLength (128000), a number the catalog contradicts
// (2026-09-26 audit, third round). The sweep should win the id collision, as
// its comment says; the window is not part of what it collided on.

import "testing"

func TestSweptModelKeepsItsDeclaredWindow(t *testing.T) {
	declared := providerCatalogDeclaredModels("openai")
	if len(declared) == 0 {
		t.Skip("the catalog no longer declares windows for openai")
	}
	if _, ok := declared["gpt-5"]; !ok {
		t.Skip("the catalog no longer declares gpt-5")
	}

	r := userRemote{Name: "openai", BaseURL: "https://api.openai.com/v1", CatalogOrigin: true}
	models, err := remoteLaunchModels(r, []string{"gpt-5", "gpt-5-mini", "an-undocumented-id"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]LaunchModel{}
	for _, m := range models {
		got[m.Name] = m
	}

	for _, id := range []string{"gpt-5", "gpt-5-mini"} {
		m, ok := got["openai/"+id]
		if !ok {
			t.Fatalf("the swept id %q produced no row", id)
		}
		want := declared[id]
		if m.ContextLength != want.Context || m.MaxOutputTokens != want.Output {
			t.Errorf("swept %q: context/output = %d/%d, want the declared %d/%d — a sweep lists ids, not windows, and dropping the declared one sends the launch to the 128k default",
				id, m.ContextLength, m.MaxOutputTokens, want.Context, want.Output)
		}
	}

	// An id the catalog does not declare keeps 0 (unknown), not a guess.
	if m := got["openai/an-undocumented-id"]; m.ContextLength != 0 {
		t.Errorf("an undeclared swept id got context %d — unknown must stay unknown", m.ContextLength)
	}
}
