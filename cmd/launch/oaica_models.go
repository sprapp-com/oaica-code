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
	"path/filepath"
	"strings"
	"time"
)

func oaicaLaunchHost() string {
	if h := strings.TrimSpace(os.Getenv("OAICA_HOST")); h != "" {
		return strings.TrimRight(h, "/")
	}
	return "https://api.sprapp.com"
}

// oaicaLocalServersRegistryEntry mirrors cmd/oaica_pull_serve.go's
// oaicaLocalServerEntry — `oaica serve` writes this file, this package
// reads it. Not a shared type (cmd imports launch, not vice versa) — raw
// JSON shape is the contract between the two files.
type oaicaLocalServersRegistryEntry struct {
	Model     string `json:"model"`
	Origin    string `json:"origin"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

func oaicaLocalServersRegistryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "local_servers.json"), nil
}

// oaicaLocalServerEntries reads the registry `oaica serve` maintains and
// returns only entries whose origin actually responds to /health right
// now — a stale entry (server crashed without cleanup running, e.g.
// kill -9) is filtered out here rather than offered as a live option.
func oaicaLocalServerEntries() []oaicaLocalServersRegistryEntry {
	path, err := oaicaLocalServersRegistryPath()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var all []oaicaLocalServersRegistryEntry
	if json.Unmarshal(b, &all) != nil {
		return nil
	}
	live := make([]oaicaLocalServersRegistryEntry, 0, len(all))
	client := &http.Client{Timeout: 800 * time.Millisecond}
	for _, e := range all {
		resp, err := client.Get(e.Origin + "/health")
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			live = append(live, e)
		}
	}
	return live
}

// oaicaResolveHostForModel is what launch integrations (claude.go etc)
// call instead of oaicaLaunchHost() directly — auto-detects a locally
// running `oaica serve` for the requested model and routes there, no
// manual OAICA_HOST needed. Explicit OAICA_HOST still wins if set (that's
// oaicaLaunchHost()'s own env-var check) — auto-detection only kicks in
// when the user hasn't overridden anything.
func oaicaResolveHostForModel(model string) string {
	if strings.TrimSpace(os.Getenv("OAICA_HOST")) != "" {
		return oaicaLaunchHost()
	}
	for _, e := range oaicaLocalServerEntries() {
		if e.Model == model {
			return e.Origin
		}
	}
	return oaicaLaunchHost()
}

func oaicaLaunchAuthorize(req *http.Request) {
	if key := oaicaLaunchAPIKeyForEnv(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// oaicaLaunchAPIKeyForEnv resolves the key the same way oaicaLaunchAuthorize
// does (env var, falling back to the file `oaica signin` writes) but
// returns the raw string — used to build ANTHROPIC_AUTH_TOKEN for a
// launched integration's own process env, not just this package's own
// HTTP calls.
func oaicaLaunchAPIKeyForEnv() string {
	key := strings.TrimSpace(os.Getenv("OAICA_API_KEY"))
	if key == "" {
		key = oaicaLaunchSavedAPIKey()
	}
	return key
}

// oaicaLaunchSavedAPIKey duplicates cmd/oaica_client.go's oaicaSavedAPIKey
// — cmd imports this launch package, so the reverse import isn't
// possible. Must read the exact same ~/.oaica/api_key file `oaica signin`
// writes; a session that just signed in only has OAICA_API_KEY set in
// THAT process's env (os.Setenv in oaicaEnsureSignedIn) — a later
// invocation is a fresh process with no env var, so without this fallback
// every launch after the first sign-in silently 401s again.
func oaicaLaunchSavedAPIKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".oaica", "api_key"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
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

// oaicaFetchCloudModelEntries does the actual GET /v1/models call. Split
// out from oaicaLiveModelEntriesErr so local-model discovery (below) can
// proceed independently of cloud reachability/auth — a laptop running
// `oaica serve` fully offline, or with no OAICA_API_KEY configured at all,
// must still be able to see and use its own local model. Cloud failure is
// no longer fatal to the whole function; it only matters if local also
// comes up empty.
func oaicaFetchCloudModelEntries() ([]oaicaModelEntry, error) {
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

func oaicaLiveModelEntriesErr() ([]oaicaModelEntry, error) {
	entries, cloudErr := oaicaFetchCloudModelEntries()

	// Merge in any locally-served models (`oaica serve`) so the picker
	// shows BOTH cloud and local in one list — no need to set OAICA_HOST
	// and re-launch just to see what's running locally. Skipped when
	// OAICA_HOST is explicitly set: that's the user pinning to ONE host
	// on purpose, mixing in unrelated local entries would be surprising.
	// A local entry with the same name as a cloud one REPLACES it in the
	// list (local takes priority — it's what oaicaResolveHostForModel
	// would actually route to for that name anyway).
	//
	// Runs even when the cloud fetch itself failed (no network, no
	// OAICA_API_KEY configured, router down) — local discovery must not
	// depend on cloud reachability, that defeats the point of local
	// self-host. Only the FINAL error (returned when there are zero
	// entries from either source) surfaces the cloud failure reason.
	if strings.TrimSpace(os.Getenv("OAICA_HOST")) == "" {
		local := oaicaLocalServerEntries()
		if len(local) > 0 {
			localNames := make(map[string]bool, len(local))
			for _, e := range local {
				localNames[e.Model] = true
			}
			deduped := entries[:0]
			for _, e := range entries {
				if !localNames[e.ID] {
					deduped = append(deduped, e)
				}
			}
			entries = deduped
			for _, e := range local {
				entries = append(entries, oaicaModelEntry{
					ID:          e.Model,
					Description: "⚡ Running locally (" + e.Origin + ") — true self-host, no cloud calls",
					Stars:       0,
				})
			}
		}
	}

	if len(entries) == 0 && cloudErr != nil {
		return nil, cloudErr
	}
	return entries, nil
}

// oaicaModelIsReady reports whether name is a real OAICA model, or a valid
// "<model>+<lora>..." composite of one — the only "readiness" concept that
// applies to router-served models (see showOrPullWithPolicy's doc comment).
func oaicaModelIsReady(name string) bool {
	base := name
	if idx := strings.Index(name, "+"); idx >= 0 {
		base = name[:idx]
	}
	for _, m := range oaicaLiveModels() {
		if m == base {
			return true
		}
	}
	return false
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
