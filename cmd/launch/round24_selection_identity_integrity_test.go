package launch

// round24_selection_identity_integrity_test.go — what makes two spellings ONE
// selection, interrogated adversarially after round 23 changed the answer
// (2026-09-27 audit, round 24).
//
// modelNamesAreTheSame exists to keep a writer that stores the id its endpoint
// serves from reading back as drift forever. Round 23 added the alias clause
// for that; the clause compared childModelIDFor alone, which strips the
// endpoint out of the comparison — so the alias `kat` pointing at box1's
// `kat-awq` compared equal to box2's `kat-awq`, and the drift term declared
// the config current while the wrong remote's base URL and key stayed in place.
// The function's own contract, nine lines above it, says the opposite: "The two
// names have to resolve to the SAME endpoint and the same upstream model, so
// two remotes serving the same id are still different selections".
//
// The other half is the display spelling. Round 23's writers store the id the
// endpoint serves, so a launch spelled `ollama/llama3.2` (or `daemon/…`) reads
// back as `llama3.2` — and compared as drift on every run, reconfiguring the
// integration forever. The ollama/ prefix is display-only for a daemon-side row
// (findLaunchModel and launchModelMatches apply exactly that rule, round 21);
// router/ and oaica/ are NOT, because they pin the router.

import (
	"strings"
	"testing"
)

func TestAnAliasOnTwoRemotesIsNotOneSelection(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[
		{"name":"box1","base_url":"https://box1.example/v1","api_key":"sk-1"},
		{"name":"box2","base_url":"https://box2.example/v1","api_key":"sk-2"}]}`)

	old := resolveModelAlias
	resolveModelAlias = func(name string) (string, bool) {
		if name == "kat" {
			return "box1/kat-awq", true
		}
		return "", false
	}
	t.Cleanup(func() { resolveModelAlias = old })

	if modelNamesAreTheSame("box2/kat-awq", "kat") {
		t.Error("an alias pointing at box1 compared equal to box2's model of the same id: the drift term would call box2's endpoint current and neither its base URL nor its key would be rewritten")
	}
	if !modelNamesAreTheSame("box1/kat-awq", "kat") {
		t.Error("the alias and its own target compared as different selections")
	}
	if !managedLiveConfigDrifted("box2/kat-awq", "kat") {
		t.Error("managedLiveConfigDrifted said the config was current while it named another remote's endpoint")
	}
	if managedLiveConfigDrifted("box1/kat-awq", "kat") {
		t.Error("managedLiveConfigDrifted reported drift for the alias's own target")
	}
}

func TestTheDaemonSpellingOfALocalModelIsNotDrift(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box"}]}`)

	for _, tc := range []struct{ a, b string }{
		{"llama3.2", "ollama/llama3.2"}, // the picker's display prefix
		{"llama3.2", "daemon/llama3.2"}, // resolveLaunchEndpoint's daemon pin
		{"daemon/llama3.2", "ollama/llama3.2"},
	} {
		if !modelNamesAreTheSame(tc.a, tc.b) {
			t.Errorf("%q and %q compared as different selections: the writers store the id the endpoint serves, so the pair reads back as drift and every launch reconfigures", tc.a, tc.b)
		}
	}

	// router/ pins a DIFFERENT endpoint than the bare daemon id, so it is not
	// folded: a config that reconfigures beats one that silently keeps the
	// wrong endpoint.
	if modelNamesAreTheSame("router/kat-awq", "kat-awq") {
		t.Error("router/kat-awq compared equal to the bare daemon id: router/ pins the router, not the daemon")
	}
}

func TestAUserRemoteNamedOllamaKeepsItsNamespace(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	// A user remote literally named "ollama": its namespace IS its identity
	// (the round-21 rule findLaunchModel applies), so `ollama/<id>` is that
	// remote's model and not the daemon's bare id.
	writeRemotes(t, `{"remotes":[{"name":"ollama","base_url":"https://ollama.example/v1","api_key":"sk-o"}]}`)

	if modelNamesAreTheSame("ollama/llama3.2", "llama3.2") {
		t.Error("a user remote named \"ollama\" claimed the daemon's bare id as its own model: its base URL and key would be dropped as a no-op")
	}
}

// ---------------------------------------------------------------------------
// The refusal that exists for exactly this did not fire.
//
// `oaica model alias kat --target box/kat-awq` + `oaica launch chatgpt
// --model kat`: codexAppRejectNonDaemonModels looks the primary up with
// findLaunchModel (exact names only) while codexAppWriteModelID resolves the
// alias, so typing the model's own spelling was refused and typing the alias
// for it was not. The app then posted the REMOTE's model id to the DAEMON's
// base URL — the mixed identity, written into the user's config.
// ---------------------------------------------------------------------------

func TestTheChatGPTAppRefusesAnAliasToAnotherEndpoint(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box"}]}`)

	old := resolveModelAlias
	resolveModelAlias = func(name string) (string, bool) {
		if name == "kat" {
			return "box/kat-awq", true
		}
		return "", false
	}
	t.Cleanup(func() { resolveModelAlias = old })

	rows := []LaunchModel{{Name: "box/kat-awq", Remote: true}, {Name: "llama3.2"}}

	// The control: the model typed by its own spelling is refused (this has
	// been true since round 20).
	if err := (&CodexApp{}).ConfigureWithModels("box/kat-awq", rows); err == nil {
		t.Fatal("control: the remote's own spelling was accepted, so this test proves nothing")
	}

	err := (&CodexApp{}).ConfigureWithModels("kat", rows)
	if err == nil {
		t.Fatal("the ChatGPT app accepted an alias pointing at another endpoint: its config names the local daemon and has no credential field, so the app would post the remote's model id to the daemon")
	}
	if !strings.Contains(err.Error(), "kat") && !strings.Contains(err.Error(), "kat-awq") {
		t.Errorf("the refusal does not name the model: %v", err)
	}
}

// ---------------------------------------------------------------------------
// An alias to a remote must not be written as a daemon row by the writers
// whose refusal half asks the same question.
// ---------------------------------------------------------------------------

func TestTheAliasToARemoteIsRefusedWhereItsSpellingIs(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box"}]}`)

	old := resolveModelAlias
	resolveModelAlias = func(name string) (string, bool) {
		if name == "kat" {
			return "box/kat-awq", true
		}
		return "", false
	}
	t.Cleanup(func() { resolveModelAlias = old })

	rows, _ := resolveLaunchModels([]string{"kat"}, []LaunchModel{{Name: "box/kat-awq", Remote: true}, {Name: "llama3.2"}})
	if len(rows) != 1 {
		t.Fatalf("resolveLaunchModels returned %d rows, want 1", len(rows))
	}
	if rows[0].Name != "box/kat-awq" || !rows[0].Remote {
		t.Fatalf("the alias resolved to %+v, want the box row — every writer downstream decides by the row", rows[0])
	}

	// The row is what every writer reads, so the id helpers must name the id
	// that endpoint answers to. (The auditor probed these with the RAW spelling,
	// which the launcher never hands a writer.)
	row := rows[0]
	for _, tc := range []struct {
		name string
		got  string
	}{
		{"opencodeModelID", opencodeModelID(row)},
		{"piModelIDFor", piModelIDFor(row)},
		{"ompLaunchModelID", ompLaunchModelID(row)},
		{"clineModelIDFor", clineModelIDFor(row.Name)},
	} {
		if tc.got != "kat-awq" {
			t.Errorf("%s(row) = %q, want %q — the box endpoint knows the model by its bare id", tc.name, tc.got, "kat-awq")
		}
	}

	// The single-endpoint harnesses never reach an id helper with this row:
	// they refuse it, and the refusal asks daemonRoutedModel — a question that
	// is only answerable from the row (2026-09-27 audit, rounds 18 and 20).
	if err := deepSeekHarnessRejectNonDaemonModels("kat", rows); err == nil {
		t.Error("the DeepSeek Harness accepted an alias to a user remote: its settings name the local daemon and one credential")
	}

	// Openclaw refuses a user remote in Run AND in Edit; the Edit refusal is
	// the one that matters, because Edit runs first and used to write the whole
	// config before Run refused it (round 16). It asks by endpoint, so it sees
	// whatever the row says.
	if err := (&Openclaw{}).Edit(rows); err == nil {
		t.Error("Openclaw.Edit accepted an alias to a user remote: it would rewrite the user's config for a model the daemon cannot serve, and only then would Run refuse")
	}
}
