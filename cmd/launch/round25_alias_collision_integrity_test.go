package launch

// round25_alias_collision_integrity_test.go — an alias whose own spelling is
// ALSO a row (2026-09-27 audit, round 25).
//
// Round 24 gave findLaunchModel an alias hop so the refusals see the row that
// serves the alias. The hop ran SECOND: findLaunchModelExact matched the name
// first, so when the inventory carried a row under the alias's OWN spelling —
// the daemon serving a model named `gemma3` while the user's alias `gemma3`
// points at box's `gemma3-ft` — the exact match won and the refusals were told
// the launch was a daemon model. The write path resolves the alias
// (childModelIDFor), so the store got the box's model id beside the daemon's
// base URL: the mixed identity the round-24 commit claims to have stopped, one
// collision away. model_alias.go states the rule the hop now follows: "an alias
// always wins if defined ... not a different thing that happens to share the
// bare id".

import (
	"strings"
	"testing"
)

func withAlias(t *testing.T, alias, target string) {
	t.Helper()
	old := resolveModelAlias
	resolveModelAlias = func(name string) (string, bool) {
		if name == alias {
			return target, true
		}
		return "", false
	}
	t.Cleanup(func() { resolveModelAlias = old })
}

func TestAnAliasWhoseOwnSpellingIsADaemonRowStillWins(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box"}]}`)
	withAlias(t, "gemma3", "box/gemma3-ft")

	// The daemon has a model whose id is the alias's own spelling, and box has
	// the model the alias points at.
	rows := []LaunchModel{
		{Name: "gemma3", LiveSource: liveSourceDaemon},
		{Name: "llama3.2", LiveSource: liveSourceDaemon},
		{Name: "box/gemma3-ft", Remote: true},
	}

	got, ok := findLaunchModel(rows, "gemma3")
	if !ok || got.Name != "box/gemma3-ft" {
		t.Fatalf("findLaunchModel(gemma3) = %+v (ok=%v), want the row the alias points at — a row that merely shares the alias's spelling is not what the alias means", got, ok)
	}

	if err := codexAppRejectNonDaemonModels("gemma3", rows); err == nil {
		t.Error("the ChatGPT app accepted an alias to a remote because a daemon row shared the alias's own spelling: the refusal saw the daemon row while the writer would store the remote's model id beside the daemon's base URL")
	}
	if err := (&CodexApp{}).ConfigureWithModels("gemma3", rows); err == nil {
		t.Error("the ChatGPT app configured itself for an alias to a remote beside a daemon row of the same spelling")
	}

	// Control: the unrelated local model is still accepted, so this is about
	// the alias and not about the list.
	if err := codexAppRejectNonDaemonModels("llama3.2", rows); err != nil {
		t.Fatalf("control: an ordinary local model was refused: %v", err)
	}
}

// TestAnAliasToTheDaemonIsNotDriftAgainstTheDaemonsSpelling is the other
// direction of the same ordering: an alias to a DAEMON model, written by the
// writer as the daemon id, must still compare as one selection — the alias hop
// may not turn a genuine match into drift.
func TestAnAliasToTheDaemonIsNotDriftAgainstTheDaemonsSpelling(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box"}]}`)
	withAlias(t, "fast", "llama3.2")

	if !modelNamesAreTheSame("fast", "llama3.2") {
		t.Error("an alias to a daemon model and the daemon's own id compared as different selections")
	}
	if !modelNamesAreTheSame("fast", "ollama/llama3.2") {
		t.Error("an alias to a daemon model and the picker spelling of the same id compared as different selections")
	}
	if managedLiveConfigDrifted("llama3.2", "fast") {
		t.Error("managedLiveConfigDrifted reported drift for an alias and the daemon model it points at")
	}
}

// TestAnAliasNeverEqualsTheDaemonModelThatSharesItsName: the fold-then-compare
// used to run BEFORE the alias clause, so an alias spelled like a daemon model
// compared equal to that model — and a store naming the daemon read as current
// for a launch aimed at the remote. Different endpoints are never one
// selection.
func TestAnAliasNeverEqualsTheDaemonModelThatSharesItsName(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box"}]}`)
	withAlias(t, "kat", "box/kat-awq")

	// The daemon's spellings of its own id. Bare "kat" is not in this list on
	// purpose: it is the same string as the alias, and nothing in a stored name
	// distinguishes "the daemon's kat" from "the alias kat" — the alias wins on
	// both sides and the pair compares equal, which is the only answer any
	// implementation can give. "ollama/kat" and "daemon/kat" DO carry that
	// information (they pin the daemon), and the fold erased it.
	for _, daemonSpelling := range []string{"ollama/kat", "daemon/kat"} {
		if modelNamesAreTheSame("kat", daemonSpelling) {
			t.Errorf("the alias kat (box's kat-awq) compared equal to the daemon's %q: the store would read as current while the launch is aimed at the box", daemonSpelling)
		}
		if !managedLiveConfigDrifted(daemonSpelling, "kat") {
			t.Errorf("managedLiveConfigDrifted said the config was current for the daemon spelling %q while the alias points at the box", daemonSpelling)
		}
	}

	// The alias and its own target are still one selection.
	if !modelNamesAreTheSame("kat", "box/kat-awq") {
		t.Error("the alias and the model it points at compared as different selections")
	}
	if managedLiveConfigDrifted("box/kat-awq", "kat") {
		t.Error("managedLiveConfigDrifted reported drift for the alias's own target")
	}
}

// TestADaemonRowIsTheDaemonsWhateverItsNameSpells: the router pin is a
// name-shaped rule, and a row that carries its provenance must not be
// reclassified by its id. A user who pulled a model literally named
// `oaica-small-7b` had it refused as a router SKU, with a message saying the
// daemon does not have it — while resolveLaunchEndpoint resolves that very
// name to the daemon.
func TestADaemonRowIsTheDaemonsWhateverItsNameSpells(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box"}]}`)

	local := LaunchModel{Name: "oaica-small-7b", LiveSource: liveSourceDaemon}
	rows := []LaunchModel{local, {Name: "llama3.2", LiveSource: liveSourceDaemon}}

	if !daemonRoutedModel(local) {
		t.Error("a row the daemon listed was keyed away from the daemon by its name: the writer refuses a model the endpoint it names actually serves")
	}
	if reason := nonDaemonRowReason(local); strings.Contains(reason, "OAICA router") {
		t.Errorf("the refusal for a daemon row claims the router: %q", reason)
	}
	if err := codexAppRejectNonDaemonModels("oaica-small-7b", rows); err != nil {
		t.Errorf("the ChatGPT app refused a model the local daemon serves: %v", err)
	}

	// Control: the same spelling with no provenance is a router SKU, and is
	// refused — the pin still exists where the name is the only evidence.
	if daemonRoutedModel(LaunchModel{Name: "oaica-small-7b"}) {
		t.Error("a router SKU with no row provenance was keyed as a daemon model")
	}
}
