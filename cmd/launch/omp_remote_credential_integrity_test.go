package launch

// omp_remote_credential_integrity_test.go — a user-remote launch handed OMP a
// provider it could not authenticate, under an auth value the schema does not
// define (2026-09-26 audit, tenth round).
//
// `oaica launch omp box/big-model` pointed the ollama provider at the remote's
// base URL and set auth: bearer — and then supplied no credential anywhere. OMP
// reads a provider credential from providers.<name>.apiKey (its own documented
// order: --api-key, then models.yml's apiKey, then stored logins, then the
// provider's environment variable), and its documented auth values are apiKey
// (the default), none, and oauth. So the written config both named a scheme the
// schema rejects and left the request unauthenticated: the remote 401s, and if
// the user has OLLAMA_API_KEY exported for ollama.com, the ollama provider's
// own env fallback can send THAT key to the third-party host.
//
// The daemon is the genuinely keyless case — omp talks to 127.0.0.1 and auth:
// none is correct there — which is why the local shape must stay byte-for-byte
// what it was.
//
// The second half is the writer disagreeing with its own health check:
// ompProviderHealthy required the DAEMON's base URL, so CurrentModel() reported
// "" for the config the same file had just written. The launcher then treats
// OMP as unconfigured and re-prompts for a model the user already chose.

import (
	"os"
	"path/filepath"
	"testing"
)

// ompProviderFromFile reads the ollama provider the last Configure wrote.
func ompProviderFromFile(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".omp", "agent", "models.yml"))
	if err != nil {
		t.Fatalf("read models.yml: %v", err)
	}
	return ompProviderFromYAML(t, parseOMPConfigYAML(t, data))
}

const ompRemoteBody = `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box-SECRET-1234567890","tool_format":"tool_calls"}]}`

// A remote launch must give OMP the remote's credential, under a scheme its
// schema defines.
func TestAnOMPRemoteLaunchCarriesTheRemotesCredential(t *testing.T) {
	home := t.TempDir()
	setOMPTestHome(t, home)
	withTempRemotesFile(t)
	writeRemotes(t, ompRemoteBody)
	stubBareIndex(t, map[string][]string{})

	if err := (&OMP{}).ConfigureWithModels("box/big-model", testLaunchModels("box/big-model")); err != nil {
		t.Fatalf("configure: %v", err)
	}

	provider := ompProviderFromFile(t, home)
	if got, _ := provider["baseUrl"].(string); got != "https://box.example/v1" {
		t.Fatalf("baseUrl = %q, want the remote", got)
	}
	auth, _ := provider["auth"].(string)
	key, _ := provider["apiKey"].(string)
	switch auth {
	case "bearer":
		t.Errorf("auth = %q, which is not one of OMP's auth values (apiKey, none, oauth)", auth)
	case "apiKey":
		if key != "sk-box-SECRET-1234567890" {
			t.Errorf("auth = apiKey but apiKey = %q, want the remote's token — this is the only place OMP reads a provider credential from besides the environment", key)
		}
	default:
		t.Errorf("auth = %q with apiKey = %q: the remote needs a credential and nothing else supplies one (the child's environment is passed through untouched, and OLLAMA_API_KEY there belongs to ollama.com)", auth, key)
	}
	if api, _ := provider["api"].(string); api != "openai-completions" {
		t.Errorf("api = %q, want openai-completions — a user remote serves /v1/chat/completions, and openai-responses is what the DAEMON serves", api)
	}
	if d, _ := provider["discovery"].(map[string]any); d != nil {
		if dt, _ := d["type"].(string); dt == "ollama" {
			t.Errorf("discovery = ollama on a non-Ollama host: OMP probes Ollama's native /api/tags endpoints, which the remote does not serve")
		}
	}
}

// A remote with no credential is the genuinely unauthenticated case.
func TestAnOMPKeylessRemoteIsDeclaredUnauthenticated(t *testing.T) {
	home := t.TempDir()
	setOMPTestHome(t, home)
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"lab","base_url":"http://127.0.0.1:9090/v1","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})

	if err := (&OMP{}).ConfigureWithModels("lab/small-model", testLaunchModels("lab/small-model")); err != nil {
		t.Fatalf("configure: %v", err)
	}

	provider := ompProviderFromFile(t, home)
	if auth, _ := provider["auth"].(string); auth != "none" {
		t.Errorf("auth = %q, want none for a remote with no credential — anything else sends an empty or borrowed key", auth)
	}
	if key, ok := provider["apiKey"]; ok && key != "" {
		t.Errorf("apiKey = %v on a keyless remote", key)
	}
}

// The daemon config is restored in full when the next launch is local: no
// remote key left behind, discovery back on Ollama's own endpoints.
func TestAnOMPLocalLaunchRestoresTheDaemonShape(t *testing.T) {
	home := t.TempDir()
	setOMPTestHome(t, home)
	withTempRemotesFile(t)
	writeRemotes(t, ompRemoteBody)
	stubBareIndex(t, map[string][]string{})

	if err := (&OMP{}).ConfigureWithModels("box/big-model", testLaunchModels("box/big-model")); err != nil {
		t.Fatalf("configure remote: %v", err)
	}
	if err := (&OMP{}).ConfigureWithModels("gemma4", testLaunchModels("gemma4")); err != nil {
		t.Fatalf("configure local: %v", err)
	}

	provider := ompProviderFromFile(t, home)
	if got, _ := provider["baseUrl"].(string); got != ompBaseURL() {
		t.Errorf("baseUrl = %q, want the daemon's %q", got, ompBaseURL())
	}
	if api, _ := provider["api"].(string); api != "openai-responses" {
		t.Errorf("api = %q, want openai-responses", api)
	}
	if auth, _ := provider["auth"].(string); auth != "none" {
		t.Errorf("auth = %q, want none", auth)
	}
	if key, ok := provider["apiKey"]; ok && key != "" {
		t.Errorf("apiKey = %v left on the daemon provider — the daemon ignores it, so it is a stale credential nobody will remove", key)
	}
	d, _ := provider["discovery"].(map[string]any)
	if d == nil || d["type"] != "ollama" {
		t.Errorf("discovery = %v, want ollama", provider["discovery"])
	}
}

// CurrentModel must report the model the same file just wrote.
func TestOMPReportsTheRemoteModelItJustWrote(t *testing.T) {
	home := t.TempDir()
	setOMPTestHome(t, home)
	withTempRemotesFile(t)
	writeRemotes(t, ompRemoteBody)
	stubBareIndex(t, map[string][]string{})

	o := &OMP{}
	if err := o.ConfigureWithModels("box/big-model", testLaunchModels("box/big-model")); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if got := o.CurrentModel(); got != "box/big-model" {
		t.Errorf("CurrentModel() = %q, want %q — the config was written by this call, and the launcher reads this id to decide whether OMP is configured (an empty answer re-prompts for a model the user already chose)", got, "box/big-model")
	}

	// Control: the daemon path is unchanged.
	if err := o.ConfigureWithModels("gemma4", testLaunchModels("gemma4")); err != nil {
		t.Fatalf("configure local: %v", err)
	}
	if got := o.CurrentModel(); got != "gemma4" {
		t.Errorf("CurrentModel() = %q, want gemma4 after a local launch", got)
	}
}

// A config pointing at a host that is neither the daemon nor a configured
// remote still reads as "not ours", so this cannot be passed by accepting
// every base URL.
func TestOMPStaleConfigStillReportsNoModel(t *testing.T) {
	home := t.TempDir()
	setOMPTestHome(t, home)
	withTempRemotesFile(t)
	writeRemotes(t, ompRemoteBody)

	path := filepath.Join(home, ".omp", "agent", "models.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "providers:\n  ollama:\n" +
		"    baseUrl: https://someone-elses-host.example/v1\n" +
		"    api: openai-completions\n" +
		"    auth: apiKey\n" +
		"    apiKey: sk-not-ours\n" +
		"    models:\n" +
		"      - id: not-ours\n"
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := (&OMP{}).CurrentModel(); got == "not-ours" {
		t.Errorf("CurrentModel() = %q for a provider pointing at a host no configured remote serves", got)
	}
}
