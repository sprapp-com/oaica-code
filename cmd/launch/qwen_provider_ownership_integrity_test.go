package launch

// qwen_provider_ownership_integrity_test.go — three defects in Qwen's provider
// bookkeeping (2026-09-27 audit, round 22).
//
// 1. "Ours" was scoped to the base URL being configured now or the daemon, so
//    the entry a previous launch had written for ANOTHER remote never matched
//    and never went away: one dead provider per distinct remote accumulated in
//    settings.json, all carrying envKey OLLAMA_API_KEY — which the next launch
//    sets to the new endpoint's key. The stale entry then attaches that key to
//    someone else's host on any later qwen run.
//
// 2. qwenIsOllamaProvider deleted an entry on envKey + the daemon's base URL
//    alone, while its own doc comment (and the merge rule beside it) requires
//    the " (Ollama)" suffix before an entry counts as ours: a user's hand-written
//    provider pointing at the daemon was deleted by an unrelated launch.
//
// 3. qwenConfiguredRemoteForBase took the FIRST remote with that base URL, so
//    two accounts on one host were indistinguishable and the read-back named
//    whichever came first in the file — the other account's picker name, whose
//    launch then reconfigures a key the user never chose. Ambiguity is refused
//    elsewhere in this package (resolveBareRemoteModel) and is refused here now.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func qwenSettingsPath(t *testing.T, home string) string {
	t.Helper()
	return filepath.Join(home, ".qwen", "settings.json")
}

func readQwenSettings(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(qwenSettingsPath(t, home))
	if err != nil {
		t.Fatalf("read qwen settings: %v", err)
	}
	return string(data)
}

func TestQwenDropsItsOwnEntryFromAnEarlierRemote(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"https://box.example/v1","api_key":"k-box","tool_format":"tool_calls"},
		{"name":"other","base_url":"https://other.example/v1","api_key":"k-other","tool_format":"tool_calls"}]}`)

	q := &Qwen{}
	if err := q.Configure("other/big-model"); err != nil {
		t.Fatalf("configure other/big-model: %v", err)
	}
	if !strings.Contains(readQwenSettings(t, home), "https://other.example/v1") {
		t.Fatal("premise: the first launch did not write its remote endpoint")
	}

	if err := q.Configure("box/model-a"); err != nil {
		t.Fatalf("configure box/model-a: %v", err)
	}

	text := readQwenSettings(t, home)
	if strings.Contains(text, "https://other.example/v1") {
		t.Errorf("the provider oaica wrote for another remote is still in settings.json, so the next qwen run attaches this launch's OLLAMA_API_KEY to that host:\n%s", text)
	}
	// Providers, not the env section (which legitimately holds the variable
	// itself): exactly the one this launch wrote.
	if got := strings.Count(text, `"envKey": "`+qwenOllamaEnvKey+`"`); got != 1 {
		t.Errorf("settings.json holds %d provider entries using %s, want exactly the one this launch wrote:\n%s", got, qwenOllamaEnvKey, text)
	}
}

// Control: the user's own provider — same env key, daemon base URL, a name that
// is not ours — survives, which is what the merge rule's doc promises.
func TestQwenKeepsAUsersOwnProviderOnTheDaemon(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)

	configDir := filepath.Join(home, ".qwen")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"model":{"name":"llama3.2"},"modelProviders":{"openai":[{"id":"ollama","name":"Remote Ollama","baseUrl":"` + qwenBaseURL() + `","envKey":"OLLAMA_API_KEY"}]}}`
	if err := os.WriteFile(qwenSettingsPath(t, home), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&Qwen{}).Configure("llama3.2"); err != nil {
		t.Fatalf("configure llama3.2: %v", err)
	}
	if text := readQwenSettings(t, home); !strings.Contains(text, "Remote Ollama") {
		t.Errorf("the user's own provider was deleted by an unrelated launch; the merge rule requires the \" (Ollama)\" suffix before an entry counts as ours:\n%s", text)
	}
}

func TestQwenRefusesToGuessBetweenRemotesOnOneBaseURL(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"https://shared.example/v1","api_key":"k-1","tool_format":"tool_calls"},
		{"name":"box2","base_url":"https://shared.example/v1","api_key":"k-2","tool_format":"tool_calls"}]}`)

	if _, ok := qwenConfiguredRemoteForBase("https://shared.example/v1"); ok {
		t.Error("qwenConfiguredRemoteForBase answered for a base URL two configured remotes share: the answer is a guess between two accounts, and the caller writes the winner's name back")
	}

	q := &Qwen{}
	if err := q.Configure("box/big-model"); err != nil {
		t.Fatalf("configure box/big-model: %v", err)
	}
	if got := q.CurrentModel(); got == "box2/big-model" {
		t.Errorf("CurrentModel = %q: that is the OTHER account on the same host, a name the launch never wrote", got)
	}
}
