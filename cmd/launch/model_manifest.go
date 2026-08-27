package launch

// model_manifest.go — a declared, persisted record of what a self-hosted
// model actually is: architecture, quantization, real context window,
// engine, and the launch flags that made it work. Complements, not
// replaces, the two things oaica already has:
//
//   - ~/.oaica/local_servers.json (oaica_models.go) is RUNTIME state: is a
//     server up right now, on what port, with what API key. Ephemeral,
//     rewritten every `oaica serve` start/stop.
//   - the live /models probe (context_window_remote.go) is BEST-EFFORT
//     discovery: ask the upstream what it thinks its window is, right now,
//     over the network, with a 2s timeout. Works great for vLLM/routers
//     that answer honestly; tells you nothing before the model is even
//     running, and nothing for engines that don't expose context_length.
//
// Neither answers "what models do I have configured, and what did I
// learn about each one the hard way" without a running server or repeat
// trial-and-error (see this session's history: --limit-mm-per-prompt
// needing JSON, Mamba align-mode block-size minimums, gpu-memory-
// utilization sized to fit kv-cache blocks — all discovered by crashing
// and reading logs). This file is where that goes, once.
//
// A manifest entry is not authoritative over a live probe: withContextWindows
// in context_window_remote.go still asks the running server first when it
// can reach one — that number reflects reality (including a downsized
// emergency config) more precisely than a static file might after a hand
// edit. The manifest is what fills the gap when there's nothing running to
// ask yet (`oaica model list`, `oaica model show`, and picking launch flags
// on cold start) and doubles as the field notes so a launch flag combination
// that took an hour to find is never rediscovered from scratch.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ModelEngine is which runtime a manifest entry launches through.
type ModelEngine string

const (
	EngineVLLM       ModelEngine = "vllm"
	EngineLlamaCPP   ModelEngine = "llama.cpp"
	EnginePrism      ModelEngine = "prism-engine"
	EngineDaemon     ModelEngine = "ollama-daemon"
	EngineUserRemote ModelEngine = "user-remote"
)

// ModelManifestEntry is one model's declared configuration. Field names and
// JSON tags are deliberately close to an Ollama Modelfile / vLLM CLI's own
// vocabulary so nothing here needs translating in your head.
type ModelManifestEntry struct {
	// ID is how launch/run/serve refer to this model: the value that goes
	// in --model, ANTHROPIC_DEFAULT_*_MODEL, and proxyRouteTable.ByModel
	// keys. Must be unique within the manifest.
	ID string `json:"id"`

	// Engine picks which launcher/probe path consumes this entry.
	Engine ModelEngine `json:"engine"`

	// Arch is a free-text architecture label (e.g. "Qwen3_5MoeForConditionalGeneration",
	// "llama", "prism-ternary-bitnet") — informational, shown in `model show`,
	// not machine-parsed.
	Arch string `json:"arch,omitempty"`

	// Quant is the quantization scheme (e.g. "awq-w4a16", "gguf-q4_k_m",
	// "ternary-1.58bit", "" for full precision).
	Quant string `json:"quant,omitempty"`

	// ContextWindow is the model's real max context in tokens, as
	// determined by the launch flags below (e.g. vLLM's --max-model-len).
	// 0 means unknown — fall back to a live probe or Claude Code's default.
	ContextWindow int `json:"context_window,omitempty"`

	// DefaultMaxOutputTokens is what this deployment should reserve for
	// output when computing a safe input budget (see
	// maxOutputTokensReserve in context_window_remote.go, which currently
	// assumes Claude Code's fixed 32000 for every backend). 0 defers to
	// that global default.
	DefaultMaxOutputTokens int `json:"default_max_output_tokens,omitempty"`

	// GPUMemGB / RAMGB are the approximate footprint this configuration
	// needs, for `model list` sizing display and for a future scheduler
	// that picks a GPU with enough headroom. Advisory only — not enforced.
	GPUMemGB float64 `json:"gpu_mem_gb,omitempty"`
	RAMGB    float64 `json:"ram_gb,omitempty"`

	// LaunchFlags are extra engine-specific CLI flags appended verbatim
	// after the base command for engines that shell out (vllm, llama.cpp).
	// Not used by daemon/user-remote entries.
	LaunchFlags []string `json:"launch_flags,omitempty"`

	// ModelPath is where the weights live locally (a directory for vLLM/
	// HF-format, a single file for llama.cpp GGUF / prism-engine .pqm).
	// Empty for daemon/user-remote entries, which resolve through their
	// own existing mechanisms.
	ModelPath string `json:"model_path,omitempty"`

	// Notes is free text for anything future-you needs to know before
	// touching this model's launch flags again — the "why" that a bare
	// flag list can't carry (e.g. "Mamba align mode needs
	// max-num-batched-tokens >= 2096; gpu-mem 0.9 required to fit
	// max-num-seqs Mamba cache blocks").
	Notes string `json:"notes,omitempty"`
}

// modelManifest is the on-disk container: a map keyed by ID for O(1)
// lookup plus a stable slice for deterministic listing.
type modelManifest struct {
	Version int                            `json:"version"`
	Models  map[string]ModelManifestEntry `json:"models"`
}

const modelManifestVersion = 1

func modelManifestPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "models.json"), nil
}

// loadModelManifest reads ~/.oaica/models.json, returning an empty (not
// nil) manifest if the file doesn't exist yet — first use should not
// require `oaica model add` to pre-create anything.
func loadModelManifest() (*modelManifest, error) {
	path, err := modelManifestPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &modelManifest{Version: modelManifestVersion, Models: map[string]ModelManifestEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m modelManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m.Models == nil {
		m.Models = map[string]ModelManifestEntry{}
	}
	return &m, nil
}

// save writes the manifest atomically (temp file + rename) so a crash
// mid-write can't corrupt an existing manifest.
func (m *modelManifest) save() error {
	path, err := modelManifestPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if m.Version == 0 {
		m.Version = modelManifestVersion
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Get returns the entry for id and whether it was found.
func (m *modelManifest) Get(id string) (ModelManifestEntry, bool) {
	e, ok := m.Models[id]
	return e, ok
}

// Put inserts or replaces an entry by its ID.
func (m *modelManifest) Put(e ModelManifestEntry) {
	if m.Models == nil {
		m.Models = map[string]ModelManifestEntry{}
	}
	m.Models[e.ID] = e
}

// Remove deletes an entry by ID, reporting whether it existed.
func (m *modelManifest) Remove(id string) bool {
	if _, ok := m.Models[id]; !ok {
		return false
	}
	delete(m.Models, id)
	return true
}

// SortedIDs returns model IDs in a stable, alphabetical order for listing.
func (m *modelManifest) SortedIDs() []string {
	ids := make([]string, 0, len(m.Models))
	for id := range m.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// contextWindowFromManifest is the manifest-first companion to the live
// /models probe: returns (window, true) if id is declared with a known
// ContextWindow, else (0, false) so callers fall through to
// remoteContextWindowFn unchanged. Kept as a package var, not a method
// call baked into withContextWindows, so callers/tests can stub it exactly
// like remoteContextWindowFn already is.
var contextWindowFromManifest = defaultContextWindowFromManifest

func defaultContextWindowFromManifest(id string) (int, bool) {
	m, err := loadModelManifest()
	if err != nil {
		return 0, false
	}
	e, ok := m.Get(id)
	if !ok || e.ContextWindow <= 0 {
		return 0, false
	}
	return e.ContextWindow, true
}

// validateModelManifestEntry rejects entries that would silently break
// launch/probe logic later — empty ID, unknown engine, negative sizes.
func validateModelManifestEntry(e ModelManifestEntry) error {
	if strings.TrimSpace(e.ID) == "" {
		return errors.New("model id is required")
	}
	switch e.Engine {
	case EngineVLLM, EngineLlamaCPP, EnginePrism, EngineDaemon, EngineUserRemote:
	default:
		return fmt.Errorf("unknown engine %q: want one of %s, %s, %s, %s, %s",
			e.Engine, EngineVLLM, EngineLlamaCPP, EnginePrism, EngineDaemon, EngineUserRemote)
	}
	if e.ContextWindow < 0 {
		return fmt.Errorf("context_window must be >= 0, got %d", e.ContextWindow)
	}
	if e.GPUMemGB < 0 || e.RAMGB < 0 {
		return errors.New("gpu_mem_gb and ram_gb must be >= 0")
	}
	return nil
}
