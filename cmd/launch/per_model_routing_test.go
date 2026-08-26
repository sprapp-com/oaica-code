package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDescriptorRemotesFile writes a remotes.json exercising the protocol
// descriptor (wire/tool_format/tool_reliable) that the capability gate keys on.
func writeDescriptorRemotesFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "remotes.json")
	content := `{
  "remotes": [
    { "name": "kat", "base_url": "http://192.168.0.50:8080", "api_key_env": "KAT_KEY", "tool_format": "freeform" },
    { "name": "vl",  "base_url": "http://10.0.0.9:8000",      "api_key": "sk-vl", "tool_format": "tool_calls" },
    { "name": "ds",  "base_url": "https://api.deepseek.com",  "api_key": "sk-ds" },
    { "name": "zai", "base_url": "https://api.z.ai/api/paas", "api_key": "sk-zai", "version": "v4" },
    { "name": "katforced", "base_url": "http://192.168.0.50:30099", "api_key": "", "tool_format": "freeform", "force_tools": true }
  ]
}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveRemoteEndpointDescriptor(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeDescriptorRemotesFile(t))
	t.Setenv("KAT_KEY", "sk-kat")

	// freeform remote: wire openai, tool_format freeform, unreliable.
	ep, ok := resolveRemoteEndpoint("kat/kat-coder")
	if !ok {
		t.Fatal("resolveRemoteEndpoint(kat/kat-coder) not ok")
	}
	if ep.Name != "kat" {
		t.Errorf("Name = %q, want kat", ep.Name)
	}
	if ep.BaseURL != "http://192.168.0.50:8080/v1" {
		t.Errorf("BaseURL = %q, want /v1 appended", ep.BaseURL)
	}
	if ep.Token != "sk-kat" {
		t.Errorf("Token = %q, want env-provided key", ep.Token)
	}
	if ep.UpstreamModel != "kat-coder" {
		t.Errorf("UpstreamModel = %q, want bare id", ep.UpstreamModel)
	}
	if ep.Wire != "openai" {
		t.Errorf("Wire = %q, want default openai", ep.Wire)
	}
	if ep.ToolFormat != "freeform" {
		t.Errorf("ToolFormat = %q, want freeform", ep.ToolFormat)
	}
	if ep.ToolReliable {
		t.Error("ToolReliable = true, want false for freeform")
	}
}

func TestResolveRemoteEndpointInferToolCalls(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeDescriptorRemotesFile(t))

	// explicit tool_calls → reliable.
	ep, ok := resolveRemoteEndpoint("vl/qwen3")
	if !ok {
		t.Fatal("resolveRemoteEndpoint(vl/qwen3) not ok")
	}
	if ep.ToolFormat != "tool_calls" || !ep.ToolReliable {
		t.Errorf("vl: ToolFormat=%q ToolReliable=%v, want tool_calls/reliable", ep.ToolFormat, ep.ToolReliable)
	}

	// no descriptor → inferred tool_calls → reliable (default caution keeps
	// vLLM/llama-server/SGLang off the gate).
	ep, ok = resolveRemoteEndpoint("ds/deepseek-chat")
	if !ok {
		t.Fatal("resolveRemoteEndpoint(ds/deepseek-chat) not ok")
	}
	if ep.ToolFormat != "tool_calls" || !ep.ToolReliable {
		t.Errorf("ds: ToolFormat=%q ToolReliable=%v, want inferred tool_calls/reliable", ep.ToolFormat, ep.ToolReliable)
	}

	// z.ai keeps its /v4 version prefix.
	ep, ok = resolveRemoteEndpoint("zai/glm-5.3")
	if !ok {
		t.Fatal("resolveRemoteEndpoint(zai/glm-5.3) not ok")
	}
	if ep.BaseURL != "https://api.z.ai/api/paas/v4" {
		t.Errorf("zai BaseURL = %q, want /v4 (version override preserved)", ep.BaseURL)
	}
}

func TestResolveRemoteEndpointLocalAndCloud(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeDescriptorRemotesFile(t))
	stubBareIndex(t, map[string][]string{}) // bare names sweep the (unroutable) remotes otherwise

	for _, name := range []string{"llama3.2", "qwen3:32b:local"} {
		if _, ok := resolveRemoteEndpoint(name); ok {
			t.Errorf("resolveRemoteEndpoint(%q) ok=true, want false (not a remote)", name)
		}
	}
}

func TestOpenAIBaseURLAndKey(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeDescriptorRemotesFile(t))
	t.Setenv("KAT_KEY", "sk-kat")
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")
	stubBareIndex(t, map[string][]string{}) // "llama3.2" sweeps the (unroutable) remotes otherwise

	t.Run("remote primary uses remote triple", func(t *testing.T) {
		base, key, id := openAIBaseURLAndKey(LaunchModel{Name: "kat/kat-coder"})
		if base != "http://192.168.0.50:8080/v1" {
			t.Errorf("base = %q, want remote base", base)
		}
		if key != "sk-kat" {
			t.Errorf("key = %q, want remote token", key)
		}
		if id != "kat-coder" {
			t.Errorf("id = %q, want bare upstream id", id)
		}
	})

	t.Run("local primary keeps daemon triple", func(t *testing.T) {
		base, key, id := openAIBaseURLAndKey(LaunchModel{Name: "llama3.2"})
		if base != "http://127.0.0.1:11434/v1" {
			t.Errorf("base = %q, want daemon /v1", base)
		}
		if key != "ollama" {
			t.Errorf("key = %q, want ollama", key)
		}
		if id != "llama3.2" {
			t.Errorf("id = %q, want picker name", id)
		}
	})
}

func TestToolGateDecision(t *testing.T) {
	reliable := RemoteEndpoint{Name: "vl", ToolFormat: "tool_calls", ToolReliable: true}
	freeform := RemoteEndpoint{Name: "kat", ToolFormat: "freeform", ToolReliable: false}
	anthropicNative := RemoteEndpoint{Name: "cl", Wire: "anthropic", ToolFormat: "xml", ToolReliable: false}

	t.Run("reliable openai passes both wires", func(t *testing.T) {
		if ok, _ := toolGateDecision(toolWireOpenAI, reliable); !ok {
			t.Error("reliable tool_calls refused on OpenAI wire")
		}
		if ok, _ := toolGateDecision(toolWireAnthropic, reliable); !ok {
			t.Error("reliable tool_calls refused on Anthropic wire")
		}
	})

	t.Run("freeform refused with actionable reason", func(t *testing.T) {
		ok, reason := toolGateDecision(toolWireOpenAI, freeform)
		if ok {
			t.Error("freeform passed the OpenAI gate, want refuse")
		}
		for _, want := range []string{"kat", "freeform", "tool_reliable=false", "--force-tools", "tool_calls"} {
			if !strings.Contains(reason, want) {
				t.Errorf("reason %q missing %q", reason, want)
			}
		}
	})

	t.Run("anthropic-native remote passes anthropic wire", func(t *testing.T) {
		if ok, _ := toolGateDecision(toolWireAnthropic, anthropicNative); !ok {
			t.Error("anthropic-native remote refused on anthropic wire (Phase 3 native)")
		}
	})
}

func TestGateOpenAITools(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeDescriptorRemotesFile(t))
	t.Setenv("KAT_KEY", "sk-kat")
	stubBareIndex(t, map[string][]string{}) // "llama3.2" sweeps the (unroutable) remotes otherwise

	t.Run("freeform refuses unless forced", func(t *testing.T) {
		if err := gateOpenAITools("kat/kat-coder", false); err == nil {
			t.Error("gateOpenAITools(freeform, force=false) nil error, want refuse")
		}
		if err := gateOpenAITools("kat/kat-coder", true); err != nil {
			t.Errorf("gateOpenAITools(freeform, force=true) error: %v, want warn-and-proceed", err)
		}
	})

	t.Run("tool_calls remote and local pass", func(t *testing.T) {
		if err := gateOpenAITools("vl/qwen3", false); err != nil {
			t.Errorf("gateOpenAITools(tool_calls) error: %v", err)
		}
		if err := gateOpenAITools("llama3.2", false); err != nil {
			t.Errorf("gateOpenAITools(local) error: %v", err)
		}
	})

	t.Run("remote-level force_tools warns-and-proceeds without the flag", func(t *testing.T) {
		if err := gateOpenAITools("katforced/kat-coder", false); err != nil {
			t.Errorf("gateOpenAITools(force_tools remote, force=false) error: %v, want warn-and-proceed via remote's own force_tools", err)
		}
	})
}

func TestGateUserRemoteTools(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeDescriptorRemotesFile(t))
	t.Setenv("KAT_KEY", "sk-kat")

	kat, bare, ok := findUserRemoteForModel("kat/kat-coder")
	if !ok {
		t.Fatal("findUserRemoteForModel(kat/kat-coder) not ok")
	}
	if err := gateUserRemoteTools(kat, bare, toolWireAnthropic, false); err == nil {
		t.Error("gateUserRemoteTools(freeform, anthropic, force=false) nil, want refuse")
	}
	if err := gateUserRemoteTools(kat, bare, toolWireAnthropic, true); err != nil {
		t.Errorf("gateUserRemoteTools(force=true) error: %v, want warn-and-proceed", err)
	}

	katforced, bare2, ok := findUserRemoteForModel("katforced/kat-coder")
	if !ok {
		t.Fatal("findUserRemoteForModel(katforced/kat-coder) not ok")
	}
	if err := gateUserRemoteTools(katforced, bare2, toolWireAnthropic, false); err != nil {
		t.Errorf("gateUserRemoteTools(remote.ForceTools, force=false) error: %v, want warn-and-proceed via remote's own force_tools", err)
	}
}

func TestExtractForceTools(t *testing.T) {
	force, rest := extractForceTools([]string{"--model", "x", "--force-tools", "extra"})
	if !force {
		t.Error("force = false, want true")
	}
	if len(rest) != 3 || rest[0] != "--model" || rest[1] != "x" || rest[2] != "extra" {
		t.Errorf("rest = %v, want --model/x/extra with --force-tools removed", rest)
	}

	force, rest = extractForceTools([]string{"--force-tools=true", "a"})
	if !force {
		t.Error("force = false for --force-tools=true, want true")
	}
	if len(rest) != 1 || rest[0] != "a" {
		t.Errorf("rest = %v, want [a]", rest)
	}
}

func TestExtractSonnetModel(t *testing.T) {
	sonnet, rest := extractSonnetModel([]string{"--model", "x", "--sonnet-model", "muse-spark-1.2", "extra"})
	if sonnet != "muse-spark-1.2" {
		t.Errorf("sonnet = %q, want muse-spark-1.2", sonnet)
	}
	if len(rest) != 3 || rest[0] != "--model" || rest[1] != "x" || rest[2] != "extra" {
		t.Errorf("rest = %v, want --model/x/extra with --sonnet-model pair removed", rest)
	}

	sonnet, rest = extractSonnetModel([]string{"--sonnet-model=muse-spark-1.2", "a"})
	if sonnet != "muse-spark-1.2" {
		t.Errorf("sonnet = %q for --sonnet-model=..., want muse-spark-1.2", sonnet)
	}
	if len(rest) != 1 || rest[0] != "a" {
		t.Errorf("rest = %v, want [a]", rest)
	}

	sonnet, rest = extractSonnetModel([]string{"--model", "x"})
	if sonnet != "" {
		t.Errorf("sonnet = %q, want empty when flag absent", sonnet)
	}
	if len(rest) != 2 {
		t.Errorf("rest = %v, want unchanged when flag absent", rest)
	}
}
