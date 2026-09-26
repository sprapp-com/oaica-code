package launch

// child_model_id_integrity_test.go — a `--model` picker spelling reached the
// child CLI unresolved (2026-09-27 audit, round 22).
//
// copilotModelIDFor / poolsideModelIDFor / kimiModelIDFor resolved only the
// user-remote case and returned everything else verbatim, while the URL and key
// siblings point the child at the daemon. Readiness accepts an alias and the
// source prefixes resolveLaunchEndpoint strips for its own lookup, so
//
//	oaica model alias glm --target ollama/glm-5.3-flash:cloud
//	oaica launch copilot --model glm
//
// configured the daemon endpoint and then asked it for "glm": exit 0, first
// inference 404, and the bogus name was saved for every later launch. The
// remote case additionally lost the endpoint when the target was an alias.

import "testing"

func TestAChildGetsTheDaemonSideIDForAPickerSpelling(t *testing.T) {
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
		{"glm", "glm-5.3-flash:cloud"},           // alias -> picker prefix -> daemon-side id
		{"kat", "kat-awq"},                       // alias -> a remote: the remote's own id
		{"ollama/llama3.2", "llama3.2"},          // documented source prefix
		{"daemon/qwen3:8b", "qwen3:8b"},          // documented source prefix
		{"box/big-model", "big-model"},           // a configured remote keeps its upstream id
		{"llama3.2", "llama3.2"},                 // already an id: untouched
		{"library/llama3.2", "library/llama3.2"}, // not one of our prefixes
	} {
		if got := childModelIDFor(tc.in); got != tc.want {
			t.Errorf("childModelIDFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := copilotModelIDFor(tc.in); got != tc.want {
			t.Errorf("copilotModelIDFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := poolsideModelIDFor(tc.in); got != tc.want {
			t.Errorf("poolsideModelIDFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := kimiModelIDFor(tc.in); got != tc.want {
			t.Errorf("kimiModelIDFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := codexModelIDFor(tc.in); got != tc.want {
			t.Errorf("codexModelIDFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
