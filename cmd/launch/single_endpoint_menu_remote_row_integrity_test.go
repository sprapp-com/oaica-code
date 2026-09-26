package launch

// single_endpoint_menu_remote_row_integrity_test.go — a remote row sitting in
// the PICKER MENU must not refuse a daemon-model launch (2026-09-27 audit,
// round 19; the guard that did this landed in round 18).
//
// The single-model integrations that also write a model list (chatgpt, dsh) are
// handed `managedSingleConfigureModels`: the launch target plus EVERY row the
// picker offers, resolved back to LaunchModels with their Remote flags. The
// refusal these two gained read that list as if it were the user's selection,
// so one router/user-remote/cloud catalogue row — a row the user never picked —
// refused the launch of a perfectly ordinary local model:
//
//	oaica launch dsh --model llama3.2 --config
//	Error: ... cannot be pointed at the remote model "deepseek/deepseek-flash"
//
// What each store actually needs is narrower: the row that decides the endpoint
// is the primary, and only rows the single configured endpoint can serve belong
// in the list it advertises. So the primary is still refused when it is a
// remote, and the rows that are not on that endpoint are left out of the write
// instead of blocking it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func menuWithARemoteRow(t *testing.T) []LaunchModel {
	t.Helper()
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box-SECRET","tool_format":"tool_calls"},
		{"name":"zai","base_url":"https://zai.example/v1","api_key":"sk-zai-SECRET","tool_format":"tool_calls"}]}`)
	for _, name := range []string{"box/big-model", "zai/glm-4.6"} {
		if _, ok := resolveRemoteEndpoint(name); !ok {
			t.Fatalf("premise: the %s row does not resolve, so it would never be treated as a remote at all", name)
		}
	}

	menu := launchModelsFromNames([]string{"llama3.2"})
	menu = append(menu,
		LaunchModel{Name: "box/big-model", Remote: true, Upstream: "big-model"},
		LaunchModel{Name: "zai/glm-4.6", Remote: true, Upstream: "glm-4.6"},
		// The control: an ollama-cloud row is proxied BY the daemon, so it is
		// on this endpoint and must not be filtered out with the remotes.
		LaunchModel{Name: "glm-5.1:cloud", Remote: true, Upstream: "glm-5.1:cloud"},
	)
	return menu
}

func TestChatGPTLaunchesADaemonModelWhenTheMenuCarriesRemoteRows(t *testing.T) {
	menu := menuWithARemoteRow(t)

	if err := (&CodexApp{}).ConfigureWithModels("llama3.2", menu); err != nil {
		t.Fatalf("a daemon-model launch was refused because of a remote row in the picker menu: %v", err)
	}

	catalogPath, err := codexAppModelCatalogPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	if strings.Contains(string(data), "box/big-model") || strings.Contains(string(data), "zai/glm-4.6") {
		t.Errorf("the ChatGPT catalogue advertises rows its single daemon endpoint cannot serve:\n%s", data)
	}
	if !strings.Contains(string(data), "llama3.2") {
		t.Errorf("the selected daemon model is missing from the catalogue:\n%s", data)
	}
	if !strings.Contains(string(data), "glm-5.1:cloud") {
		t.Errorf("an ollama-cloud row was dropped even though the daemon serves it:\n%s", data)
	}
}

func TestDeepSeekHarnessLaunchesADaemonModelWhenTheMenuCarriesRemoteRows(t *testing.T) {
	menu := menuWithARemoteRow(t)

	if err := (&DeepSeekHarness{}).ConfigureWithModels("llama3.2", menu); err != nil {
		t.Fatalf("a daemon-model launch was refused because of a remote row in the picker menu: %v", err)
	}

	settingsPath, err := deepSeekHarnessSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if strings.Contains(string(data), "box/big-model") || strings.Contains(string(data), "zai/glm-4.6") {
		t.Errorf("the harness settings advertise models its single daemon endpoint cannot serve:\n%s", data)
	}
	if !strings.Contains(string(data), "llama3.2") {
		t.Errorf("the selected daemon model is missing from the settings:\n%s", data)
	}
	if !strings.Contains(string(data), "glm-5.1:cloud") {
		t.Errorf("an ollama-cloud row was dropped even though the daemon serves it:\n%s", data)
	}

	patchPath, perr := deepSeekHarnessPatchPath()
	if perr != nil {
		t.Fatal(perr)
	}
	if _, serr := os.Stat(filepath.Join(filepath.Dir(patchPath), filepath.Base(patchPath))); serr != nil {
		t.Errorf("the launch did not write its patch file: %v", serr)
	}
}
