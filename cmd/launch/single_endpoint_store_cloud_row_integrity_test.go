package launch

// single_endpoint_store_cloud_row_integrity_test.go — an ollama-cloud model,
// which the LOCAL DAEMON serves and proxies, was refused as if it were a
// user remote (2026-09-27 audit, round 20).
//
// Three writers refuse a selection their store cannot express: the DeepSeek
// Harness, the ChatGPT app and muse each name ONE endpoint for every row — the
// local daemon — and no credential field oaica can write. The predicate they
// used was LaunchModel.Remote, which is not "not on this endpoint": the picker
// marks an ollama-cloud row ("glm-5.1:cloud") Remote too, and that row is
// served BY the daemon, which proxies it to ollama.com over the same base URL
// and the same (absent) credential as any local model. So `oaica launch dsh
// --model glm-5.1:cloud` was refused with a message telling the user to
// "launch a daemon-backed model" — the thing they had just done.
//
// singleEndpointModels, the filter these three writers already share, keys on
// the endpoint (launchModelEndpointKey), and it is what round 19's own control
// proves: a cloud row is on the daemon's endpoint and survives the filter. The
// refusals have to agree with it, or the writer refuses a row it would have
// written correctly.
//
// The rule: a single-endpoint store refuses a row routed to an endpoint other
// than the daemon; a daemon-routed row, cloud or not, is writable.

import (
	"os"
	"strings"
	"testing"
)

// cloudRowMenu is the picker menu shape these writers see: a daemon model, an
// ollama-cloud row the daemon proxies, and a genuine user remote.
func cloudRowMenu(t *testing.T) []LaunchModel {
	t.Helper()
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box-SECRET","tool_format":"tool_calls"}]}`)
	if _, ok := resolveRemoteEndpoint("box/big-model"); !ok {
		t.Fatal("premise: the box/big-model row does not resolve, so the remote case below proves nothing")
	}

	menu := launchModelsFromNames([]string{"llama3.2"})
	menu = append(menu,
		LaunchModel{Name: "glm-5.1:cloud", Remote: true, Upstream: "glm-5.1:cloud"}.WithCloudLimits(),
		LaunchModel{Name: "box/big-model", Remote: true, Upstream: "big-model"},
	)
	if got := launchModelEndpointKey(menu[1]); got != "daemon" {
		t.Fatalf("premise: the cloud row is routed to %q, not the local daemon, so it is not the false-refusal this test is about", got)
	}
	if got := launchModelEndpointKey(menu[2]); got == "daemon" {
		t.Fatalf("premise: the user-remote row is routed to the daemon, so nothing is refused at all")
	}
	return menu
}

func TestChatGPTAcceptsADaemonProxiedCloudModel(t *testing.T) {
	menu := cloudRowMenu(t)

	if err := (&CodexApp{}).ConfigureWithModels("glm-5.1:cloud", menu); err != nil {
		t.Fatalf("the ChatGPT app refused an ollama-cloud model: the daemon serves and proxies it over the same endpoint and credential as the local models, so there is nothing unrepresentable about the selection: %v", err)
	}

	catalogPath, err := codexAppModelCatalogPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	if !strings.Contains(string(data), "glm-5.1:cloud") {
		t.Errorf("the cloud model the user selected is not in the catalogue:\n%s", data)
	}
	if strings.Contains(string(data), "box/big-model") {
		t.Errorf("the catalogue advertises a user remote its single daemon endpoint cannot serve:\n%s", data)
	}
}

func TestDeepSeekHarnessAcceptsADaemonProxiedCloudModel(t *testing.T) {
	menu := cloudRowMenu(t)

	if err := (&DeepSeekHarness{}).ConfigureWithModels("glm-5.1:cloud", menu); err != nil {
		t.Fatalf("the DeepSeek Harness refused an ollama-cloud model: the settings name the local daemon, which is exactly what serves and proxies that row: %v", err)
	}

	settingsPath, err := deepSeekHarnessSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if !strings.Contains(string(data), "glm-5.1:cloud") {
		t.Errorf("the cloud model is missing from the harness settings:\n%s", data)
	}
	if strings.Contains(string(data), "box/big-model") {
		t.Errorf("the harness settings advertise a user remote its single daemon endpoint cannot serve:\n%s", data)
	}
}

func TestMuseAcceptsADaemonProxiedCloudModel(t *testing.T) {
	menu := cloudRowMenu(t)

	if err := (&Muse{}).Edit(menu[:2]); err != nil {
		t.Fatalf("muse refused an ollama-cloud model: its settings name the local daemon, which is what serves and proxies that row: %v", err)
	}

	settingsPath, err := museSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if !strings.Contains(string(data), "glm-5.1:cloud") {
		t.Errorf("the cloud model is missing from muse's settings:\n%s", data)
	}
}

// Control: the refusal these writers exist for is untouched — a row on an
// endpoint other than the daemon is still refused, by name, before the write.
func TestMuseStillRefusesAUserRemoteModel(t *testing.T) {
	menu := cloudRowMenu(t)

	err := (&Muse{}).Edit(menu)
	if err == nil {
		t.Fatal("muse accepted a user-remote model: its settings carry one global endpoint for every row and no credential field, so the namespaced picker name is posted to the local daemon, which does not resolve it")
	}
	if !strings.Contains(err.Error(), "box/big-model") {
		t.Errorf("the refusal does not name the model that cannot be used: %v", err)
	}
}
