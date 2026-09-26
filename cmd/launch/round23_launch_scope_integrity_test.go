package launch

// round23_launch_scope_integrity_test.go — four ways the round-22 "a row's
// identity is its endpoint" change stopped one call site short (2026-09-27
// audit, round 23).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 1. launchAfterConfiguration's refusal saw the WHOLE menu.
//
// rejectServedModels refuses a row served by `oaica serve` on its own origin —
// right for the row being launched, wrong for a row the user never picked.
// claude is the one integration whose WantsFullModelChoices() is true, so
// launchSingleIntegration hands the backstop the whole stripped inventory: one
// running `oaica serve` anywhere in the menu refused `oaica launch claude
// --model llama3.2`, which names neither the serve row nor its origin. The
// managed-single writers were given the filter in round 22 (models.go); this
// hook was not.
// ---------------------------------------------------------------------------

func TestAnUnselectedServeRowDoesNotRefuseAnIntegrationLaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake integration is an sh stub on PATH")
	}
	setTestHome(t, t.TempDir())
	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)

	runner := &launcherManagedRunner{}
	withIntegrationOverride(t, "claude", runner)

	models := []LaunchModel{
		{Name: "llama3.2", LiveSource: liveSourceDaemon},
		{Name: "kat", LiveSource: liveSourceLocal},
	}
	if err := launchAfterConfiguration("claude", runner, "llama3.2", models, IntegrationLaunchRequest{}); err != nil {
		t.Errorf("launchAfterConfiguration = %v, want success: the row served by `oaica serve` was never selected, and refusing on it means one running serve instance refuses every ordinary launch of this integration", err)
	}
	if runner.ranModel != "llama3.2" {
		t.Errorf("ranModel = %q, want llama3.2", runner.ranModel)
	}
}

// TestASelectedServeRowIsStillRefusedByTheLaunchBackstop is the control: the
// refusal still fires for the row the launch is actually for.
func TestASelectedServeRowIsStillRefusedByTheLaunchBackstop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake integration is an sh stub on PATH")
	}
	setTestHome(t, t.TempDir())
	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)

	runner := &launcherManagedRunner{}
	withIntegrationOverride(t, "claude", runner)

	models := []LaunchModel{
		{Name: "llama3.2", LiveSource: liveSourceDaemon},
		{Name: "kat", LiveSource: liveSourceLocal},
	}
	err := launchAfterConfiguration("claude", runner, "kat", models, IntegrationLaunchRequest{})
	if err == nil {
		t.Fatal("a launch whose OWN model is served by `oaica serve` was allowed: the integration's config names the local daemon for every row it writes, and the daemon does not serve it")
	}
	if !strings.Contains(err.Error(), "kat") {
		t.Errorf("refusal = %v, want it to name the serve row kat", err)
	}
	if runner.ranModel != "" {
		t.Errorf("ranModel = %q: the refused launch must not reach the integration", runner.ranModel)
	}
}

// ---------------------------------------------------------------------------
// 2. qwen and hermes wrote the user's spelling, not the endpoint's id.
//
// Every sibling writer (copilot, poolside, kimi, codex) resolves the picker
// spelling through childModelIDFor; these two kept the raw `--model` string and
// only consulted the endpoint for the remote case. Readiness accepts an alias
// and the source prefixes, so
//
//	oaica model alias glm --target ollama/glm-5.3-flash:cloud
//	oaica launch qwen --model glm
//
// configured the daemon endpoint — hermes' base URL half WAS converted — and
// then asked it for "glm", a name it does not serve.
// ---------------------------------------------------------------------------

func TestQwenAndHermesWriteTheIDTheirEndpointServes(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box","tool_format":"tool_calls"}]}`)

	old := resolveModelAlias
	resolveModelAlias = func(name string) (string, bool) {
		switch name {
		case "glm":
			return "ollama/glm-5.3-flash:cloud", true
		case "kat":
			return "box/kat-awq", true
		}
		return "", false
	}
	t.Cleanup(func() { resolveModelAlias = old })

	for _, tc := range []struct{ in, want string }{
		{"glm", "glm-5.3-flash:cloud"},  // alias -> the daemon-side catalogue id
		{"ollama/llama3.2", "llama3.2"}, // documented source prefix
		{"kat", "kat-awq"},              // alias -> a remote: the remote's own id
		{"llama3.2", "llama3.2"},        // already an id: untouched
	} {
		if got := qwenModelIDFor(tc.in); got != tc.want {
			t.Errorf("qwenModelIDFor(%q) = %q, want %q — the id is written beside the daemon's base URL, so it must be the id the daemon serves", tc.in, got, tc.want)
		}
		if got := hermesModelIDFor(tc.in); got != tc.want {
			t.Errorf("hermesModelIDFor(%q) = %q, want %q — the id is written beside the daemon's base URL, so it must be the id the daemon serves", tc.in, got, tc.want)
		}
	}
}

// TestAnAliasAndTheIDItResolvesToAreOneSelection pins the drift rule that keeps
// a writer which stores the served id from looking like drift forever: the
// launcher saved the alias, the integration's config reads back the target.
func TestAnAliasAndTheIDItResolvesToAreOneSelection(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)

	old := resolveModelAlias
	resolveModelAlias = func(name string) (string, bool) {
		if name == "glm" {
			return "ollama/glm-5.3-flash:cloud", true
		}
		return "", false
	}
	t.Cleanup(func() { resolveModelAlias = old })

	if !modelNamesAreTheSame("glm", "glm-5.3-flash:cloud") {
		t.Error("an alias and the id it resolves to compared as different selections: the writer stores the id the endpoint serves and reads it back, so the launcher would treat the config as drifted and reconfigure on every run")
	}
	if modelNamesAreTheSame("glm", "qwen3:8b") {
		t.Error("an alias compared equal to an unrelated model: only the alias's own target may match")
	}
	if modelNamesAreTheSame("llama3.2", "qwen3:8b") {
		t.Error("two unrelated local ids compared equal")
	}
}

// ---------------------------------------------------------------------------
// 3. The ChatGPT app was configured with the picker spelling.
//
// `--model glm` (an alias) or `--model ollama/llama3.2` reached
// writeCodexAppConfig and the model catalog verbatim, so the root model and
// every catalogue slug named a model the daemon does not serve, and the app
// posts exactly those.
// ---------------------------------------------------------------------------

func TestTheChatGPTAppIsPointedAtTheIDItsEndpointServes(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)

	old := resolveModelAlias
	resolveModelAlias = func(name string) (string, bool) {
		if name == "glm" {
			return "ollama/glm-5.3-flash:cloud", true
		}
		return "", false
	}
	t.Cleanup(func() { resolveModelAlias = old })

	rows := []LaunchModel{
		{Name: "glm-5.3-flash", Upstream: "glm-5.3-flash:cloud"},
		{Name: "llama3.2"},
	}
	if err := (&CodexApp{}).ConfigureWithModels("glm", rows); err != nil {
		t.Fatalf("ConfigureWithModels: %v", err)
	}

	configPath, err := codexConfigPath()
	if err != nil {
		t.Fatalf("codexConfigPath: %v", err)
	}
	cfg, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if !strings.Contains(string(cfg), `model = "glm-5.3-flash:cloud"`) {
		t.Errorf("config.toml does not name the daemon-side id:\n%s", cfg)
	}

	catalogPath, err := codexAppModelCatalogPath()
	if err != nil {
		t.Fatalf("codexAppModelCatalogPath: %v", err)
	}
	raw, err := os.ReadFile(filepath.Clean(catalogPath))
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	var catalog struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("catalog not JSON: %v\n%s", err, raw)
	}
	slugs := make([]string, 0, len(catalog.Models))
	for _, m := range catalog.Models {
		slugs = append(slugs, m.Slug)
	}
	// The alias row is written under the id its endpoint serves; the second row
	// keeps its own id. Every slug must be a name the daemon answers to.
	want := map[string]bool{"glm-5.3-flash:cloud": true, "llama3.2": true}
	if len(slugs) != len(want) {
		t.Fatalf("catalogue slugs = %v, want exactly %v", slugs, want)
	}
	for _, slug := range slugs {
		if !want[slug] {
			t.Errorf("catalogue slug %q is not an id the daemon serves (want %v): the app posts the slug verbatim to the provider base URL oaica configured", slug, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. A tool-only turn was reported as zero output.
//
// The output estimate "counts what was relayed to the client" — its own stated
// rule, which is why round 17 added thinking to it. A tool_use block and its
// argument JSON are relayed and billed as output, but only Content and Thinking
// were counted, so a Claude Code tool call against an upstream that states no
// usage reported output_tokens: 0 for the turn that did the work.
// ---------------------------------------------------------------------------

func TestAToolOnlyTurnIsNotReportedAsZeroOutput(t *testing.T) {
	// Streaming: a tool call and no usage chunk at all.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		for _, frame := range []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\""}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"/tmp/x.txt\"}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, frame+"\n\n")
			f.Flush()
		}
	}))
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-r23-toolonly")
	resp, err := http.Post(proxy+"/v1/messages", "application/json", bytes.NewReader(calibMessagesBody(t, 400, 64, true)))
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)

	if !strings.Contains(body, `"tool_use"`) {
		t.Fatalf("premise: the tool call did not reach the client, so this test is not about the output estimate:\n%s", body)
	}
	if got := usageInt(deltaUsage(t, body), "output_tokens"); got <= 0 {
		t.Errorf("output_tokens = %d for a turn whose tool_use block and arguments were relayed: the estimate counts what the client received, and an upstream that states no usage leaves this as the only number available", got)
	}
}
