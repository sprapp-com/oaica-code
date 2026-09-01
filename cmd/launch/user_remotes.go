package launch

// User-defined remotes — bring your own inference endpoint.
//
// Anyone running their own box (llama-server, prism_server, vLLM, an OpenAI
// gateway) can list it in ~/.oaica/remotes.json and have its models appear in
// the SAME picker as local and OAICA-hosted ones. Nothing routes through
// api.oaica.com, which is a convenience router, not a licence gate — see
// docs/architecture/PER_MODEL_ROUTING.md for per-model wire routing.
//
//	{
//	  "remotes": [
//	    { "name": "mybox",  "base_url": "https://kat.example.com", "api_key": "sk-..." },
//	    { "name": "lan",    "base_url": "http://192.168.1.50:8080", "api_key_env": "LAN_KEY" }
//	  ]
//	}
//
// api_key_env is preferred: it keeps the secret out of the file. If both are
// set the env var wins, so a shared/committed config can name a variable each
// user supplies privately.
//
// Protocol descriptor (optional, for per-model wire routing):
//
//	{ "name": "kat-a100b", "base_url": "http://192.168.0.50:8080",
//	  "api_key_env": "KAT_KEY", "tool_format": "freeform" }
//
//   wire          "openai" (default) or "anthropic" — which /v1 endpoint the
//                 box speaks. Drives direct-vs-translation-proxy routing.
//   tool_format   how the model emits tool calls: "tool_calls" (default for an
//                 openai wire — real OpenAI tool_calls JSON), "freeform"
//                 (free-form JSON/XML text, not structured tool_calls — e.g.
//                 kat-coder), "xml", or "none". Set "freeform" for a model that
//                 cannot satisfy an integration's structured tool loop so oaica
//                 can gate it instead of silently spiraling.
//   tool_reliable whether tool_format reliably satisfies a tool loop. Defaults
//                 true only for "tool_calls"; false for "freeform"/"xml"/"none"
//                 unless explicitly set true.
//   force_tools   set true to always warn-and-proceed past the capability
//                 gate for this remote instead of refusing, equivalent to
//                 passing --force-tools on every launch. Use once you've
//                 deliberately decided to drive an unreliable-tool-format
//                 model (e.g. kat-coder) only through an OpenAI-wire
//                 integration (opencode, codex, ...) so you don't retype the
//                 flag each time. Still prints the warning.
//
// Failure of ONE remote never hides the others (or local models): each is
// Failure of ONE remote never hides the others (or local models): each is
// queried independently and errors are collected, not propagated. A box that
// is asleep should cost you its own entry, not the whole menu.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type userRemote struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	APIKeyEnv string `json:"api_key_env"`
	// Version is the OpenAI API version prefix appended to base_url when
	// building endpoint URLs (e.g. "/v1/chat/completions"). Defaults to "v1".
	// z.ai is the notable exception: it uses "v4"
	// (https://api.z.ai/api/paas/v4/chat/completions). Leave empty for the
	// common OpenAI "v1" layout.
	Version string `json:"version"`
	// Wire is the request/response protocol the box speaks: "openai"
	// (/v1/chat/completions, the default) or "anthropic" (/v1/messages). Empty
	// defaults to "openai". Drives routing: matching wire → direct; mismatch →
	// the Anthropic↔OpenAI translation proxy.
	Wire string `json:"wire,omitempty"`
	// ToolFormat is how the model emits tool calls: "tool_calls", "freeform",
	// "xml", or "none". Empty is inferred from Wire (openai→"tool_calls",
	// anthropic→"xml"). Used by the capability gate to refuse pairings that
	// will spiral (e.g. a freeform model behind a tool_use loop).
	ToolFormat string `json:"tool_format,omitempty"`
	// ToolReliable is whether ToolFormat reliably satisfies a structured tool
	// loop. Defaults to true only for inferred "tool_calls"; false otherwise
	// unless explicitly set. See Descriptor().
	ToolReliable *bool `json:"tool_reliable,omitempty"`
	// ForceTools makes the capability gate (agent_routing.go) always
	// warn-and-proceed for this remote instead of refusing, equivalent to
	// passing --force-tools on every launch. Set this on a remote you've
	// deliberately decided to use despite an unreliable tool format (e.g. a
	// freeform-tool-calling coder model you only ever drive through an
	// OpenAI-wire integration) so you don't have to remember the flag each
	// time. Still prints the same stderr warning — this only skips the
	// refusal, not the visibility.
	ForceTools bool `json:"force_tools,omitempty"`
	// PriceInputPerM / PriceOutputPerM are informational USD-per-million-token
	// rates for this remote (see docs/PRICING.md). oaica-code has no billing
	// enforcement — these are surfaced in the launch banner only, so a human
	// driving the picker sees the rate before racking up usage. Zero means
	// "not priced" (e.g. a personal/free box) and prints nothing.
	PriceInputPerM  float64 `json:"price_input_per_m,omitempty"`
	PriceOutputPerM float64 `json:"price_output_per_m,omitempty"`
	// RoutePolicy is the local default for `oaica launch claude
	// --route-policy` (route_policy.go): local-first | remote-first | auto
	// | local-only | remote-only. The launch flag wins when both are set.
	// Invalid values fail loudly AT LAUNCH, not silently.
	RoutePolicy string `json:"route_policy,omitempty"`
}

type userRemotesFile struct {
	Remotes []userRemote `json:"remotes"`
}

// key resolves the bearer, preferring the environment so secrets need not be
// written to disk.
func (r userRemote) key() string {
	if env := strings.TrimSpace(r.APIKeyEnv); env != "" {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	return strings.TrimSpace(r.APIKey)
}

// RemoteDescriptor is the per-remote protocol metadata that decides routing
// (direct vs translation proxy) and the capability gate. Zero values mean
// "unknown / default"; consumers use the wire-format defaults.
type RemoteDescriptor struct {
	Wire         string // "openai" | "anthropic"
	ToolFormat   string // "tool_calls" | "freeform" | "xml" | "none"
	ToolReliable bool
}

// Descriptor resolves a remote's protocol descriptor with defaults: Wire
// "openai", ToolFormat inferred from Wire (openai→"tool_calls",
// anthropic→"xml"), and ToolReliable true only when the tool format is
// "tool_calls" — unless ToolReliable is explicitly set, which always wins.
func (r userRemote) Descriptor() RemoteDescriptor {
	wire := strings.ToLower(strings.TrimSpace(r.Wire))
	if wire == "" {
		wire = "openai"
	}
	tf := strings.ToLower(strings.TrimSpace(r.ToolFormat))
	if tf == "" {
		if wire == "anthropic" {
			tf = "xml"
		} else {
			tf = "tool_calls"
		}
	}
	reliable := tf == "tool_calls"
	if r.ToolReliable != nil {
		reliable = *r.ToolReliable
	}
	return RemoteDescriptor{Wire: wire, ToolFormat: tf, ToolReliable: reliable}
}

func userRemotesPath() string {
	if p := strings.TrimSpace(os.Getenv("OAICA_REMOTES_FILE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".oaica", "remotes.json")
}

// Built-in z.ai provider. Exporting Z_AI_API_KEY (e.g. in ~/.bashrc) is enough
// to make z.ai appear in the picker — no remotes.json edit. Uses the official
// z.ai platform OpenAI-compatible endpoint under its "v4" API version, so GLM
// models (e.g. glm-5.3) are discoverable and selectable.
const (
	zaiName    = "zai"
	zaiBaseURL = "https://api.z.ai/api/paas"
	zaiEnvKey  = "Z_AI_API_KEY"
)

// Built-in OpenRouter provider. Exporting OPENROUTER_API_KEY is enough to put
// every OpenRouter model (~400) in the picker as "openrouter/<vendor>/<id>",
// searchable with type-to-filter. Ids keep their "vendor/" prefix -- see
// remoteDisplayID; OpenRouter rejects the stripped form as ambiguous.
//
// This replaces the per-model pattern ("openrouter-ox-alpha" pinned to one
// id) that needed a remotes.json edit for every model tried.
const (
	openrouterName    = "openrouter"
	openrouterBaseURL = "https://openrouter.ai/api"
	openrouterEnvKey  = "OPENROUTER_API_KEY"
)

// Built-in Ollama Cloud provider (ollama.com's hosted models over its
// OpenAI-compatible API, https://ollama.com/v1). Exporting OLLAMA_API_KEY is
// enough to list every cloud model in the picker as "ollama-cloud/<id>" --
// no local Ollama daemon and no `ollama signin` needed, unlike the
// daemon-proxied ":cloud" aliases. Named "ollama-cloud", NOT "ollama": the
// bare "ollama/" prefix already selects the LOCAL daemon in
// resolveLaunchEndpoint (see hasSourcePrefix), and a remote of that name
// would be unreachable behind it.
const (
	ollamaCloudName    = "ollama-cloud"
	ollamaCloudBaseURL = "https://ollama.com"
	ollamaCloudEnvKey  = "OLLAMA_API_KEY"
)

// catalogProviders is the built-in directory of first-party inference
// providers (the "comprehensive" list — opencode builds its picker from
// models.dev, whose catalog has no endpoint URLs; this table is the
// canonical endpoints each provider's OpenAI- or Anthropic-compatible API
// lives at). A provider appears in the picker only while its key is in the
// environment — no key, nothing to route through, no row. A remotes.json
// entry of the same name overrides (e.g. a proxy URL for openai).
var catalogProviders = []userRemote{
	{Name: "anthropic", BaseURL: "https://api.anthropic.com", Wire: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"},
	{Name: "openai", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
	{Name: "google", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKeyEnv: "GEMINI_API_KEY"},
	{Name: "groq", BaseURL: "https://api.groq.com/openai/v1", APIKeyEnv: "GROQ_API_KEY"},
	{Name: "mistral", BaseURL: "https://api.mistral.ai/v1", APIKeyEnv: "MISTRAL_API_KEY"},
	{Name: "deepseek", BaseURL: "https://api.deepseek.com", APIKeyEnv: "DEEPSEEK_API_KEY"},
	{Name: "xai", BaseURL: "https://api.x.ai/v1", APIKeyEnv: "XAI_API_KEY"},
	{Name: "together", BaseURL: "https://api.together.xyz/v1", APIKeyEnv: "TOGETHER_API_KEY"},
	{Name: "fireworks", BaseURL: "https://api.fireworks.ai/inference/v1", APIKeyEnv: "FIREWORKS_API_KEY"},
	{Name: "cerebras", BaseURL: "https://api.cerebras.ai/v1", APIKeyEnv: "CEREBRAS_API_KEY"},
	{Name: "perplexity", BaseURL: "https://api.perplexity.ai", APIKeyEnv: "PERPLEXITY_API_KEY"},
	{Name: "alibaba", BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", APIKeyEnv: "DASHSCOPE_API_KEY"},
	{Name: "moonshot", BaseURL: "https://api.moonshot.ai/v1", APIKeyEnv: "MOONSHOT_API_KEY"},
	{Name: "zhipu", BaseURL: "https://open.bigmodel.cn/api/paas/v4", APIKeyEnv: "ZHIPU_API_KEY"},
	{Name: "minimax", BaseURL: "https://api.minimax.io/v1", APIKeyEnv: "MINIMAX_API_KEY"},
}

// builtinRemotes returns remotes that oaica knows about without config, active
// only while their credential env var is set. A user-defined remote of the
// same name in remotes.json wins (see loadUserRemotes).
func builtinRemotes() []userRemote {
	var out []userRemote
	if os.Getenv(zaiEnvKey) != "" {
		out = append(out, userRemote{
			Name:      zaiName,
			BaseURL:   zaiBaseURL,
			APIKeyEnv: zaiEnvKey,
			Version:   "v4",
		})
	}
	if os.Getenv(openrouterEnvKey) != "" {
		out = append(out, userRemote{
			Name:       openrouterName,
			BaseURL:    openrouterBaseURL,
			APIKeyEnv:  openrouterEnvKey,
			ToolFormat: "tool_calls",
		})
	}
	if os.Getenv(ollamaCloudEnvKey) != "" {
		out = append(out, userRemote{
			Name:       ollamaCloudName,
			BaseURL:    ollamaCloudBaseURL,
			APIKeyEnv:  ollamaCloudEnvKey,
			ToolFormat: "tool_calls",
		})
	}
	// First-party catalog providers: active while their key is set, and
	// only when the user hasn't defined their own remote of that name
	// (loadUserRemotes dedupes by name, custom wins). Each contributes its
	// full live /models list to the picker as "<provider>/<id>".
	configured := map[string]bool{}
	for _, r := range out {
		configured[r.Name] = true
	}
	for _, p := range catalogProviders {
		if configured[p.Name] || os.Getenv(p.APIKeyEnv) == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// loadUserRemotes returns the configured remotes. A missing file is normal and
// yields no error — most users have none. Built-in providers (z.ai) are merged
// in so a key exported in the shell is enough to surface them.
func loadUserRemotes() ([]userRemote, error) {
	path := userRemotesPath()
	if path == "" {
		return builtinRemotes(), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return builtinRemotes(), nil
		}
		return nil, err
	}
	var f userRemotesFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make([]userRemote, 0, len(f.Remotes))
	for _, r := range f.Remotes {
		r.Name = strings.TrimSpace(r.Name)
		r.BaseURL = strings.TrimRight(strings.TrimSpace(r.BaseURL), "/")
		if r.Name == "" || r.BaseURL == "" {
			continue // skip malformed entries rather than fail the whole file
		}
		out = append(out, r)
	}
	seen := make(map[string]bool, len(out))
	for _, r := range out {
		seen[r.Name] = true
	}
	for _, b := range builtinRemotes() {
		if !seen[b.Name] {
			out = append(out, b)
		}
	}
	return out, nil
}

// findUserRemoteForModel splits a "<remote>/<model>" picker name and returns
// the matching configured userRemote plus the bare upstream model id (the
// part after the first "/"), or ok=false if the prefix matches no remote.
// A remote named "deepseek" + model "deepseek/deepseek-v4-flash" →
// (deepseekRemote, "deepseek-v4-flash", true). The bare id is what the
// upstream OpenAI-compatible endpoint expects; the namespaced picker name
// is an oaica-only convention.
func findUserRemoteForModel(name string) (userRemote, string, bool) {
	idx := strings.Index(name, "/")
	if idx <= 0 {
		return resolveBareRemoteModel(name)
	}
	prefix := name[:idx]
	bare := name[idx+1:]
	remotes, err := loadUserRemotes()
	if err != nil {
		return userRemote{}, "", false
	}
	for _, r := range remotes {
		if r.Name == prefix {
			return r, bare, true
		}
	}
	return userRemote{}, "", false
}

// bareRemoteModelIndex lists every "<remote>/<id>" the configured remotes
// serve, keyed by the bare id. Built from userRemoteLaunchModels on EVERY
// call (there is no per-process cache), so each bare-name lookup through
// resolveBareRemoteModel is a full remote sweep — up to fetchRemoteModels'
// 6s timeout per unreachable remote. Tests that resolve bare names must
// stub this (stubBareIndex) or userRemoteLaunchModels; the picker already
// fetches the same lists, so in production this is the same network cost,
// not an extra one. Overridable in tests.
var bareRemoteModelIndex = func() map[string][]string {
	models, _ := userRemoteLaunchModels()
	idx := make(map[string][]string, len(models))
	for _, m := range models {
		if i := strings.Index(m.Name, "/"); i > 0 {
			bare := m.Name[i+1:]
			idx[bare] = append(idx[bare], m.Name)
		}
	}
	return idx
}

// resolveBareRemoteModel maps a bare model name (no "/") to the ONE remote
// that serves it. This is the fix for the "Download <model>?" trap: a user
// typing `--model kat-awq` -- the exact id shown by their own kat-awq box --
// was routed to the Ollama-registry pull path because only "<remote>/<id>"
// was recognised as remote. Now, if exactly one configured remote serves
// that bare id, it resolves as if the user had typed the full form.
//
// Ambiguity is deliberately NOT resolved: if two remotes both serve
// "deepseek-chat", picking one silently would send traffic to the wrong
// box. The caller then falls through to the old behaviour and the user is
// told to disambiguate with "<remote>/<id>".
func resolveBareRemoteModel(bare string) (userRemote, string, bool) {
	bare = strings.TrimSpace(bare)
	if bare == "" || strings.Contains(bare, "/") {
		return userRemote{}, "", false
	}
	full := bareRemoteModelIndex()[bare]
	if len(full) != 1 {
		return userRemote{}, "", false
	}
	prefix := full[0][:strings.Index(full[0], "/")]
	remotes, err := loadUserRemotes()
	if err != nil {
		return userRemote{}, "", false
	}
	for _, r := range remotes {
		if r.Name == prefix {
			return r, bare, true
		}
	}
	return userRemote{}, "", false
}

// RemoteEndpoint is the resolved, ready-to-hit endpoint for a picker model that
// lives on a user-defined remote: the direct base URL (including the /v1 or /v4
// version prefix), the bearer token, the bare upstream model id, and the
// protocol descriptor used by the capability gate.
type RemoteEndpoint struct {
	Name    string // remote.Name — provider id in integration catalogs
	BaseURL string // r.openAIBase() — includes the /v1 (or /v4) version prefix
	Token   string // r.key(), resolved once at build time — see TokenEnv for why this is a fallback, not the source of truth
	// TokenEnv is remote.APIKeyEnv, carried through so the proxy can re-read
	// the credential from the environment on every request instead of
	// caching whatever it resolved to when the launch proxy started. A
	// process built before an env var was exported (or before it was
	// rotated) otherwise keeps rejecting/using the stale value for its
	// entire lifetime — hit in production 2026-08-29 (a client box's
	// OAICA_GATEWAY_KEY was exported to ~/.bashrc after `oaica launch
	// claude` had already started; every request 401'd until the process
	// was killed and relaunched). Empty means Token came from a literal
	// api_key in remotes.json, which has no live source to re-read.
	TokenEnv        string
	UpstreamModel   string // bare id the remote expects (part after the first "/")
	Wire            string
	ToolFormat      string
	ToolReliable    bool
	ForceTools      bool   // remote.ForceTools — skip the capability gate's refusal for this remote
	RoutePolicy     string // remote.RoutePolicy — default --route-policy for launches using this endpoint
	PriceInputPerM  float64
	PriceOutputPerM float64
}

// resolveRemoteEndpoint splits a "<remote>/<model>" picker name and resolves the
// full endpoint the integration should talk to directly. Returns ok=false for
// anything that is not a user remote (local, cloud, ":local" — those use ":" or
// no prefix at the split position, so findUserRemoteForModel misses them). The
// base URL reuses openAIBase() so the /v1 (or /v4) version prefix is respected.
func resolveRemoteEndpoint(model string) (RemoteEndpoint, bool) {
	remote, bare, ok := findUserRemoteForModel(model)
	if !ok {
		return RemoteEndpoint{}, false
	}
	d := remote.Descriptor()
	return RemoteEndpoint{
		Name:            remote.Name,
		BaseURL:         remote.openAIBase(),
		Token:           remote.key(),
		TokenEnv:        strings.TrimSpace(remote.APIKeyEnv),
		UpstreamModel:   bare,
		Wire:            d.Wire,
		ToolFormat:      d.ToolFormat,
		ToolReliable:    d.ToolReliable,
		ForceTools:      remote.ForceTools,
		RoutePolicy:     remote.RoutePolicy,
		PriceInputPerM:  remote.PriceInputPerM,
		PriceOutputPerM: remote.PriceOutputPerM,
	}, true
}

// remoteBaseURL normalizes a remote's base_url to a form without a trailing
// "/" or "/v1" version prefix. Remotes are configured both ways — some include
// the OpenAI version prefix in base_url ("https://api.deepseek.com/v1"), some
// don't ("http://192.168.1.50:8080"). openAIBase appends "/<version>" exactly
// once, so a trailing /v1 here would produce /v1/v1/<endpoint> (404).
func remoteBaseURL(r userRemote) string {
	b := strings.TrimRight(strings.TrimSpace(r.BaseURL), "/")
	b = strings.TrimSuffix(b, "/v1")
	return b
}

// openAIBase returns the remote's full OpenAI base including its API version
// prefix, the value endpoint URLs are appended to. Almost all OpenAI-compatible
// endpoints version under "v1" ("https://api.deepseek.com/v1/chat/completions");
// z.ai versions under "v4" ("https://api.z.ai/api/paas/v4/chat/completions").
// The version defaults to "v1"; set userRemote.Version to override.
func (r userRemote) openAIBase() string {
	v := strings.Trim(strings.TrimSpace(r.Version), "/")
	if v == "" {
		v = "v1"
	}
	return remoteBaseURL(r) + "/" + v
}

// fetchRemoteModels lists one remote's models endpoint (e.g. /v1/models).
// Short timeout: a sleeping box must not stall the picker.
func fetchRemoteModels(r userRemote) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, r.openAIBase()+"/models", nil)
	if err != nil {
		return nil, err
	}
	if k := r.key(); k != "" {
		if r.Descriptor().Wire == "anthropic" {
			// Anthropic lists models at /v1/models but authenticates with
			// x-api-key (+ anthropic-version), not a Bearer header.
			req.Header.Set("x-api-key", k)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	}
	resp, err := (&http.Client{Timeout: 6 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", r.Name, resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(payload.Data))
	for _, d := range payload.Data {
		if id := strings.TrimSpace(d.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// remoteDisplayID turns a remote's raw model id into the bare id the picker
// shows after "<remote>/" and, crucially, the id that gets sent BACK to the
// remote as the upstream model. Two remote families disagree about what an
// id looks like:
//
//   - llama-server reports the GGUF FILE PATH as the id
//     ("/dev/shm/oaica_malay35b_plain_q4km.gguf"). Only the basename is a
//     usable name, so the directory prefix and .gguf suffix are dropped.
//   - Aggregators (OpenRouter, OpenCode Zen) use "vendor/model" ids
//     ("deepseek/deepseek-chat"). The vendor prefix is part of the id:
//     OpenRouter rejects the stripped form as ambiguous ("'deepseek-chat'
//     matches multiple models ... use the full model ID"), confirmed
//     empirically 2026-08-26. Stripping it here silently broke every
//     aggregator model in the picker.
//
// The two are told apart by the leading "/": an absolute path is a file
// (strip to basename), anything else is an opaque id (keep it whole).
func remoteDisplayID(id string) string {
	display := id
	if strings.HasPrefix(id, "/") {
		if idx := strings.LastIndex(id, "/"); idx >= 0 {
			display = id[idx+1:]
		}
	}
	return strings.TrimSuffix(display, ".gguf")
}

// userRemoteLaunchModels queries every configured remote and returns picker
// entries named "<remote>/<model>". Errors are returned alongside the models,
// never instead of them.
//
// Package var so tests can replace the remote sweep: it is the single network
// call behind both the picker inventory (modelInventory.load) and the bare-id
// index (bareRemoteModelIndex), and every configured remote costs up to
// fetchRemoteModels' 6s timeout when it is unreachable. The default is the
// live sweep.
var userRemoteLaunchModels = userRemoteLaunchModelsLive

// userRemoteLaunchModelsLive is the real remote sweep behind
// userRemoteLaunchModels.
//
// Remotes are fetched CONCURRENTLY, not one at a time: fetchRemoteModels has a
// 6s timeout per remote, and a real config can easily list a dozen-plus boxes
// (LAN, external, cloud). Sequentially that's minutes in the worst case for
// one sleeping/slow box to cost the whole picker; in parallel the wall-clock
// cost is bounded by the single slowest remote, ~6s worst case. Results are
// reassembled in the original remotes.json order so the picker stays
// deterministic across runs regardless of which goroutine finishes first.
// remoteModelsCacheTTL is how long a remote's /models answer is trusted on
// disk; remoteModelsErrorTTL bounds how long a FAILED fetch is remembered
// (shorter — a box that was asleep may wake up any minute).
const (
	remoteModelsCacheTTL = 10 * time.Minute
	remoteModelsErrorTTL = 2 * time.Minute
)

type remoteModelsCacheFile struct {
	SavedAt   time.Time `json:"saved_at"`
	TTLSecond float64   `json:"ttl_seconds"`
	Error     string    `json:"error,omitempty"`
	IDs       []string  `json:"ids,omitempty"`
}

// remoteModelsCachePath is one file per remote under ~/.oaica/cache/models/.
// A section cache, not a bundle cache: one dead remote no longer invalidates
// (or re-triggers) every other source's fetch.
func remoteModelsCachePath(remoteName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, remoteName)
	return filepath.Join(home, ".oaica", "cache", "models", "remote-"+safe+".json"), nil
}

// fetchRemoteModelsCached wraps fetchRemoteModels with a per-remote disk
// cache: fresh within remoteModelsCacheTTL, remembered failure within
// remoteModelsErrorTTL (a sleeping box costs its timeout once, not once per
// launch), refetched otherwise. Atomic rename, best-effort — a cache write
// failure just means the next launch re-probes.
func fetchRemoteModelsCached(r userRemote) ([]string, error) {
	path, err := remoteModelsCachePath(r.Name)
	if err != nil {
		return fetchRemoteModels(r)
	}
	if b, rerr := os.ReadFile(path); rerr == nil {
		var f remoteModelsCacheFile
		if json.Unmarshal(b, &f) == nil {
			ttl := f.TTLSecond
			if ttl <= 0 {
				ttl = remoteModelsCacheTTL.Seconds()
			}
			if time.Since(f.SavedAt) < time.Duration(ttl*float64(time.Second)) {
				if f.Error != "" {
					return nil, errors.New(f.Error)
				}
				return f.IDs, nil
			}
		}
	}

	ids, ferr := fetchRemoteModels(r)
	if ferr != nil {
		// Remember the failure briefly so a sleeping box costs its timeout
		// once per window, not once per launch.
		b, _ := json.Marshal(remoteModelsCacheFile{SavedAt: time.Now(), TTLSecond: remoteModelsErrorTTL.Seconds(), Error: ferr.Error()})
		_ = writeAtomic(path, b)
		return ids, ferr
	}
	b, _ := json.Marshal(remoteModelsCacheFile{SavedAt: time.Now(), TTLSecond: remoteModelsCacheTTL.Seconds(), IDs: ids})
	_ = writeAtomic(path, b)
	return ids, nil
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func userRemoteLaunchModelsLive() ([]LaunchModel, []error) {
	remotes, err := loadUserRemotes()
	if err != nil {
		return nil, []error{err}
	}

	type result struct {
		models []LaunchModel
		err    error
	}
	results := make([]result, len(remotes))
	var wg sync.WaitGroup
	for i, r := range remotes {
		wg.Add(1)
		go func(i int, r userRemote) {
			defer wg.Done()
			ids, ferr := fetchRemoteModelsCached(r)
			if ferr != nil {
				results[i] = result{err: ferr}
				return
			}
			d := r.Descriptor()
			rm := make([]LaunchModel, 0, len(ids))
			for _, id := range ids {
				// Namespaced so two boxes serving the same model stay distinct,
				// and so the picker shows WHERE a model runs.
				display := remoteDisplayID(id)
				rm = append(rm, LaunchModel{
					Name:         r.Name + "/" + display,
					Remote:       true,
					Wire:         d.Wire,
					ToolFormat:   d.ToolFormat,
					ToolReliable: d.ToolReliable,
				}.WithCloudLimits())
			}
			results[i] = result{models: rm}
		}(i, r)
	}
	wg.Wait()

	var (
		models []LaunchModel
		errs   []error
	)
	for _, res := range results {
		if res.err != nil {
			errs = append(errs, res.err)
			continue
		}
		models = append(models, res.models...)
	}
	return models, errs
}
