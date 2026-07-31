package cmd

// oaica_client.go — minimal OpenAI-compatible client for the api.sprapp.com
// router, used by the `/model` interactive command. Deliberately separate
// from api.Client (Ollama's native /api/generate protocol, a different wire
// shape) — this fork's chosen architecture (OAICA_FORK_PLAN.md, option 2) is
// a thin client to our own API, not Ollama's local-inference server.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const oaicaDefaultHost = "https://api.sprapp.com"

func oaicaHost() string {
	if h := strings.TrimSpace(os.Getenv("OAICA_HOST")); h != "" {
		return strings.TrimRight(h, "/")
	}
	return oaicaDefaultHost
}

// oaicaAuthorize attaches the bearer token the router requires on every
// route except /router-health. Reads OAICA_API_KEY each call (not cached)
// so a key set mid-session takes effect on the next request.
func oaicaAuthorize(req *http.Request) {
	if key := strings.TrimSpace(os.Getenv("OAICA_API_KEY")); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

type oaicaModelList struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// oaicaListModels fetches the live model list from the router's /v1/models.
func oaicaListModels() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, oaicaHost()+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	oaicaAuthorize(req)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach %s: %w", oaicaHost(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var list oaicaModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("bad response from %s: %w", oaicaHost(), err)
	}
	names := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		names = append(names, m.ID)
	}
	return names, nil
}

type oaicaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaicaChatRequest struct {
	Model             string              `json:"model"`
	Messages          []oaicaChatMessage  `json:"messages"`
	MaxTokens         int                 `json:"max_tokens,omitempty"`
	Temperature       float64             `json:"temperature"`
	Stream            bool                `json:"stream"`
	// Some backends (small reasoning-tuned models, e.g. Qwen3.5) default to
	// emitting a hidden <think> block that can consume the entire max_tokens
	// budget, leaving `content` empty. Off by default; llama.cpp/vLLM ignore
	// unknown fields so this is a no-op on backends that don't support it.
	ChatTemplateKwargs map[string]bool `json:"chat_template_kwargs,omitempty"`
}

type oaicaChatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// oaicaChat sends a non-streaming chat completion to the router and returns
// the assistant's reply text.
func oaicaChat(model string, messages []oaicaChatMessage) (string, error) {
	reqBody := oaicaChatRequest{
		Model:              model,
		Messages:           messages,
		MaxTokens:          1024,
		Temperature:        0.4,
		Stream:             false,
		ChatTemplateKwargs: map[string]bool{"enable_thinking": false},
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, oaicaHost()+"/v1/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	oaicaAuthorize(req)
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("couldn't reach %s: %w", oaicaHost(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var out oaicaChatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("bad response (HTTP %d): %s", resp.StatusCode, string(body))
	}
	if out.Error != nil {
		return "", fmt.Errorf("%s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("empty response (HTTP %d): %s", resp.StatusCode, string(body))
	}
	msg := out.Choices[0].Message
	if msg.Content == "" && msg.ReasoningContent != "" {
		return stripThinkTags(msg.ReasoningContent), nil
	}
	return stripThinkTags(msg.Content), nil
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
	for _, n := range names {
		if n == name {
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
	req, err := http.NewRequest(http.MethodGet, oaicaHost()+"/v1/lora", nil)
	if err != nil {
		return nil, err
	}
	oaicaAuthorize(req)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach %s: %w", oaicaHost(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var list oaicaLoraList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("bad response from %s: %w", oaicaHost(), err)
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
	req, err := http.NewRequest(http.MethodPost, oaicaHost()+path, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	oaicaAuthorize(req)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("couldn't reach %s: %w", oaicaHost(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var out oaicaLoraToggleResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("bad response (HTTP %d): %s", resp.StatusCode, string(body))
	}
	if out.Error != nil {
		return "", fmt.Errorf("%s", out.Error.Message)
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
	Name         string `json:"name"`
	Origin       string `json:"origin"`
	HasAuth      bool   `json:"hasAuth"`
	UpstreamModel string `json:"upstreamModel"`
}

type oaicaProviderList struct {
	Data []oaicaProviderEntry `json:"data"`
}

// oaicaAuthList fetches the live provider registry (admin-gated). Never
// returns secret values — the router's GET response only ever includes
// hasAuth (bool), never the auth header's actual value.
func oaicaAuthList() ([]oaicaProviderEntry, error) {
	req, err := http.NewRequest(http.MethodGet, oaicaHost()+"/v1/admin/providers", nil)
	if err != nil {
		return nil, err
	}
	if ok, _ := oaicaAdminAuthorize(req); !ok {
		return nil, fmt.Errorf("OAICA_ADMIN_KEY not set — auth commands need the operator admin key, not OAICA_API_KEY")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach %s: %w", oaicaHost(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var list oaicaProviderList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("bad response from %s: %w", oaicaHost(), err)
	}
	return list.Data, nil
}

type oaicaAuthLoginRequest struct {
	Name          string `json:"name"`
	Origin        string `json:"origin"`
	AuthHeaderName string `json:"authHeaderName,omitempty"`
	UpstreamModel string `json:"upstreamModel,omitempty"`
}

type oaicaAuthLoginResponse struct {
	OK    bool   `json:"ok"`
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
		Name:          name,
		Origin:        origin,
		AuthHeaderName: authHeaderName,
		UpstreamModel: upstreamModel,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, oaicaHost()+"/v1/admin/providers", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if ok, _ := oaicaAdminAuthorize(req); !ok {
		return fmt.Errorf("OAICA_ADMIN_KEY not set — auth commands need the operator admin key, not OAICA_API_KEY")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach %s: %w", oaicaHost(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var out oaicaAuthLoginResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("bad response (HTTP %d): %s", resp.StatusCode, string(body))
	}
	if out.Error != nil {
		return fmt.Errorf("%s", out.Error.Message)
	}
	if !out.OK {
		return fmt.Errorf("registration failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// oaicaAuthLogout removes a provider from the router's live registry.
func oaicaAuthLogout(name string) error {
	req, err := http.NewRequest(http.MethodDelete, oaicaHost()+"/v1/admin/providers/"+name, nil)
	if err != nil {
		return err
	}
	if ok, _ := oaicaAdminAuthorize(req); !ok {
		return fmt.Errorf("OAICA_ADMIN_KEY not set — auth commands need the operator admin key, not OAICA_API_KEY")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach %s: %w", oaicaHost(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// oaicaAgentHost is the NeMo Agent Toolkit sidecar's base URL — a local
// FastAPI service wrapping a react_agent workflow (see /tmp/nat_workflow.yml
// on the box this was built on; the sidecar is NOT api.sprapp.com, it's a
// separate local process that itself calls api.sprapp.com as its LLM
// backend). Override via OAICA_AGENT_HOST for a different sidecar address.
func oaicaAgentHost() string {
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
	req, err := http.NewRequest(http.MethodPost, oaicaAgentHost()+"/generate", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("couldn't reach agent sidecar at %s (is it running? see OAICA_AGENT_HOST): %w", oaicaAgentHost(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agent sidecar HTTP %d: %s", resp.StatusCode, string(body))
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
		return strings.Split(stdinRaw, "\n")
	}
	if joinedPrompt != "" {
		return []string{joinedPrompt}
	}
	return nil
}

// oaicaDispatchLine mirrors generateInteractive's (cmd/interactive.go)
// /model and /lora handling for the non-TTY one-shot path — same commands,
// same underlying client calls, just without the readline/spinner UI.
// Returns (output, true, nil) if the line was a recognized command;
// (_, false, nil) if it's plain chat text the caller should send itself.
func oaicaDispatchLine(line string, activeModel *string) (string, bool, error) {
	switch {
	case strings.HasPrefix(line, "/model"):
		args := strings.Fields(line)
		if len(args) < 2 {
			return fmt.Sprintf("Usage:\n  /model <name>\n  /model list\nActive OAICA model: %s", *activeModel), true, nil
		}
		if args[1] == "list" {
			names, err := oaicaListModels()
			if err != nil {
				return "", true, err
			}
			return "Available models:\n  " + strings.Join(names, "\n  "), true, nil
		}
		requested := args[1]
		ok, names, err := oaicaModelExists(requested)
		if err != nil {
			return "", true, err
		}
		if !ok {
			return fmt.Sprintf("Unknown model '%s'. Available models:\n  %s", requested, strings.Join(names, "\n  ")), true, nil
		}
		*activeModel = requested
		return fmt.Sprintf("Switched to model '%s'", *activeModel), true, nil

	case strings.HasPrefix(line, "/lora"):
		args := strings.Fields(line)
		if len(args) < 2 {
			return "Usage:\n  /lora add <name>\n  /lora remove <name>\n  /lora list", true, nil
		}
		switch args[1] {
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
				out += fmt.Sprintf("\n  %s  (model: %s, slot: %d)", l.Name, l.Model, l.ID)
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
			return fmt.Sprintf("LoRA '%s' activated on model '%s'", args[2], model), true, nil
		case "remove":
			if len(args) < 3 {
				return "Usage: /lora remove <name>", true, nil
			}
			model, err := oaicaLoraRemove(args[2])
			if err != nil {
				return "", true, err
			}
			return fmt.Sprintf("LoRA '%s' deactivated on model '%s'", args[2], model), true, nil
		default:
			return "Usage:\n  /lora add <name>\n  /lora remove <name>\n  /lora list", true, nil
		}

	case strings.HasPrefix(line, "/agent"):
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
