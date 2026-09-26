package launch

// store_unknown_members_integrity_test.go — a command that changed nothing
// still rewrote its store, and every top-level member the struct does not
// model was destroyed by the rewrite (2026-09-26 audit).
//
// The four store writers (remotes.json, auth.json, the aliases file,
// plans.json) each save() unconditionally after their mutate closure, and each
// struct is a PARTIAL view of the document: it models the members the client
// knows and nothing else. A user who adds a note, a label, or a field from a
// newer version of the file has it silently deleted by the next command — and
// the deletion is not even coupled to a change. `oaica remote rm <typo>`
// reports "no remote named ..." with exit 1 and still re-serialises the file;
// `oaica auth logout <provider not logged in>` reports success, having
// removed nothing but someone else's field. models.json was protected against
// exactly this by the updateModelManifest "changed" gate; these four were not.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStoreFile lays a document down at path and points the store's env
// override at it.
func storeFixture(t *testing.T, env, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(env, path)
	return path
}

func storeBytes(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the store is gone: %v", err)
	}
	return string(b)
}

// A removal of something that was never there must leave the file exactly as
// it found it.
func TestANoOpRemovalLeavesEveryStoreByteIdentical(t *testing.T) {
	cases := []struct {
		env, file, body string
		run             func() error
	}{
		{
			"OAICA_REMOTES_FILE", "remotes.json",
			"{\n  \"remotes\": [],\n  \"schema_note\": \"keep\",\n  \"my_own_label\": \"mine\"\n}\n",
			func() error { _, err := RemoteRemove("nope"); return err },
		},
		{
			"OAICA_AUTH_FILE", "auth.json",
			"{\n  \"version\": 1,\n  \"providers\": {},\n  \"_note\": \"mine\"\n}\n",
			func() error { return AuthLogout(io.Discard, "not-logged-in") },
		},
		{
			"OAICA_ALIASES_FILE", "aliases.json",
			"{\n  \"version\": 1,\n  \"aliases\": {},\n  \"_note\": \"mine\"\n}\n",
			func() error { _, err := ModelAliasRemove("nope"); return err },
		},
		{
			"OAICA_PLANS_FILE", "plans.json",
			"{\n  \"version\": 1,\n  \"profiles\": {},\n  \"_note\": \"mine\"\n}\n",
			func() error { _, err := PlanRemove("nope"); return err },
		},
	}
	for _, tc := range cases {
		path := storeFixture(t, tc.env, tc.file, tc.body)
		before := storeBytes(t, path)
		if err := tc.run(); err != nil {
			t.Fatalf("%s: the no-op removal failed: %v", tc.file, err)
		}
		if after := storeBytes(t, path); after != before {
			t.Errorf("%s was rewritten by a command that removed nothing:\n--- before ---\n%s\n--- after ---\n%s\n— the rewrite re-serialises a struct that models only part of the document, so every hand-added member (`_note`, a label, a field from a newer version) is destroyed on a command that reported no change", tc.file, before, after)
		}
	}
}

// And a command that DOES change something must keep the members it does not
// model: the fix cannot be "never write".
func TestARealChangeKeepsTheHandAddedMembers(t *testing.T) {
	path := storeFixture(t, "OAICA_REMOTES_FILE", "remotes.json",
		"{\n  \"remotes\": [],\n  \"schema_note\": \"keep\",\n  \"my_own_label\": \"mine\"\n}\n")
	if _, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: "https://box.example.com/v1", Wire: "openai"}); err != nil {
		t.Fatalf("RemoteAdd: %v", err)
	}
	got := storeBytes(t, path)
	for _, want := range []string{"schema_note", "my_own_label", `"box"`} {
		if !strings.Contains(got, want) {
			t.Errorf("remotes.json after a real add lost %s:\n%s", want, got)
		}
	}

	aliasPath := storeFixture(t, "OAICA_ALIASES_FILE", "aliases.json",
		"{\n  \"version\": 1,\n  \"aliases\": {},\n  \"_note\": \"mine\"\n}\n")
	if err := ModelAliasSet("fast", "box/kat-awq"); err != nil {
		t.Fatalf("ModelAliasSet: %v", err)
	}
	if got := storeBytes(t, aliasPath); !strings.Contains(got, "_note") || !strings.Contains(got, "fast") {
		t.Errorf("the aliases file lost either the hand-added member or the alias it was asked to add:\n%s", got)
	}

	planPath := storeFixture(t, "OAICA_PLANS_FILE", "plans.json",
		"{\n  \"version\": 1,\n  \"profiles\": {},\n  \"_note\": \"mine\"\n}\n")
	if err := PlanSet("p1", TierPlanProfile{Model: "box/kat-awq"}); err != nil {
		t.Fatalf("PlanSet: %v", err)
	}
	if got := storeBytes(t, planPath); !strings.Contains(got, "_note") || !strings.Contains(got, "p1") {
		t.Errorf("plans.json lost either the hand-added member or the plan it was asked to add:\n%s", got)
	}
}
