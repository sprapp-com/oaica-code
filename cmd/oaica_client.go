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
	Model       string              `json:"model"`
	Messages    []oaicaChatMessage  `json:"messages"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature float64             `json:"temperature"`
	Stream      bool                `json:"stream"`
}

type oaicaChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
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
		Model:       model,
		Messages:    messages,
		MaxTokens:   1024,
		Temperature: 0.4,
		Stream:      false,
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
	return out.Choices[0].Message.Content, nil
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
