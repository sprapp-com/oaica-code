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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

type userRemote struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	APIKeyEnv string `json:"api_key_env"`
	// CatalogOrigin records that this row came from providerCatalog(), not from
	// remotes.json — set only by providerCatalogAsUserRemotes. It gates the
	// catalog's declared model list (providerCatalogDeclaredModels): that list
	// describes the VENDOR's cloud models, so applying it to a user's own box
	// because it happens to share a provider's name offered ids the box never
	// heard of, and a launch POSTed them. json:"-" so the marker cannot be
	// forged by the file, and a user row never carries it.
	CatalogOrigin bool `json:"-"`
	// Version is the OpenAI API version prefix appended to base_url when
	// building endpoint URLs (e.g. "/v1/chat/completions"). Defaults to "v1".
	// z.ai is the notable exception: it uses "v4"
	// (https://api.z.ai/api/paas/v4/chat/completions). Leave empty for the
	// common OpenAI "v1" layout. Set it to remoteVersionNone ("none") for an
	// endpoint whose base_url is already the complete base and must not have a
	// segment appended (Google: ".../v1beta/openai"). "none" uses base_url
	// verbatim apart from a trailing slash, so a base that ends in "/v1" keeps
	// it.
	//
	// The version belongs in base_url OR in Version, never both: whatever
	// base_url ends with is preserved and Version is appended to it, so a
	// version in each place resolves to "/v4/v1" and 404s every request.
	Version string `json:"version"`
	// ModelsPath overrides where this remote's model list lives, relative to
	// base_url (or an absolute URL). Empty means openAIBase()+"/models". Set it
	// for a vendor whose model list is versioned differently from its chat
	// endpoint — see modelsURL.
	ModelsPath string `json:"models_path,omitempty"`
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
	// Weight opts this remote into RouteWeighted's consistent-hash traffic
	// split (route_policy.go) when --route-policy weighted is used: higher
	// relative to other legs' weights means a larger share of DIFFERENT
	// sessions land here, session-sticky (one conversation stays on one
	// leg). 0 (the default) means this remote is never chosen by weighted
	// distribution — it stays failover-only, unaffected by this field.
	Weight int `json:"weight,omitempty"`
	// AuthVia names an external credential store to reuse a login from
	// (auth_external.go) — "opencode" today. Set on the catalog rows whose
	// credential opencode already owns (`zai-coding-plan`, `minimax-coding-plan`,
	// ...), so a user who ran `opencode auth login` does not have to hand oaica
	// the same key a second time. Off by default and opt-in by name: see
	// provider_catalog.go's note on why a bare id match is not enough.
	AuthVia string `json:"auth_via,omitempty"`
}

type userRemotesFile struct {
	Remotes []userRemote `json:"remotes"`
}

// key resolves the bearer: the environment first (a secret stays off disk),
// then the `oaica auth login` store (auth_store.go), then an external store
// this row declared (auth_external.go — another agent CLI's own login, reused
// rather than re-prompted), then an api_key written inline in remotes.json.
// Every credential path ends here, so a provider logged in interactively — by
// oaica or by opencode — works everywhere an env var does. A key the user
// explicitly handed oaica outranks one another tool left on disk.
// keyEnvNames splits a row's api_key_env into the environment variables it
// names. Usually one. A second name is for a vendor that documents a
// different spelling than the one an earlier catalog shipped: opencode's own
// docs and models.dev both say OPENCODE_API_KEY for the Go subscription, but
// this catalog shipped OPENCODE_GO_API_KEY, so a host that exported the old
// name must keep working. Listing both is the honest way to rename a variable
// that a user has already put in a shell profile — silently dropping a
// variant is indistinguishable, from that user's seat, from the provider
// disappearing.
func (r userRemote) keyEnvNames() []string { return splitKeyEnvNames(r.APIKeyEnv) }

// splitKeyEnvNames is the one parser for an api_key_env string, shared by
// userRemote and providerCatalogEntry — two copies of it is how the two gates
// drifted apart. The comma-joined form is a LIST of acceptable variable names,
// and every consumer has to read it as one: `auth list` and the picker's
// no-match hint compared the raw string against os.Getenv, so they judged a row
// usable-with-the-OPENCODE_API_KEY-shaped name NOT usable, and told a user
// whose provider was already working to go log in.
func splitKeyEnvNames(apiKeyEnv string) []string {
	fields := strings.FieldsFunc(apiKeyEnv, func(c rune) bool {
		return c == ',' || c == ' ' || c == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// keyEnvNameSet returns the first name in the list that is set in the
// environment, or "" — the gate for every "is this row configured?" question.
// First, not any: a row naming two variables should report the one the user
// actually exported, and printing the raw comma-joined string as if it were a
// variable name is what made the two gates disagree.
func keyEnvNameSet(apiKeyEnv string) string {
	for _, env := range splitKeyEnvNames(apiKeyEnv) {
		if strings.TrimSpace(os.Getenv(env)) != "" {
			return env
		}
	}
	return ""
}

// remoteKeyEnvSet reports whether any variable this row names is set — the
// picker's gate for showing a key-gated builtin.
func remoteKeyEnvSet(r userRemote) bool { return keyEnvNameSet(r.APIKeyEnv) != "" }

// keyEnvNamesProse joins a row's acceptable variable names for a sentence:
// "OPENCODE_API_KEY or OPENCODE_GO_API_KEY". Printing the raw comma-joined
// field instead reads as a single variable name no user can export.
func keyEnvNamesProse(apiKeyEnv string) string {
	return strings.Join(splitKeyEnvNames(apiKeyEnv), " or ")
}

// authSource names WHERE this remote's credential comes from, or "none".
//
// It exists because the listing and `remote show` used to answer that question
// from two fields alone — api_key_env and api_key — while key() resolves FIVE
// sources. A remote logged in with `oaica auth login` (a stored credential), or
// one reusing another tool's login via `auth_via`, or one carrying the
// credential in base_url's userinfo (which prints REDACTED), therefore read
// "none" in `oaica remote list`/`show` while the proxy authenticated with a
// real credential. The AUTH column is exactly where a user looks to answer
// "why is this remote not working", so it must not deny the source that is
// actually in use (2026-09-26 audit, ninth round).
//
// The order mirrors key() so the two cannot disagree about which source wins.
// A declared api_key_env is reported from the CONFIG even when this process
// has no such variable set — the proxy may be launched from a shell that does,
// and the label is about the remote's configuration, not this shell's.
func (r userRemote) authSource() string {
	if env := strings.TrimSpace(r.APIKeyEnv); env != "" {
		if set := keyEnvNameSet(env); set != "" {
			return "env:" + set
		}
		return "env:" + splitKeyEnvNames(env)[0]
	}
	if storedAuthKey(r.Name) != "" {
		return "stored"
	}
	if via := strings.TrimSpace(r.AuthVia); via != "" && externalAuthKey(via, r.Name) != "" {
		return "via:" + via
	}
	if strings.TrimSpace(r.APIKey) != "" {
		return "key"
	}
	if _, token := splitRemoteUserinfo(r.BaseURL); token != "" {
		return "url"
	}
	return "none"
}

// authSourceProse renders authSource for a one-line report field: the value
// itself is never shown, only which source holds it.
func authSourceProse(src string) string {
	switch {
	case src == "none":
		return "none"
	case src == "key":
		return "<set>"
	case src == "stored":
		return "<set, stored by `oaica auth login`>"
	case src == "url":
		return "<set in base_url userinfo>"
	case strings.HasPrefix(src, "via:"):
		return "<set, reused from " + strings.TrimPrefix(src, "via:") + ">"
	default:
		return src
	}
}

// keyEnvName returns the ONE variable name a consumer that re-reads the
// credential per request should watch: the name actually set in the
// environment, or the first acceptable name when none is. TokenEnv/KeyEnv
// document themselves as an environment variable to os.Getenv on every
// request, so handing them the raw comma-joined list — which names no variable
// — made the live re-read dead: resolveKey's Getenv returned "" forever and the
// proxy silently used the launch-time value for its whole lifetime, exactly the
// stale-credential failure the field exists to prevent.
func (r userRemote) keyEnvName() string {
	if name := keyEnvNameSet(r.APIKeyEnv); name != "" {
		return name
	}
	if names := r.keyEnvNames(); len(names) > 0 {
		return names[0]
	}
	return ""
}

func (r userRemote) key() string {
	for _, env := range r.keyEnvNames() {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	if v := storedAuthKey(r.Name); v != "" {
		return v
	}
	if v := externalAuthKey(r.AuthVia, r.Name); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.APIKey); v != "" {
		return v
	}
	// Last: a credential embedded in base_url's userinfo. openAIBase strips
	// it from the URL (splitRemoteUserinfo), so this is the only place it is
	// read back — without it, a remote configured as https://key@host would
	// have lost its credential entirely once the URL stopped carrying it.
	_, token := splitRemoteUserinfo(r.BaseURL)
	return token
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

// builtinRemotes returns remotes that oaica knows about without config,
// active while a credential exists — the catalog row's own env var, an `oaica
// auth login` entry, or a login this row's auth_via declares that another
// agent CLI already stores (auth_external.go). No key, nothing to route
// through, no row.
// Sourced entirely from providerCatalog() (provider_catalog.go): the
// embedded default (cmd/launch/providers/providers.json) plus whatever
// `oaica remote sync` has pulled down. Adding a provider, adding a new
// billing plan for an existing one (e.g. z.ai's Coding Plan alongside its
// pay-per-token API), relabeling, or fixing an endpoint is a providers.json
// change — never a change here. A user-defined remote of the same name in
// remotes.json still wins (see loadUserRemotes).
func builtinRemotes() []userRemote {
	var out []userRemote
	for _, p := range providerCatalogAsUserRemotes() {
		if remoteKeyEnvSet(p) {
			out = append(out, p)
			continue
		}
		if hasStoredAuth(p.Name) {
			out = append(out, p)
			continue
		}
		if externalAuthKey(p.AuthVia, p.Name) != "" {
			out = append(out, p)
		}
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
	for i, r := range f.Remotes {
		r.Name = strings.TrimSpace(r.Name)
		r.BaseURL = strings.TrimRight(strings.TrimSpace(r.BaseURL), "/")
		if r.Name == "" || r.BaseURL == "" {
			// Skipped rather than failing the whole file, but not silently:
			// this is the user's own hand-edited store, and a row dropped here
			// is a remote that simply does not exist on every screen that
			// lists them, with nothing saying which file or which row
			// (2026-09-26 audit, tenth round).
			warnSkippedRemoteRow(path, i+1, r)
			continue
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

// warnSkippedRemoteRow reports a remotes.json row that loadUserRemotes could
// not use and therefore dropped: one with no name, no base_url, or neither.
// Such a row is not merely unused — it is a remote that is absent from the
// picker, from `remote list` and from `doctor` at once, and before this the
// user had no way to learn that the cause was a row in their own file rather
// than a box that was down, a key that was missing, or oaica ignoring them
// (2026-09-26 audit, tenth round).
//
// Reported through noticeWriter from the loader itself, like the warning for a
// store that cannot be read at all (warnUnreadableRemotesFile): this is the one
// place every path that reads the store goes through, so the listing, doctor,
// the picker and a "<remote>/<model>" lookup all say it. A skipped row is
// therefore reported once per load, which can be more than once in a command —
// deliberately, since the alternative is silence on whichever path the user
// happened to take.
func warnSkippedRemoteRow(path string, row int, r userRemote) {
	who := fmt.Sprintf("row %d", row)
	if r.Name != "" {
		// printableName, not the raw field: remotes.json is hand-edited, and a
		// name carrying a control character would forge a line in this warning
		// exactly as it would in `remote list`.
		who += " (" + printableName(r.Name) + ")"
	}
	reason := "it has no base_url"
	switch {
	case r.Name == "" && r.BaseURL == "":
		reason = "it has neither a name nor a base_url"
	case r.Name == "":
		reason = "it has no name"
	}
	fmt.Fprintf(noticeWriter(),
		"%sWarning: %s %s is skipped: %s — a remote needs both, so nothing this row configures is offered; fix the row or remove it.%s\n",
		ansiYellow, path, who, reason, ansiReset)
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
		// A file that cannot be read as a whole must not take the built-in
		// providers with it: they need no file, and a corrupt store is the
		// one case where the user most needs what still works
		// (2026-09-26 audit, tenth round).
		remotes = builtinRemotes()
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
	// TokenEnv is the ONE environment variable name the proxy re-reads on
	// every request (remote.keyEnvName()) instead of caching whatever it
	// resolved to when the launch proxy started. A process built before an env
	// var was exported (or before it was rotated) otherwise keeps
	// rejecting/using the stale value for its entire lifetime — hit in
	// production 2026-08-29 (a client box's OAICA_GATEWAY_KEY was exported to
	// ~/.bashrc after `oaica launch claude` had already started; every request
	// 401'd until the process was killed and relaunched). Empty means Token
	// came from a literal api_key in remotes.json — or that the row names no
	// variable at all — so there is no live source to re-read.
	//
	// A single name, not the row's raw api_key_env string: see keyEnvName.
	TokenEnv string
	// APIKeyEnv is the row's raw api_key_env spec (r.APIKeyEnv, comma-joined
	// when it lists more than one), for the callers that must know EVERY
	// variable the row reads a credential from — the child-environment
	// scrubber (tierPlan.credentialEnvNames) removes credentials from the
	// agent's environment, and scrubbing only TokenEnv left a second name a
	// row listed ("BOX_KEY_A,BOX_KEY_B") in place, still holding a real key
	// for the same account (2026-09-26 audit, eleventh round).
	//
	// Not for os.Getenv: that is TokenEnv, and handing it a comma-joined list
	// is the stale-credential bug keyEnvName exists to prevent. A string, not
	// a []string, so this struct stays comparable for the exact-endpoint
	// assertions the tier tests make.
	APIKeyEnv string
	// ModelsURL is r.modelsURL() — where THIS remote's model list actually
	// lives, resolved once. The version prefix is per-surface, not per-host
	// (perplexity serves chat unversioned and lists models under /v1/models),
	// so a probe that rebuilds "<base>/models" from BaseURL asks a URL the
	// vendor does not serve: the picker sweep and doctor's probe both honor
	// models_path and found the list, while the context-window probe 404'd —
	// the launch then ran with no real window, so no
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS and no context-fit clamp ceiling in the
	// proxy. Empty means BaseURL + "/models".
	ModelsURL       string
	UpstreamModel   string // bare id the remote expects (part after the first "/")
	Wire            string
	ToolFormat      string
	ToolReliable    bool
	ForceTools      bool   // remote.ForceTools — skip the capability gate's refusal for this remote
	RoutePolicy     string // remote.RoutePolicy — default --route-policy for launches using this endpoint
	Weight          int    // remote.Weight — this leg's share under --route-policy weighted
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
		TokenEnv:        remote.keyEnvName(),
		APIKeyEnv:       remote.APIKeyEnv,
		ModelsURL:       remote.modelsURL(),
		UpstreamModel:   bare,
		Wire:            d.Wire,
		ToolFormat:      d.ToolFormat,
		ToolReliable:    d.ToolReliable,
		ForceTools:      remote.ForceTools,
		RoutePolicy:     remote.RoutePolicy,
		Weight:          remote.Weight,
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
	b, _ := splitRemoteUserinfo(r.BaseURL)
	b = strings.TrimRight(strings.TrimSpace(b), "/")
	b = strings.TrimSuffix(b, "/v1")
	return b
}

// splitRemoteUserinfo separates a credential embedded in a base_url's
// userinfo from the URL itself:
//
//	https://sk-abc@api.example.com/v1  ->  ("https://api.example.com/v1", "sk-abc")
//
// Remotes do get configured this way — it is the shape that leaked a key into
// doctor output, `oaica remote list` and requests.log. It also broke auth:
// net/http silently turns userinfo into Basic auth, while oaica's own code
// reads the credential from key() and sends it as a bearer, so nothing oaica
// printed or probed agreed with what actually authenticated the request.
//
// Only a bare userinfo is promoted. `user:password@host` is a genuine Basic
// credential — rewriting it into a bearer would change how the request
// authenticates, so it is left intact and only ever redacted at print sites
// (redactCredentials).
func splitRemoteUserinfo(baseURL string) (cleanURL, token string) {
	raw := strings.TrimSpace(baseURL)
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return baseURL, ""
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		return baseURL, ""
	}
	token = u.User.Username() // no password: the username IS the credential
	if token == "" {
		return baseURL, ""
	}
	u.User = nil
	return u.String(), token
}

// remoteVersionNone is the Version value meaning "append no version segment at
// all". It exists for endpoints whose version is a path segment of base_url
// itself -- Google's OpenAI-compatible root is
// "https://generativelanguage.googleapis.com/v1beta/openai", where appending
// "/v1" gives a 404 (".../openai/v1/chat/completions"). Such a row cannot be
// fixed by defaulting, because the default is the thing that breaks it.
const remoteVersionNone = "none"

// openAIBase returns the remote's full OpenAI base including its API version
// prefix, the value endpoint URLs are appended to. Almost all OpenAI-compatible
// endpoints version under "v1" ("https://api.deepseek.com/v1/chat/completions");
// z.ai versions under "v4" ("https://api.z.ai/api/paas/v4/chat/completions"),
// as do the other rows that carry their version in the base URL -- the version
// may live in base_url OR in userRemote.Version, but only in one of them.
// The version defaults to "v1"; set userRemote.Version to override it, or to
// remoteVersionNone to append nothing.
func (r userRemote) openAIBase() string {
	v := strings.Trim(strings.TrimSpace(r.Version), "/")
	if strings.EqualFold(v, remoteVersionNone) {
		// Nothing to append, so nothing may be stripped either: the caller
		// said base_url is already the whole base. Going through
		// remoteBaseURL here would TrimSuffix("/v1") and quietly delete a
		// version the user configured -- and "/v1" is the most common way an
		// OpenAI-compatible base ends, so `--api-version none` on such a
		// remote sent every request to <host>/chat/completions (404).
		b, _ := splitRemoteUserinfo(r.BaseURL)
		return strings.TrimRight(strings.TrimSpace(b), "/")
	}
	if v == "" {
		v = "v1"
	}
	return remoteBaseURL(r) + "/" + v
}

// modelsURL is where this remote's model list lives: openAIBase()+"/models"
// unless the row says otherwise.
//
// The override exists because a vendor's version prefix is per-SURFACE, not
// per-host. Perplexity serves chat unversioned ("/chat/completions" — the
// versioned path 404s) while listing models under "/v1/models"; one `version`
// field cannot express both, so such a row sets version "none" (correct for
// every request oaica sends) plus models_path "/v1/models". Without it, the
// picker's sweep and doctor's reachability probe both ask a URL the host does
// not serve, and a working provider reads as broken.
func (r userRemote) modelsURL() string {
	p := strings.TrimSpace(r.ModelsPath)
	switch {
	case p == "":
		return r.openAIBase() + "/models"
	case strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://"):
		return p
	default:
		base, _ := splitRemoteUserinfo(r.BaseURL)
		return strings.TrimRight(strings.TrimSpace(base), "/") + "/" + strings.TrimLeft(p, "/")
	}
}

// EndpointBase is openAIBase for callers outside this package — the base URL
// oaica appends its endpoint paths to. Print sites should use THIS, not
// BaseURL: the configured base is a prefix (a v4 row's BaseURL stops at
// "/api/paas"), so a user who curls what a message showed them gets a 404 while
// the row itself works. Credentials are already stripped (splitRemoteUserinfo),
// but callers must still redact before printing (RedactBaseURL) since a token
// can ride in a query parameter.
func (r userRemote) EndpointBase() string { return r.openAIBase() }

// remoteModelsFetchTimeout bounds one remote's contribution to the picker.
// All remotes are queried concurrently, but the picker should still appear
// promptly when a VPN/LAN or cloud endpoint has gone away.
const remoteModelsFetchTimeout = 2 * time.Second

// fetchRemoteModels lists one remote's models endpoint (e.g. /v1/models).
// Short timeout: a sleeping box must not stall the picker.
func fetchRemoteModels(r userRemote) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, r.modelsURL(), nil)
	if err != nil {
		// redactErr: the picker's warning would otherwise print the remote's
		// URL, key included, when the URL carries the key as userinfo.
		return nil, redactErr(err)
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
	resp, err := (&http.Client{Timeout: remoteModelsFetchTimeout}).Do(req)
	if err != nil {
		return nil, redactErr(err)
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
// fetchRemoteModels' short timeout when it is unreachable. The default is the
// live sweep.
var userRemoteLaunchModels = userRemoteLaunchModelsLive

// userRemoteLaunchModelsLive is the real remote sweep behind
// userRemoteLaunchModels.
//
// Remotes are fetched CONCURRENTLY, not one at a time: fetchRemoteModels has a
// short timeout per remote, and a real config can easily list a dozen-plus boxes
// (LAN, external, cloud). Sequentially that's minutes in the worst case for
// one sleeping/slow box to cost the whole picker; in parallel the wall-clock
// cost is bounded by the single slowest remote, ~2s worst case. Results are
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

// remoteModelsCachePath is one file per remote ENDPOINT under
// ~/.oaica/cache/models/. A section cache, not a bundle cache: one dead remote
// no longer invalidates (or re-triggers) every other source's fetch.
//
// The key is the remote's NAME **and** its base_url, because base_url is the
// only input that decides which host the sweep talks to and `oaica remote add`
// is documented as "add or replace": keyed on the name alone, repointing a box
// at a new host kept serving the previous host's /models rows for up to
// remoteModelsCacheTTL, and the row resolved against the CURRENT base_url, so
// selecting it sent the old host's model id to the new host (2026-09-26 audit).
//
// The readable part of the name is kept for a human poking at the cache
// directory; the hash is what makes the key injective — the sanitizer alone is
// lossy, so "my box" and "my_box" (both legal remote names) collided on one
// file and each was served the other's model list.
func remoteModelsCachePath(remoteName, baseURL string) (string, error) {
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
	if len(safe) > 40 {
		safe = safe[:40]
	}
	sum := sha256.Sum256([]byte(remoteName + "\x00" + baseURL))
	return filepath.Join(home, ".oaica", "cache", "models",
		"remote-"+safe+"-"+hex.EncodeToString(sum[:])[:12]+".json"), nil
}

// fetchRemoteModelsCached wraps fetchRemoteModels with a per-remote disk
// cache: fresh within remoteModelsCacheTTL, remembered failure within
// remoteModelsErrorTTL (a sleeping box costs its timeout once, not once per
// launch), refetched otherwise. Atomic rename, best-effort — a cache write
// failure just means the next launch re-probes.
func fetchRemoteModelsCached(r userRemote) ([]string, error) {
	path, err := remoteModelsCachePath(r.Name, r.BaseURL)
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

// writeAtomic publishes b at path through a unique temp + rename. Callers are
// the auth store, the remote model cache and the license file — all read by a
// different command than the one that writes them, which is why the temp name
// must be unique per writer rather than "<path>.tmp" (2026-09-26 audit).
func writeAtomic(path string, b []byte) error {
	return fileutil.WriteFileAtomic(path, b, 0o600)
}

// remoteLaunchModels turns one remote's swept model ids into picker rows,
// falling back to (and filling in from) the ids the provider catalog
// declares. A provider that declares its models still has rows to offer
// when the sweep cannot run — the Anthropic-compatible subscription
// endpoints (z.ai's Coding Plan, MiniMax's) serve no model list at all, and
// a key-gated sweep can be refused — so only a remote with neither a sweep
// nor a declared list is a real error. Swept ids keep the live list's
// ordering and win any id collision; declared-only ids are appended in
// sorted order, since a map has none.
func remoteLaunchModels(r userRemote, sweptIDs []string, sweepErr error) ([]LaunchModel, error) {
	// The declared list is the VENDOR's, so it applies only to the catalog's
	// own row. `oaica remote add minimax-coding-plan <your own box>` used to
	// inherit MiniMax's cloud model ids (the lookup is by NAME) and offer them
	// against a box that serves none of them.
	var declared map[string]providerCatalogModelLimit
	if r.CatalogOrigin {
		declared = providerCatalogDeclaredModels(r.Name)
	}
	if sweepErr != nil && len(declared) == 0 {
		return nil, sweepErr
	}
	d := r.Descriptor()
	rm := make([]LaunchModel, 0, len(sweptIDs)+len(declared))
	seen := make(map[string]bool, len(sweptIDs))
	for _, id := range sweptIDs {
		// Namespaced so two boxes serving the same model stay distinct,
		// and so the picker shows WHERE a model runs.
		display := remoteDisplayID(id)
		seen[display] = true
		row := LaunchModel{
			Name:         r.Name + "/" + display,
			Remote:       true,
			Wire:         d.Wire,
			ToolFormat:   d.ToolFormat,
			ToolReliable: d.ToolReliable,
		}.WithCloudLimits()
		// The sweep wins the id collision, but it carries no window to
		// collide WITH: a /v1/models response lists ids. Without this, an id
		// the catalog declares a window for came out at 0 and the launch fell
		// back to defaultAgentContextLength (128000) on a vendor whose real
		// window is 400000 (2026-09-26 audit).
		if lim, ok := declared[id]; ok {
			if row.ContextLength <= 0 {
				row.ContextLength = lim.Context
			}
			if row.MaxOutputTokens <= 0 {
				row.MaxOutputTokens = lim.Output
			}
		}
		rm = append(rm, row)
	}
	extra := make([]string, 0, len(declared))
	for id := range declared {
		if !seen[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		lim := declared[id]
		rm = append(rm, LaunchModel{
			Name:            r.Name + "/" + id,
			Remote:          true,
			ToolCapable:     true,
			ContextLength:   lim.Context,
			MaxOutputTokens: lim.Output,
			Wire:            d.Wire,
			ToolFormat:      d.ToolFormat,
			ToolReliable:    d.ToolReliable,
		}.WithCloudLimits())
	}
	return rm, nil
}

func userRemoteLaunchModelsLive() ([]LaunchModel, []error) {
	remotes, errs := launchSweepRemotes()
	if len(remotes) == 0 {
		return nil, errs
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
			rm, rerr := remoteLaunchModels(r, ids, ferr)
			if rerr != nil {
				results[i] = result{err: rerr}
				return
			}
			results[i] = result{models: rm}
		}(i, r)
	}
	wg.Wait()

	var models []LaunchModel
	for _, res := range results {
		if res.err != nil {
			errs = append(errs, res.err)
			continue
		}
		models = append(models, res.models...)
	}
	return models, errs
}

// launchSweepRemotes decides which remotes a launch should sweep and what to
// report about the ones it could not read.
//
// One unreadable byte in remotes.json used to drop EVERY row, including the
// built-in providers, which need no file at all — and the picker
// (model_inventory.go) collected the error and never read it, so
// `oaica launch` simply showed a smaller menu with no reason given
// (2026-09-26 audit, tenth round). The error is still reported, and the
// built-ins are still swept.
func launchSweepRemotes() ([]userRemote, []error) {
	remotes, err := loadUserRemotes()
	if err != nil {
		return warnUnreadableRemotesFile(err), []error{err}
	}
	return remotes, nil
}

// warnUnreadableRemotesFile reports a remotes.json that could not be read as a
// remote store at all, and returns what the launch should sweep instead: the
// built-in providers, which are keyed on an environment variable or a stored
// login and need no file. Called once per launch, so a broken file is visible
// on every launch until it is fixed.
func warnUnreadableRemotesFile(err error) []userRemote {
	fmt.Fprintf(noticeWriter(),
		"%sWarning: %s is not readable as a remote store (%v) — remotes you configured there are hidden until it is fixed; the built-in providers are unaffected.%s\n",
		ansiYellow, userRemotesPath(), err, ansiReset)
	return builtinRemotes()
}
