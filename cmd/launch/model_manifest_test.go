package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempOaicaHome points modelManifestPath at a throwaway HOME so tests
// never touch the real ~/.oaica/models.json.
func withTempOaicaHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".oaica"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestModelManifest_AddListShowRemove(t *testing.T) {
	withTempOaicaHome(t)

	e, err := ModelAdd(ModelAddOptions{
		ID: "kat-awq", Engine: "vllm", Arch: "Qwen3_5MoeForConditionalGeneration",
		Quant: "awq-w4a16", ContextWindow: 262144, DefaultMaxOutputTokens: 32000,
		GPUMemGB: 73, LaunchFlags: []string{"--kv-cache-dtype", "fp8"},
		Notes: "Mamba align mode needs max-num-batched-tokens >= 2096",
	})
	if err != nil {
		t.Fatalf("ModelAdd: %v", err)
	}
	if e.ID != "kat-awq" || e.ContextWindow != 262144 {
		t.Fatalf("unexpected entry: %+v", e)
	}

	got, err := ModelShow("kat-awq")
	if err != nil {
		t.Fatalf("ModelShow: %v", err)
	}
	if got.Quant != "awq-w4a16" {
		t.Fatalf("ModelShow quant = %q", got.Quant)
	}

	var buf bytes.Buffer
	if err := WriteModelList(&buf); err != nil {
		t.Fatalf("WriteModelList: %v", err)
	}
	if !strings.Contains(buf.String(), "kat-awq") {
		t.Fatalf("list missing entry: %s", buf.String())
	}

	existed, err := ModelRemove("kat-awq")
	if err != nil {
		t.Fatalf("ModelRemove: %v", err)
	}
	if !existed {
		t.Fatal("ModelRemove reported not-existed for an entry that was just added")
	}
	if _, err := ModelShow("kat-awq"); err == nil {
		t.Fatal("ModelShow succeeded after removal")
	}
}

func TestModelManifest_AddValidation(t *testing.T) {
	withTempOaicaHome(t)

	if _, err := ModelAdd(ModelAddOptions{ID: "", Engine: "vllm"}); err == nil {
		t.Fatal("expected error for empty id")
	}
	if _, err := ModelAdd(ModelAddOptions{ID: "x", Engine: "not-a-real-engine"}); err == nil {
		t.Fatal("expected error for unknown engine")
	}
	if _, err := ModelAdd(ModelAddOptions{ID: "x", Engine: "vllm", ContextWindow: -1}); err == nil {
		t.Fatal("expected error for negative context window")
	}
}

func TestModelManifest_ListEmpty(t *testing.T) {
	withTempOaicaHome(t)
	var buf bytes.Buffer
	if err := WriteModelList(&buf); err != nil {
		t.Fatalf("WriteModelList: %v", err)
	}
	if !strings.Contains(buf.String(), "No models") {
		t.Fatalf("expected empty-manifest message, got: %s", buf.String())
	}
}

func TestModelManifest_PersistsAcrossLoads(t *testing.T) {
	withTempOaicaHome(t)
	if _, err := ModelAdd(ModelAddOptions{ID: "a", Engine: "llama.cpp", ContextWindow: 8192}); err != nil {
		t.Fatal(err)
	}
	if _, err := ModelAdd(ModelAddOptions{ID: "b", Engine: "prism-engine", ContextWindow: 4096}); err != nil {
		t.Fatal(err)
	}
	m, err := loadModelManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Models) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m.Models))
	}
	ids := m.SortedIDs()
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("SortedIDs = %v", ids)
	}
}

func TestContextWindowFromManifest(t *testing.T) {
	withTempOaicaHome(t)
	if _, ok := contextWindowFromManifest("nope"); ok {
		t.Fatal("expected no hit for unknown model")
	}
	if _, err := ModelAdd(ModelAddOptions{ID: "kat-awq", Engine: "vllm", ContextWindow: 262144}); err != nil {
		t.Fatal(err)
	}
	v, ok := contextWindowFromManifest("kat-awq")
	if !ok || v != 262144 {
		t.Fatalf("contextWindowFromManifest = (%d, %v)", v, ok)
	}
}

func TestWithContextWindows_ManifestFallback(t *testing.T) {
	withTempOaicaHome(t)
	if _, err := ModelAdd(ModelAddOptions{ID: "kat-awq", Engine: "vllm", ContextWindow: 262144}); err != nil {
		t.Fatal(err)
	}
	old := remoteContextWindowFn
	defer func() { remoteContextWindowFn = old }()
	remoteContextWindowFn = func(route proxyRoute) int { return 0 } // simulate unreachable/unknown upstream

	plan := tierPlan{PrimaryName: "kat-awq", SecondaryName: "kat-awq",
		Routes: proxyRouteTable{Default: proxyRoute{BaseURL: "http://x/v1"}}}
	plan.withContextWindows()
	if plan.PrimaryContext != 262144 {
		t.Fatalf("PrimaryContext = %d, want manifest fallback 262144", plan.PrimaryContext)
	}
}

func TestWithContextWindows_LiveProbeWinsOverManifest(t *testing.T) {
	withTempOaicaHome(t)
	// Manifest says 262144 (the normal config); live probe reports a
	// downsized 32768 (e.g. emergency config during GPU crowding) — the
	// live number must win, matching the 2026-08-27 incident.
	if _, err := ModelAdd(ModelAddOptions{ID: "kat-awq", Engine: "vllm", ContextWindow: 262144}); err != nil {
		t.Fatal(err)
	}
	old := remoteContextWindowFn
	defer func() { remoteContextWindowFn = old }()
	remoteContextWindowFn = func(route proxyRoute) int { return 32768 }

	plan := tierPlan{PrimaryName: "kat-awq", SecondaryName: "kat-awq",
		Routes: proxyRouteTable{Default: proxyRoute{BaseURL: "http://x/v1"}}}
	plan.withContextWindows()
	if plan.PrimaryContext != 32768 {
		t.Fatalf("PrimaryContext = %d, want live probe 32768 to win over manifest", plan.PrimaryContext)
	}
}

func TestContextEnvVars_UsesManifestOutputReserve(t *testing.T) {
	withTempOaicaHome(t)
	if _, err := ModelAdd(ModelAddOptions{ID: "kat-awq", Engine: "vllm", ContextWindow: 262144, DefaultMaxOutputTokens: 8000}); err != nil {
		t.Fatal(err)
	}
	plan := tierPlan{PrimaryName: "kat-awq", SecondaryName: "kat-awq", PrimaryContext: 262144,
		Routes: proxyRouteTable{Default: proxyRoute{BaseURL: "http://x/v1"}}}
	env := plan.contextEnvVars()
	want := "CLAUDE_CODE_MAX_CONTEXT_TOKENS=254144" // 262144 - 8000, not the global 32000 reserve
	found := false
	for _, kv := range env {
		if kv == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("contextEnvVars = %v, want %q", env, want)
	}
}
