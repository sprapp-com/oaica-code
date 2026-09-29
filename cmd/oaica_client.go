package cmd

// oaica_client.go — minimal OpenAI-compatible client for the api.oaica.com
// router, used by the `/model` interactive command. Deliberately separate
// from api.Client (Ollama's native /api/generate protocol, a different wire
// shape) — this fork's chosen architecture (OAICA_FORK_PLAN.md, option 2) is
// a thin client to our own API, not Ollama's local-inference server.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
	"github.com/ollama/ollama/cmd/internal/httpbody"
	"github.com/ollama/ollama/cmd/launch"
)

const oaicaDefaultHost = "https://api.oaica.com"

// oaicaHost is the router URL every request and every message uses. A
// credential embedded in OAICA_HOST's userinfo is deliberately NOT part of it:
// it is promoted to the bearer below (see oaicaUserinfoKey), which is what
// stops the key from riding along in URLs — and therefore in net/http's
// transport errors, in `ps` output and in every "couldn't reach %s" line this
// package prints. Same fix, same reasoning as a remote's base_url
// (launch.splitRemoteUserinfo).
func oaicaHost() string {
	clean, _ := launch.SplitUserinfoCredential(oaicaHostRaw())
	return clean
}

// oaicaHostRaw is OAICA_HOST as configured, userinfo included. Only the
// credential promotion needs this.
func oaicaHostRaw() string {
	if h := strings.TrimSpace(os.Getenv("OAICA_HOST")); h != "" {
		return strings.TrimRight(h, "/")
	}
	return oaicaDefaultHost
}

// oaicaUserinfoKey is the key OAICA_HOST carried in its userinfo, if any —
// the last fallback in oaicaAuthorize, since stripping it from the URL would
// otherwise have silently de-authenticated that setup.
func oaicaUserinfoKey() string {
	_, token := launch.SplitUserinfoCredential(oaicaHostRaw())
	return token
}

// oaicaAuthorize attaches the bearer token the router requires on every
// route except /router-health. Reads OAICA_API_KEY each call (not cached)
// so a key set mid-session takes effect on the next request. Falls back to
// a key saved via `oaica signin` (~/.oaica/api_key) when the env var isn't
// set — the env var always wins if both are present.
func oaicaAuthorize(req *http.Request) {
	if key := oaicaSentAPIKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// oaicaSentAPIKey is the credential oaicaAuthorize puts on the wire: the
// OAICA_API_KEY env var, else the key saved by `oaica signin`, else the one
// OAICA_HOST carries in its userinfo. oaicaDiagnosis scrubs with this same
// function, so what gets hidden cannot drift from what was sent.
func oaicaSentAPIKey() string {
	if key := strings.TrimSpace(os.Getenv("OAICA_API_KEY")); key != "" {
		return key
	}
	if key := oaicaSavedAPIKey(); key != "" {
		return key
	}
	return oaicaUserinfoKey()
}

// oaicaDiagnosis sanitizes gateway-authored text on its way into a CLI error.
// A vendor's refusal routinely quotes the request URL AND names the key it
// rejected, bare, as ordinary prose ("invalid api key sk-live-…") — a string no
// pattern can tell from a word, so only the caller that sent it can name it.
// This is the CLI-side twin of the proxy's redactUpstreamDiagnosis
// (2026-09-26 audit, tenth round).
func oaicaDiagnosis(text string) string {
	secrets := []string{oaicaSentAPIKey()}
	// The router admin key rides as the bearer of `oaica router list/login/logout`, whose error bodies
	// come through here (2026-09-29 audit, round 122, F122-L1-4).
	if admin := strings.TrimSpace(os.Getenv("OAICA_ADMIN_KEY")); admin != "" {
		secrets = append(secrets, admin)
	}
	// The weights-distribution licence is a credential too, and it travels as
	// a bearer on the manifest request — the one request that carries it — so
	// an error body echoing the Authorization header would print it in full
	// (2026-09-26 audit, sixteenth round).
	if license := oaicaLicenseKey(); license != "" {
		secrets = append(secrets, license)
	}
	// The agent sidecar is reached at its own address, which may carry its own
	// credential in the userinfo — separate from the router key above.
	if _, tok := launch.SplitUserinfoCredential(oaicaAgentHostRaw()); tok != "" {
		secrets = append(secrets, tok)
	}
	for _, secret := range secrets {
		text = launch.RedactDiagnosis(text, secret)
	}
	return text
}

// oaicaDiagnosisBody is oaicaDiagnosis for a response body: shape and literal
// redaction, and a length a person can actually read. The router's answer to a
// failed request is not bounded by anything this process controls
// (httpbody.DefaultMax is 64 MiB), and this is the string a user pastes into a
// support ticket.
func oaicaDiagnosisBody(body []byte) string {
	// Redacted BEFORE it is cut: cut first, a key that crosses the boundary is left as a fragment the
	// exact-match redaction cannot see, and 21 of its 40 characters printed (2026-09-29 audit,
	// round 123, F123-L1-1).
	return truncateForError([]byte(oaicaDiagnosis(string(body))))
}

// oaicaPrintableCells renders each of a router-supplied list's entries for a
// one-line report, through the same rule the rest of the CLI uses
// (launch.PrintableCell): an ordinary entry as-is, one carrying a control
// character in Go-quoted form. The one-shot dispatcher builds its model and
// LoRA lists from the ROUTER's answer, and a newline in a model id or
// description forged extra rows under a header the user already trusts — the
// same forgery round 13 fixed in the interactive twin
// (2026-09-26 audit, fifteenth round).
func oaicaPrintableCells(names []string) []string { return launch.PrintableCells(names) }

// oaicaAuthHint is appended to every router auth failure so the fix arrives
// with the diagnosis instead of leaving a fresh user with a bare "missing or
// invalid API key" from the gateway (audit 0.4.6, P1-3). One hint line, max.
const oaicaAuthHint = "Run `oaica signin`, or set OAICA_API_KEY (get a key at https://oaica.com)"

// oaicaWrapAuthError turns a router error payload into a CLI error, appending
// oaicaAuthHint when the failure is about credentials. The HTTP status decides
// that, not the wording: 401 and 403 are what a router returns when the key is
// missing, wrong, or insufficient, and their message is prose it is free to
// change ("unauthorized", "invalid bearer token", a proxy's rewrite). Keyed on
// the text instead, the hint disappeared on exactly those, and appeared on
// failures nobody can sign in their way out of — a rate limit scoped to a key,
// a model that needs its own.
func oaicaWrapAuthError(status int, msg string) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("%s\n%s", msg, oaicaAuthHint)
	}
	return errors.New(msg)
}

func oaicaConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica"), nil
}

func oaicaAPIKeyPath() (string, error) {
	dir, err := oaicaConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "api_key"), nil
}

// oaicaSavedAPIKey reads the key saved by `oaica signin`, or "" if none
// (missing file, unreadable, or never signed in — never an error the
// caller needs to handle, since the env var is always the primary path).
func oaicaSavedAPIKey() string {
	path, err := oaicaAPIKeyPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func oaicaSaveAPIKey(key string) error {
	dir, err := oaicaConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path, err := oaicaAPIKeyPath()
	if err != nil {
		return err
	}
	// Atomic: this file IS the credential (the oaica.com API key), and it is
	// read by every oaica invocation while `oaica auth login` may be
	// rewriting it — an in-place O_TRUNC leaves readers an empty key
	// (2026-09-26 audit). WriteFileAtomic also re-asserts the mode, which
	// os.WriteFile never did for a hand-created or copied file.
	return fileutil.WriteFileAtomic(path, []byte(key+"\n"), 0o600)
}

func oaicaClearAPIKey() error {
	path, err := oaicaAPIKeyPath()
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

type oaicaModelListEntry struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Stars       int    `json:"stars"`
}

type oaicaModelList struct {
	Data []oaicaModelListEntry `json:"data"`
}

// oaicaListModelsDetailed fetches the live model list from the router's
// /v1/models, including each model's "recommended for" description and
// 1-5 star rating (both set once via the router's admin API — a single
// source of truth every client reads, rather than duplicating quality
// claims in this CLI, the site, and the launch picker separately).
// Unrated models have Stars == 0 and empty Description — never fabricate
// a rating client-side.
// oaicaListModelsDetailed is a package var so tests can replace the network
// call. RunHandler consults the OAICA router on every run; the cmd tests
// only mock Ollama's OLLAMA_HOST, so without this seam each one reached the
// real router (or an unreachable OAICA_HOST) and failed with "couldn't
// reach ... /v1/models". Defaults to the live fetch.
var oaicaListModelsDetailed = oaicaListModelsDetailedLive

func oaicaListModelsDetailedLive() ([]oaicaModelListEntry, error) {
	req, err := launch.NewRedactedRequest(http.MethodGet, oaicaHost()+"/v1/models", nil)
	if err != nil {
		// OAICA_HOST can be configured with the key as URL userinfo
		// (https://KEY@host), and net/http quotes the URL back in its own
		// transport errors — so both the host we name and the wrapped cause
		// are redacted before this reaches stderr.
		return nil, launch.RedactError(fmt.Errorf("couldn't reach %s: %w", launch.RedactBaseURL(oaicaHost()), err))
	}
	oaicaAuthorize(req)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: launch.CredentialSafeRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return nil, launch.RedactError(fmt.Errorf("couldn't reach %s: %w", launch.RedactBaseURL(oaicaHost()), err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body := httpbody.ReadCappedOrEmpty(resp.Body, httpbody.DiagnosticMax, "the router error body")
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, launch.RedactError(fmt.Errorf("%s rejected the API key (HTTP %d)\n%s", launch.RedactBaseURL(oaicaHost()), resp.StatusCode, oaicaAuthHint))
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, oaicaDiagnosisBody(body))
	}
	var list oaicaModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, launch.RedactError(fmt.Errorf("bad response from %s: %w", launch.RedactBaseURL(oaicaHost()), err))
	}
	return list.Data, nil
}

func starString(n int) string {
	if n <= 0 {
		return "unrated"
	}
	s := ""
	for i := 0; i < 5; i++ {
		if i < n {
			s += "★"
		} else {
			s += "☆"
		}
	}
	return s
}

// oaicaListModels fetches just the model names — used by the existence
// gate and anywhere only the name (not the description/stars) matters.
func oaicaListModels() ([]string, error) {
	entries, err := oaicaListModelsDetailed()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, m := range entries {
		names = append(names, m.ID)
	}
	return names, nil
}

type oaicaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaicaLoraRequestEntry struct {
	ID    int     `json:"id"`
	Scale float64 `json:"scale"`
}

type oaicaChatRequest struct {
	Model       string             `json:"model"`
	Messages    []oaicaChatMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Temperature float64            `json:"temperature"`
	Stream      bool               `json:"stream"`
	// Some backends (small reasoning-tuned models, e.g. Qwen3.5) default to
	// emitting a hidden <think> block that can consume the entire max_tokens
	// budget, leaving `content` empty. Off by default; llama.cpp/vLLM ignore
	// unknown fields so this is a no-op on backends that don't support it.
	ChatTemplateKwargs map[string]bool `json:"chat_template_kwargs,omitempty"`
	// llama.cpp-specific sampling fields (ignored by OpenAI-compat backends
	// that don't recognize them). Small models degenerate into repetition
	// loops without this — documented, reproduced live via /lora on a
	// 0.8B model: "for for for for ... Garmin" (see INDUSTRY_LORA_
	// SUBSCRIPTION_DEMO.md's "Repetition-loop degeneration" finding).
	RepeatPenalty float64 `json:"repeat_penalty,omitempty"`
	NoRepeatNgram int     `json:"no_repeat_ngram_size,omitempty"`
	// Per-request LoRA scale (llama-server native — scoped to THIS request's
	// slot only, unlike the global POST /v1/lora/add|remove toggle which
	// affects every concurrent caller of that model). Set via `/lora use`
	// in the REPL — local to this CLI process, never touches other users.
	Lora []oaicaLoraRequestEntry `json:"lora,omitempty"`
}

type oaicaChatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	// Usage is what --verbose reports. Optional by the OpenAI wire (a backend
	// may omit it entirely), so it is a pointer: absent and zero are different
	// answers, and only one of them may be presented as a measurement.
	Usage *oaicaChatUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// oaicaChatUsage is the token accounting a router backend reports for one
// completion, when it reports one.
type oaicaChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// activeLocalLoras holds this CLI process's own per-request LoRA choice(s),
// set via `/lora use <name>` (replaces) or `/lora stack <name>` (adds) in
// the REPL. Local process state ONLY — sent as the request-scoped `lora`
// field, never the global /v1/lora/add|remove toggle, so it affects
// nothing outside this session. Multiple entries = stacked/composed
// adapters (llama-server applies every {id,scale} pair in the array
// together) — requires the backend to have loaded all of them at launch
// via multiple --lora flags; not something this client can provision.
var activeLocalLoras []oaicaLoraRequestEntry

// oaicaChat sends a non-streaming chat completion to the router and returns
// the assistant's reply text.
// oaicaChat is a package var so tests can intercept the one-shot chat call
// without a network. Defaults to the live request.
var oaicaChat = oaicaChatLive

// oaicaChatMetered is the same request, also returning the backend's token
// counts. Separate from oaicaChat rather than folded into it because the
// counts exist for --verbose alone: every other caller would have to ignore a
// second return value, and the tests that stub oaicaChat (four of them) would
// all have to stub this one too, for a line none of them assert.
var oaicaChatMetered = oaicaChatMeteredLive

func oaicaChatLive(model string, messages []oaicaChatMessage) (string, error) {
	reply, _, err := oaicaChatComplete(model, messages)
	return reply, err
}

func oaicaChatMeteredLive(model string, messages []oaicaChatMessage) (string, oaicaChatUsage, error) {
	return oaicaChatComplete(model, messages)
}

// oaicaChatTimed sends one turn and, when verbose is on, reports on w what
// `--verbose` has always promised ("Show timings for response") — the same
// flag chat() reads on the native path via latest.Summary(). That path prints
// its summary to STDOUT; this one writes to STDERR, because the one-shot shapes
// the fork documents are piped (`oaica run <model> "prompt" | jq`,
// `> answer.txt`) and a timing line on stdout is captured together with the
// answer. Nothing here is invented: a duration is measured, token counts are
// the backend's or reported as absent (2026-09-26 audit — the flag used to
// have no reader at all on this path, so it silently did nothing).
func oaicaChatTimed(w io.Writer, verbose bool, model string, messages []oaicaChatMessage) (string, error) {
	if !verbose {
		return oaicaChat(model, messages)
	}
	start := time.Now()
	reply, usage, err := oaicaChatMetered(model, messages)
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fmt.Fprintf(w, "total duration: %s (request failed)\n", elapsed)
		return reply, err
	}
	fmt.Fprintf(w, "total duration: %s\n", elapsed)
	if usage.TotalTokens == 0 && usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		fmt.Fprintln(w, "token counts: not reported by this backend")
		return reply, nil
	}
	fmt.Fprintf(w, "tokens: %d prompt, %d completion (%d total)\n", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	return reply, nil
}

func oaicaChatComplete(model string, messages []oaicaChatMessage) (string, oaicaChatUsage, error) {
	reqBody := oaicaChatRequest{
		Model:              model,
		Messages:           messages,
		MaxTokens:          1024,
		Temperature:        0.4,
		Stream:             false,
		ChatTemplateKwargs: map[string]bool{"enable_thinking": false},
		RepeatPenalty:      1.3,
		NoRepeatNgram:      3,
	}
	if len(activeLocalLoras) > 0 {
		reqBody.Lora = activeLocalLoras
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", oaicaChatUsage{}, err
	}
	req, err := launch.NewRedactedRequest(http.MethodPost, oaicaHost()+"/v1/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", oaicaChatUsage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	oaicaAuthorize(req)
	client := &http.Client{Timeout: 120 * time.Second, CheckRedirect: launch.CredentialSafeRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return "", oaicaChatUsage{}, launch.RedactError(fmt.Errorf("couldn't reach %s: %w", launch.RedactBaseURL(oaicaHost()), err))
	}
	defer resp.Body.Close()
	body, err := httpbody.ReadCapped(resp.Body, httpbody.DefaultMax, "the router response")
	if err != nil {
		return "", oaicaChatUsage{}, err
	}
	var out oaicaChatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", oaicaChatUsage{}, fmt.Errorf("bad response (HTTP %d): %s", resp.StatusCode, oaicaDiagnosisBody(body))
	}
	if out.Error != nil {
		return "", oaicaChatUsage{}, oaicaWrapAuthError(resp.StatusCode, oaicaRouterMessage(out.Error.Message))
	}
	if len(out.Choices) == 0 {
		return "", oaicaChatUsage{}, fmt.Errorf("empty response (HTTP %d): %s", resp.StatusCode, oaicaDiagnosisBody(body))
	}
	var usage oaicaChatUsage
	if out.Usage != nil {
		usage = *out.Usage
	}
	msg := out.Choices[0].Message
	if msg.Content == "" && msg.ReasoningContent != "" {
		return stripThinkTags(msg.ReasoningContent), usage, nil
	}
	return stripThinkTags(msg.Content), usage, nil
}

// Some backends (MiniMax M3, notably — enable_thinking/reasoning.enabled are
// silently IGNORED by its endpoint) inline a literal <think>...</think>
// block into `content` itself rather than using a separate reasoning_content
// field. Strip it so the CLI shows only the actual answer.
var thinkTagRe = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)

func stripThinkTags(s string) string {
	return strings.TrimSpace(thinkTagRe.ReplaceAllString(s, ""))
}

// oaicaModelExists checks a candidate name against the live /v1/models list.
func oaicaModelExists(name string) (bool, []string, error) {
	names, err := oaicaListModels()
	if err != nil {
		return false, nil, err
	}
	// Composite "<model>+<lora1>+<lora2>" names (router's stacked-LoRA
	// syntax, see prism-api-router's extractModelName) never appear in
	// /v1/models — they're generated syntax, not enumerated entries.
	// Only the base model needs to exist here; the router validates the
	// lora segments itself and returns a clear error if any are bad.
	checkName := name
	if idx := strings.Index(name, "+"); idx >= 0 {
		checkName = name[:idx]
	}
	for _, n := range names {
		if n == checkName {
			return true, names, nil
		}
	}
	return false, names, nil
}

type oaicaLoraListEntry struct {
	Name  string `json:"name"`
	Model string `json:"model"`
	ID    int    `json:"id"`
}

type oaicaLoraList struct {
	Data []oaicaLoraListEntry `json:"data"`
}

// oaicaListLoras fetches the configured LoRA adapters from the router.
// Note: this lists what's CONFIGURED (pre-loaded on a backend), not live
// on/off state — llama-server doesn't expose a GET for that, see
// prism-api-router/src/index.ts's /v1/lora handler.
func oaicaListLoras() ([]oaicaLoraListEntry, error) {
	req, err := launch.NewRedactedRequest(http.MethodGet, oaicaHost()+"/v1/lora", nil)
	if err != nil {
		return nil, err
	}
	oaicaAuthorize(req)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: launch.CredentialSafeRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return nil, launch.RedactError(fmt.Errorf("couldn't reach %s: %w", launch.RedactBaseURL(oaicaHost()), err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body := httpbody.ReadCappedOrEmpty(resp.Body, httpbody.DiagnosticMax, "the router error body")
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, oaicaDiagnosisBody(body))
	}
	var list oaicaLoraList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, launch.RedactError(fmt.Errorf("bad response from %s: %w", launch.RedactBaseURL(oaicaHost()), err))
	}
	return list.Data, nil
}

type oaicaLoraToggleRequest struct {
	Name string `json:"name"`
}

type oaicaLoraToggleResponse struct {
	OK    bool   `json:"ok"`
	Model string `json:"model"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func oaicaLoraToggle(path, name string) (string, error) {
	buf, err := json.Marshal(oaicaLoraToggleRequest{Name: name})
	if err != nil {
		return "", err
	}
	req, err := launch.NewRedactedRequest(http.MethodPost, oaicaHost()+path, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	oaicaAuthorize(req)
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: launch.CredentialSafeRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return "", launch.RedactError(fmt.Errorf("couldn't reach %s: %w", launch.RedactBaseURL(oaicaHost()), err))
	}
	defer resp.Body.Close()
	body, err := httpbody.ReadCapped(resp.Body, httpbody.DefaultMax, "the router response")
	if err != nil {
		return "", err
	}
	var out oaicaLoraToggleResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("bad response (HTTP %d): %s", resp.StatusCode, oaicaDiagnosisBody(body))
	}
	if out.Error != nil {
		return "", oaicaWrapAuthError(resp.StatusCode, oaicaRouterMessage(out.Error.Message))
	}
	return out.Model, nil
}

// oaicaLoraAdd activates a pre-loaded LoRA adapter on its backend model.
func oaicaLoraAdd(name string) (string, error) {
	return oaicaLoraToggle("/v1/lora/add", name)
}

// oaicaLoraRemove deactivates a LoRA adapter (scale=0), without unloading it.
func oaicaLoraRemove(name string) (string, error) {
	return oaicaLoraToggle("/v1/lora/remove", name)
}

// oaicaAdminAuthorize attaches the ADMIN_KEY bearer token — a SEPARATE,
// higher-privilege credential from OAICA_API_KEY. Only `auth login/list/
// logout` use this: those calls can add or overwrite ANY model backend for
// EVERY caller of the router, so they must never share the client-facing
// key. Regular /model and /lora usage stays on OAICA_API_KEY via
// oaicaAuthorize above.
func oaicaAdminAuthorize(req *http.Request) (bool, string) {
	key := strings.TrimSpace(os.Getenv("OAICA_ADMIN_KEY"))
	if key == "" {
		return false, ""
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return true, key
}

type oaicaProviderEntry struct {
	Name          string `json:"name"`
	Origin        string `json:"origin"`
	HasAuth       bool   `json:"hasAuth"`
	UpstreamModel string `json:"upstreamModel"`
}

type oaicaProviderList struct {
	Data []oaicaProviderEntry `json:"data"`
}

// oaicaPrintedOrigin is the ONE place the auth verbs render a provider origin.
//
// An origin is a URL this project's convention allows to carry a credential in
// its userinfo (the same shape a remote's base_url may have), and this text
// goes to terminals, shell history, CI logs and pasted support reports — which
// is exactly why launch.redactBaseURL exists. `oaica auth list` printed the
// raw value in its ORIGIN column and `oaica auth login` echoed it in its
// confirmation line, so the two commands that exist to tell you WHICH provider
// you are talking to could also print its key (2026-09-26 audit).
func oaicaPrintedOrigin(origin string) string {
	return launch.RedactBaseURL(origin)
}

// oaicaAuthList fetches the live provider registry (admin-gated). Never
// returns secret values — the router's GET response only ever includes
// hasAuth (bool), never the auth header's actual value.
func oaicaAuthList() ([]oaicaProviderEntry, error) {
	req, err := launch.NewRedactedRequest(http.MethodGet, oaicaHost()+"/v1/admin/providers", nil)
	if err != nil {
		return nil, err
	}
	if ok, _ := oaicaAdminAuthorize(req); !ok {
		return nil, fmt.Errorf("OAICA_ADMIN_KEY not set — auth commands need the operator admin key, not OAICA_API_KEY")
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: launch.CredentialSafeRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return nil, launch.RedactError(fmt.Errorf("couldn't reach %s: %w", launch.RedactBaseURL(oaicaHost()), err))
	}
	defer resp.Body.Close()
	body, err := httpbody.ReadCapped(resp.Body, httpbody.DefaultMax, "the router response")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, oaicaDiagnosisBody(body))
	}
	var list oaicaProviderList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, launch.RedactError(fmt.Errorf("bad response from %s: %w", launch.RedactBaseURL(oaicaHost()), err))
	}
	return list.Data, nil
}

type oaicaAuthLoginRequest struct {
	Name           string `json:"name"`
	Origin         string `json:"origin"`
	AuthHeaderName string `json:"authHeaderName,omitempty"`
	UpstreamModel  string `json:"upstreamModel,omitempty"`
}

type oaicaAuthLoginResponse struct {
	OK    bool `json:"ok"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// oaicaAuthLogin registers (or overwrites) a provider backend in the
// router's live registry — takes effect immediately, no redeploy. The
// provider's own API key must already be uploaded as a Worker secret named
// authHeaderName (`wrangler secret put <authHeaderName>` on the router side)
// before this is called — this command only wires up the ROUTING entry that
// points at it, it does not itself hold or transmit the provider's secret.
func oaicaAuthLogin(name, origin, authHeaderName, upstreamModel string) error {
	buf, err := json.Marshal(oaicaAuthLoginRequest{
		Name:           name,
		Origin:         origin,
		AuthHeaderName: authHeaderName,
		UpstreamModel:  upstreamModel,
	})
	if err != nil {
		return err
	}
	req, err := launch.NewRedactedRequest(http.MethodPost, oaicaHost()+"/v1/admin/providers", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if ok, _ := oaicaAdminAuthorize(req); !ok {
		return fmt.Errorf("OAICA_ADMIN_KEY not set — auth commands need the operator admin key, not OAICA_API_KEY")
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: launch.CredentialSafeRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return launch.RedactError(fmt.Errorf("couldn't reach %s: %w", launch.RedactBaseURL(oaicaHost()), err))
	}
	defer resp.Body.Close()
	body, err := httpbody.ReadCapped(resp.Body, httpbody.DefaultMax, "the router response")
	if err != nil {
		return err
	}
	var out oaicaAuthLoginResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("bad response (HTTP %d): %s", resp.StatusCode, oaicaDiagnosisBody(body))
	}
	if out.Error != nil {
		return fmt.Errorf("%s", oaicaRouterMessage(out.Error.Message))
	}
	if !out.OK {
		return fmt.Errorf("registration failed (HTTP %d): %s", resp.StatusCode, oaicaDiagnosisBody(body))
	}
	return nil
}

// oaicaAuthLogout removes a provider from the router's live registry.
//
// The name is escaped into the path (2026-09-26 audit): it comes from the
// command line, and interpolated raw a name containing "/" or "?" stops being
// one segment — the DELETE then addresses whatever that string spells on the
// operator's router, which is not the deletion the user asked for.
func oaicaAuthLogout(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("no provider name given — nothing to log out of")
	}
	req, err := launch.NewRedactedRequest(http.MethodDelete, oaicaHost()+"/v1/admin/providers/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	if ok, _ := oaicaAdminAuthorize(req); !ok {
		return fmt.Errorf("OAICA_ADMIN_KEY not set — auth commands need the operator admin key, not OAICA_API_KEY")
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: launch.CredentialSafeRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return launch.RedactError(fmt.Errorf("couldn't reach %s: %w", launch.RedactBaseURL(oaicaHost()), err))
	}
	defer resp.Body.Close()
	body, err := httpbody.ReadCapped(resp.Body, httpbody.DefaultMax, "the router response")
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, oaicaDiagnosisBody(body))
	}
	return nil
}

// oaicaAgentHost is the NeMo Agent Toolkit sidecar's base URL — a local
// FastAPI service wrapping a react_agent workflow (see /tmp/nat_workflow.yml
// on the box this was built on; the sidecar is NOT api.oaica.com, it's a
// separate local process that itself calls api.oaica.com as its LLM
// backend). Override via OAICA_AGENT_HOST for a different sidecar address.
func oaicaAgentHost() string {
	clean, _ := launch.SplitUserinfoCredential(oaicaAgentHostRaw())
	return clean
}

// oaicaAgentHostRaw is OAICA_AGENT_HOST as configured, userinfo included —
// the same shape OAICA_HOST may have, and the same reason
// (launch.SplitUserinfoCredential promotes the key to a bearer token instead of
// leaving it in the URL that error messages quote).
func oaicaAgentHostRaw() string {
	if h := strings.TrimSpace(os.Getenv("OAICA_AGENT_HOST")); h != "" {
		return strings.TrimRight(h, "/")
	}
	return "http://127.0.0.1:8600"
}

type oaicaAgentRequest struct {
	InputMessage string `json:"input_message"`
}

type oaicaAgentResponse struct {
	Value string `json:"value"`
}

// oaicaAgentRun sends a task to the NeMo Agent Toolkit sidecar's /generate
// endpoint, which runs a real ReAct tool-use loop (not a plain chat call —
// the agent decides whether/which tool to invoke, executes it, and folds
// the result back in) before returning a final answer.
func oaicaAgentRun(task string) (string, error) {
	buf, err := json.Marshal(oaicaAgentRequest{InputMessage: task})
	if err != nil {
		return "", err
	}
	req, err := launch.NewRedactedRequest(http.MethodPost, oaicaAgentHost()+"/generate", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	// oaicaAgentHost() drops the userinfo credential from the URL (that is the
	// point of it), so it has to be re-attached as a bearer or the sidecar's
	// own key-protected setups silently de-authenticate.
	if _, token := launch.SplitUserinfoCredential(oaicaAgentHostRaw()); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 120 * time.Second, CheckRedirect: launch.CredentialSafeRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return "", launch.RedactError(fmt.Errorf("couldn't reach agent sidecar at %s (is it running? see OAICA_AGENT_HOST): %w", launch.RedactBaseURL(oaicaAgentHost()), err))
	}
	defer resp.Body.Close()
	body, err := httpbody.ReadCapped(resp.Body, httpbody.DefaultMax, "the router response")
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agent sidecar HTTP %d: %s", resp.StatusCode, oaicaDiagnosisBody(body))
	}
	var out oaicaAgentResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("bad response from agent sidecar: %w", err)
	}
	return out.Value, nil
}

// stdinLinesOrArgPrompt splits raw piped stdin into individual lines (so
// /model and /lora on their own line get parsed, matching how a human
// typing into the interactive REPL would trigger them). Falls back to
// treating the whole joined prompt as one line when there's no piped stdin
// (e.g. `oaica run <model> "some prompt"` with no pipe).
func stdinLinesOrArgPrompt(stdinRaw, joinedPrompt string) []string {
	if stdinRaw != "" {
		lines := strings.Split(stdinRaw, "\n")
		// The prompt given as an argument is NOT dropped for having piped
		// stdin: `oaica run m "summarise this" < file` used to send only the
		// file's lines, so the question the user typed was never asked and
		// nothing said so (2026-09-26 audit, sixteenth round). The piped lines
		// stay first, in order, because they are "type these at the prompt" —
		// the same ordering cmd.go uses when it prepends stdin to the prompts —
		// and the argument goes last, after any command line in the pipe has
		// run.
		if joinedPrompt != "" {
			lines = append(lines, joinedPrompt)
		}
		return lines
	}
	if joinedPrompt != "" {
		return []string{joinedPrompt}
	}
	return nil
}

// isSlashCommand reports whether line invokes the command name: the line is
// exactly the name, or the name followed by whitespace and arguments.
//
// A plain prefix match is not enough, and the difference is not cosmetic. The
// one-shot path walks PIPED input line by line and sends every line that is
// not a command to the model, so this match decides whether a prompt reaches
// the model at all: `printf '/models are slow today\n' | oaica run kat-awq`
// ran `/model` with the argument "are", printed "Unknown model 'are'", and
// exited 0 with the user's line consumed and no request logged. In the REPL
// the same match turned any sentence beginning with those letters into a
// command. A command is therefore a line whose FIRST TOKEN is exactly the
// command word — the boundary has to be checked, because a prefix is
// something prose can share (2026-09-26 audit).
func isSlashCommand(line, name string) bool {
	rest, ok := strings.CutPrefix(line, name)
	if !ok {
		return false
	}
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}

// slashLineDisposition decides what the REPL does with a line that starts
// with "/" and that no command claimed.
//
// A slash line is not automatically a command. A line with more than one
// token is prose that happens to start with a slash — "a sentence beginning
// with /models" — and the one-shot path sends exactly that line to the model,
// so the REPL has to as well, or typing the line and piping it behave
// differently. A single unknown token keeps the typo guard this branch was
// written for ("/modl" gets told, rather than being answered by the model).
func slashLineDisposition(line string) slashDisposition {
	if len(strings.Fields(line)) > 1 {
		return slashProse
	}
	return slashUnknownCommand
}

type slashDisposition int

const (
	slashProse slashDisposition = iota
	slashUnknownCommand
)

// oaicaDispatchLine mirrors generateInteractive's (cmd/interactive.go)
// /model and /lora handling for the non-TTY one-shot path — same commands,
// same underlying client calls, just without the readline/spinner UI.
// Returns (output, true, nil) if the line was a recognized command;
// (_, false, nil) if it's plain chat text the caller should send itself.
func oaicaDispatchLine(line string, activeModel *string) (string, bool, error) {
	switch {
	case isSlashCommand(line, "/model"):
		args := strings.Fields(line)
		if len(args) < 2 {
			return fmt.Sprintf("Usage:\n  /model <name>\n  /model list\nActive OAICA model: %s", *activeModel), true, nil
		}
		if args[1] == "list" {
			entries, err := oaicaListModelsDetailed()
			if err != nil {
				return "", true, err
			}
			out := "Available models:"
			for _, m := range entries {
				out += fmt.Sprintf("\n  %-28s %s", launch.PrintableCell(m.ID), starString(m.Stars))
				if m.Description != "" {
					out += fmt.Sprintf("\n  %-28s %s", "", launch.PrintableCell(m.Description))
				}
			}
			return out, true, nil
		}
		requested := args[1]
		ok, names, err := oaicaModelExists(requested)
		if err != nil {
			return "", true, err
		}
		if !ok {
			return fmt.Sprintf("Unknown model '%s'. Available models:\n  %s", requested, strings.Join(oaicaPrintableCells(names), "\n  ")), true, nil
		}
		*activeModel = requested
		return fmt.Sprintf("Switched to model '%s'", *activeModel), true, nil

	case isSlashCommand(line, "/lora"):
		args := strings.Fields(line)
		if len(args) < 2 {
			return "Usage:\n  /lora add <name>\n  /lora remove <name>\n  /lora list\n  /lora use <name> [name2 ...]\n  /lora stack <name>\n  /lora off", true, nil
		}
		switch args[1] {
		case "use", "stack":
			if len(args) < 3 {
				return fmt.Sprintf("Usage: /lora %s <name> [name2 ...]", args[1]), true, nil
			}
			loras, err := oaicaListLoras()
			if err != nil {
				return "", true, err
			}
			byName := map[string]oaicaLoraListEntry{}
			for _, l := range loras {
				byName[l.Name] = l
			}
			entries := []oaicaLoraRequestEntry{}
			models := map[string]bool{}
			if args[1] == "stack" {
				entries = append(entries, activeLocalLoras...)
			}
			var addedNames []string
			for _, name := range args[2:] {
				found, ok := byName[name]
				if !ok {
					names := make([]string, len(loras))
					for i, l := range loras {
						names[i] = l.Name
					}
					return fmt.Sprintf("Unknown LoRA '%s'. Configured: %s", name, strings.Join(oaicaPrintableCells(names), ", ")), true, nil
				}
				entries = append(entries, oaicaLoraRequestEntry{ID: found.ID, Scale: 1})
				models[found.Model] = true
				addedNames = append(addedNames, name)
			}
			if len(models) > 1 {
				return "Stacked LoRAs must all belong to the same backend model (they load together into one llama-server) — mixed models given.", true, nil
			}
			activeLocalLoras = entries
			verb := "Using"
			if args[1] == "stack" {
				verb = "Stacked"
			}
			return fmt.Sprintf("%s LoRA(s) [%s] for this session only (per-request — doesn't affect other users)", verb, strings.Join(addedNames, ", ")), true, nil
		case "off":
			activeLocalLoras = nil
			return "Per-request LoRA disabled for this session.", true, nil
		case "list":
			loras, err := oaicaListLoras()
			if err != nil {
				return "", true, err
			}
			if len(loras) == 0 {
				return "No LoRA adapters configured.", true, nil
			}
			out := "Configured LoRA adapters:"
			for _, l := range loras {
				out += fmt.Sprintf("\n  %s  (model: %s, slot: %d)", launch.PrintableCell(l.Name), launch.PrintableCell(l.Model), l.ID)
			}
			return out, true, nil
		case "add":
			if len(args) < 3 {
				return "Usage: /lora add <name>", true, nil
			}
			model, err := oaicaLoraAdd(args[2])
			if err != nil {
				return "", true, err
			}
			// The model is the ROUTER's answer, not the user's text: a control
			// character in it forged a status line of its own in the reply
			// (2026-09-26 audit, sixteenth round).
			return fmt.Sprintf("LoRA '%s' activated on model '%s'", args[2], launch.PrintableCell(model)), true, nil
		case "remove":
			if len(args) < 3 {
				return "Usage: /lora remove <name>", true, nil
			}
			model, err := oaicaLoraRemove(args[2])
			if err != nil {
				return "", true, err
			}
			return fmt.Sprintf("LoRA '%s' deactivated on model '%s'", args[2], launch.PrintableCell(model)), true, nil
		default:
			return "Usage:\n  /lora add <name>\n  /lora remove <name>\n  /lora list\n  /lora use <name>\n  /lora off", true, nil
		}

	case isSlashCommand(line, "/agent"):
		task := strings.TrimSpace(strings.TrimPrefix(line, "/agent"))
		if task == "" {
			return "Usage:\n  /agent <task>", true, nil
		}
		result, err := oaicaAgentRun(task)
		if err != nil {
			return "", true, err
		}
		return result, true, nil
	}
	return "", false, nil
}
