package launch

// oaica_models.go — fetches the REAL live model roster from the OAICA
// router (api.sprapp.com) for the launch/picker flow (`oaica launch
// claude`, etc). The rest of this package (recommendations(),
// modelInventory) defaults to Ollama's native local-server/cloud-catalog
// APIs, which don't exist in this thin-client fork — that surfaced as the
// launch picker showing Ollama's generic upstream catalog (glm-5.2:cloud,
// kimi-k2.7-code:cloud, ...) instead of our actual models. Duplicated
// (rather than imported) from cmd/oaica_client.go's equivalents: cmd
// imports this launch package, so the reverse import isn't possible.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func oaicaLaunchHost() string {
	if h := strings.TrimSpace(os.Getenv("OAICA_HOST")); h != "" {
		return strings.TrimRight(h, "/")
	}
	return "https://api.sprapp.com"
}

func oaicaLaunchAuthorize(req *http.Request) {
	if key := strings.TrimSpace(os.Getenv("OAICA_API_KEY")); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

type oaicaModelEntry struct {
	ID          string
	Description string
	Stars       int
}

// oaicaLiveModelEntries fetches /v1/models including each model's
// "recommended for" description and 1-5 star rating — set once via the
// router's admin API, a single source of truth every client (this picker,
// oaica-code's `/model list`, oaica.com) reads rather than duplicating
// quality claims. Returns nil (discarding the reason) on any failure —
// used only by oaicaLiveModels() below; requestRecommendations calls
// oaicaLiveModelEntriesErr instead so it can surface the REAL reason
// (401 missing key, network unreachable, etc) instead of a generic
// "no models available" that gives no signal on what actually broke.
func oaicaLiveModelEntries() []oaicaModelEntry {
	entries, _ := oaicaLiveModelEntriesErr()
	return entries
}

func oaicaLiveModelEntriesErr() ([]oaicaModelEntry, error) {
	req, err := http.NewRequest(http.MethodGet, oaicaLaunchHost()+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	oaicaLaunchAuthorize(req)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach %s: %w", oaicaLaunchHost(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, oaicaLaunchHost(), strings.TrimSpace(string(body)))
	}
	var list struct {
		Data []struct {
			ID          string `json:"id"`
			Description string `json:"description"`
			Stars       int    `json:"stars"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("bad response from %s: %w", oaicaLaunchHost(), err)
	}
	entries := make([]oaicaModelEntry, 0, len(list.Data))
	for _, m := range list.Data {
		entries = append(entries, oaicaModelEntry{ID: m.ID, Description: m.Description, Stars: m.Stars})
	}
	return entries, nil
}

// oaicaLiveModels fetches just the model names (used by modelInventory,
// which doesn't need descriptions/stars).
func oaicaLiveModels() []string {
	entries := oaicaLiveModelEntries()
	if entries == nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, m := range entries {
		names = append(names, m.ID)
	}
	return names
}

type oaicaLoraEntry struct {
	name  string
	model string
}

// oaicaLiveLoraEntries fetches /v1/lora — configured (not necessarily
// active) LoRA adapters, each with the base model it's registered on
// (needed to build a valid "<model>+<lora>" composite name — the router
// rejects stacking adapters registered on different backends).
func oaicaLiveLoraEntries() []oaicaLoraEntry {
	req, err := http.NewRequest(http.MethodGet, oaicaLaunchHost()+"/v1/lora", nil)
	if err != nil {
		return nil
	}
	oaicaLaunchAuthorize(req)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var list struct {
		Data []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil
	}
	entries := make([]oaicaLoraEntry, 0, len(list.Data))
	for _, l := range list.Data {
		entries = append(entries, oaicaLoraEntry{name: l.Name, model: l.Model})
	}
	return entries
}
