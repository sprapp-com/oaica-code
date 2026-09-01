package launch

// oaica_models.go — fetches the REAL live model roster from the OAICA
// router (api.oaica.com) for the launch/picker flow (`oaica launch
// claude`, etc). The rest of this package (recommendations(),
// modelInventory) defaults to Ollama's native local-server/cloud-catalog
// APIs, which don't exist in this thin-client fork — that surfaced as the
// launch picker showing Ollama's generic upstream catalog (glm-5.2:cloud,
// kimi-k2.7-code:cloud, ...) instead of our actual models. Duplicated
// (rather than imported) from cmd/oaica_client.go's equivalents: cmd
// imports this launch package, so the reverse import isn't possible.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ollama/ollama/envconfig"
)

func oaicaLaunchHost() string {
	if h := strings.TrimSpace(os.Getenv("OAICA_HOST")); h != "" {
		return strings.TrimRight(h, "/")
	}
	return "https://api.oaica.com"
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
	APIKey    string `json:"api_key,omitempty"` // --api-key the server requires, if any
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

// oaicaLocalTagSuffix marks a picker entry as explicitly local — Ollama's
// own "name:tag" convention (e.g. "llama3:8b"), reused here so the picker
// can show "kat-coder-i-compact" (cloud) and "kat-coder-i-compact:local"
// (this box's own `oaica serve`) as two DISTINCT, both-selectable entries
// instead of one silently shadowing the other. Deliberate design choice:
// an earlier version had the bare name auto-prefer local when one was
// running, which meant picking what LOOKED like the cloud entry could
// silently route local — selecting from a menu should mean what it says.
const oaicaLocalTagSuffix = ":local"

func oaicaStripLocalTag(model string) (base string, wasLocal bool) {
	if strings.HasSuffix(model, oaicaLocalTagSuffix) {
		return strings.TrimSuffix(model, oaicaLocalTagSuffix), true
	}
	return model, false
}

// oaicaResolveHostForModel is what launch integrations (claude.go etc)
// call instead of oaicaLaunchHost() directly. Explicit OAICA_HOST always
// wins (user pinning beats everything). Otherwise: a "<model>:local" tag
// forces local (the exact entry the picker shows for a running `oaica
// serve`); the bare name always means cloud — no silent local preference,
// see oaicaLocalTagSuffix's doc for why.
func oaicaResolveHostForModel(model string) string {
	if strings.TrimSpace(os.Getenv("OAICA_HOST")) != "" {
		return oaicaLaunchHost()
	}
	base, wasLocal := oaicaStripLocalTag(model)
	if wasLocal {
		for _, e := range oaicaLocalServerEntries() {
			if e.Model == base {
				return e.Origin
			}
		}
		// Tagged :local but no matching live server (killed since the
		// picker was built, e.g.) — fall through to cloud rather than
		// silently failing; oaicaModelIsReady's own check will have
		// already caught a genuinely nonexistent model earlier.
	}
	return oaicaLaunchHost()
}

// openAIBaseURLAndKey returns the base URL, bearer key, and bare model id an
// OpenAI-speaking integration (opencode, codex, hermes, ...) should talk to for
// a selected model. For a user-remote model it resolves the direct remote
// endpoint — base URL includes the /v1 (or /v4) version prefix via
// resolveRemoteEndpoint, key is the remote's, model id is the bare upstream id.
// For local/cloud/":local" it falls back to the daemon triple — byte-identical
// to what integrations hardcoded before, so local launches are unchanged.
func openAIBaseURLAndKey(primary LaunchModel) (baseURL, apiKey, modelID string) {
	if ep, ok := resolveRemoteEndpoint(primary.Name); ok {
		return ep.BaseURL, ep.Token, ep.UpstreamModel
	}
	return envconfig.Host().String() + "/v1", "ollama", primary.Name
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
// oaicaFetchCloudModelEntries is a package var so tests can replace the
// network call. Every launch-path test previously reached the REAL router
// (api.oaica.com, or whatever OAICA_HOST pointed at) and either polluted
// the picker with live production models or failed outright when the host
// was unreachable -- 9 tests were red for exactly this reason. Tests that
// want a router stub set this; the default hits the network.
var oaicaFetchCloudModelEntries = oaicaFetchCloudModelEntriesLive

// oaicaRouterError is a non-2xx answer from the router's /v1/models. It is
// typed so callers can tell "your key is wrong" (401/403 — the user must
// act) apart from "router unreachable" (fail open to local models).
type oaicaRouterError struct {
	Status int
	Host   string
	Body   string
}

func (e *oaicaRouterError) Error() string {
	return fmt.Sprintf("HTTP %d from %s: %s", e.Status, e.Host, e.Body)
}

// isOaicaRouterAuthErr reports whether err is the router rejecting the
// credential (missing or invalid OAICA_API_KEY).
func isOaicaRouterAuthErr(err error) bool {
	var re *oaicaRouterError
	return errors.As(err, &re) && (re.Status == http.StatusUnauthorized || re.Status == http.StatusForbidden)
}

// cloudEntriesCache memoizes the live router /v1/models list per host for
// cloudEntriesTTL. resolveLaunchEndpoint consults the router list once per
// resolved model — the oversize step resolves EVERY picker candidate, and
// without this each miss on the daemon path refetches the whole list (8s
// timeout). Tests stub the oaicaFetchCloudModelEntries var, so they never
// touch this.
var cloudEntriesCache struct {
	sync.Mutex
	entries   []oaicaModelEntry
	err       error
	host      string
	expiresAt time.Time
}

const cloudEntriesTTL = 5 * time.Minute

// routerCacheFile is the cross-process router-list cache:
// ~/.oaica/cache/models/router.json. Carries the router's ETag so a refresh
// sends If-None-Match and a 304 costs no body transfer at all.
type routerCacheFile struct {
	SavedAt   time.Time         `json:"saved_at"`
	TTLSecond float64           `json:"ttl_seconds"`
	Host      string            `json:"host,omitempty"`
	ETag      string            `json:"etag,omitempty"`
	Entries   []oaicaModelEntry `json:"entries,omitempty"`
	Error     string            `json:"error,omitempty"`
}

func routerCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "cache", "models", "router.json"), nil
}

func mustRouterCachePath() string { p, _ := routerCachePath(); return p }

func loadRouterCache(host string) (routerCacheFile, bool) {
	path, err := routerCachePath()
	if err != nil {
		return routerCacheFile{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return routerCacheFile{}, false
	}
	var f routerCacheFile
	if json.Unmarshal(b, &f) != nil {
		return routerCacheFile{}, false
	}
	if f.Host != "" && f.Host != host {
		return routerCacheFile{}, false // different router: ignore entirely
	}
	return f, true
}

func saveRouterCache(host, etag string, entries []oaicaModelEntry) error {
	b, err := json.Marshal(routerCacheFile{
		SavedAt: time.Now(), TTLSecond: cloudEntriesTTL.Seconds(),
		Host: host, ETag: etag, Entries: entries,
	})
	if err != nil {
		return err
	}
	return writeAtomic(mustRouterCachePath(), b)
}

func oaicaFetchCloudModelEntriesLive() ([]oaicaModelEntry, error) {
	host := oaicaLaunchHost()
	cloudEntriesCache.Lock()
	if cloudEntriesCache.host == host && time.Now().Before(cloudEntriesCache.expiresAt) {
		entries, err := cloudEntriesCache.entries, cloudEntriesCache.err
		cloudEntriesCache.Unlock()
		return entries, err
	}
	cloudEntriesCache.Unlock()

	cached, haveCache := loadRouterCache(host)

	entries, etag, err := oaicaFetchCloudModelEntriesLiveUncached(host, cached.ETag)

	if err == nil {
		// 200 with a body: persist entries (+ etag) to disk.
		_ = saveRouterCache(host, etag, entries)
	} else if haveCache && isRouterNotModified(err) && len(cached.Entries) > 0 {
		// 304: the body we skipped is identical to the cache — reuse it and
		// extend the freshness window.
		entries = cached.Entries
		err = nil
		cached.SavedAt = time.Now()
		if b, jerr := json.Marshal(cached); jerr == nil {
			_ = writeAtomic(mustRouterCachePath(), b)
		}
	} else if haveCache && len(cached.Entries) > 0 && !isRouterAuthErr(err) {
		// Router down/unreachable but we HAVE a list: serve the stale copy
		// rather than blanking the whole catalog.
		entries = cached.Entries
		err = nil
	}

	cloudEntriesCache.Lock()
	cloudEntriesCache.host = host
	cloudEntriesCache.entries = entries
	cloudEntriesCache.err = err
	cloudEntriesCache.expiresAt = time.Now().Add(cloudEntriesTTL)
	cloudEntriesCache.Unlock()
	return entries, err
}

func isRouterNotModified(err error) bool {
	var re *oaicaRouterError
	return errors.As(err, &re) && re.Status == http.StatusNotModified
}

func isRouterAuthErr(err error) bool { return isOaicaRouterAuthErr(err) }

func oaicaFetchCloudModelEntriesLiveUncached(host, etag string) ([]oaicaModelEntry, string, error) {
	req, err := http.NewRequest(http.MethodGet, host+"/v1/models", nil)
	if err != nil {
		return nil, "", err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	oaicaLaunchAuthorize(req)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("couldn't reach %s: %w", host, err)
	}
	defer resp.Body.Close()
	respETag := resp.Header.Get("ETag")
	if resp.StatusCode == http.StatusNotModified {
		return nil, respETag, &oaicaRouterError{Status: resp.StatusCode, Host: host}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, respETag, &oaicaRouterError{Status: resp.StatusCode, Host: host, Body: strings.TrimSpace(string(body))}
	}
	var list struct {
		Data []struct {
			ID          string `json:"id"`
			Description string `json:"description"`
			Stars       int    `json:"stars"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, respETag, fmt.Errorf("bad response from %s: %w", host, err)
	}
	entries := make([]oaicaModelEntry, 0, len(list.Data))
	for _, m := range list.Data {
		entries = append(entries, oaicaModelEntry{ID: m.ID, Description: m.Description, Stars: m.Stars})
	}
	return entries, respETag, nil
}

func oaicaLiveModelEntriesErr() ([]oaicaModelEntry, error) {
	entries, cloudErr := oaicaFetchCloudModelEntries()

	// Merge in any locally-served models (`oaica serve`) as SEPARATE,
	// distinctly-tagged entries ("<model>:local", Ollama's own name:tag
	// convention) — not a replacement of the cloud entry. Selecting from a
	// menu should mean exactly what it says: pick "kat-coder-i-compact"
	// and you get cloud, pick "kat-coder-i-compact:local" and you get
	// this box's own server, both visible and choosable in the same list.
	// Skipped when OAICA_HOST is explicitly set: that's the user pinning
	// to ONE host on purpose, mixing in local entries would be surprising.
	//
	// Runs even when the cloud fetch itself failed (no network, no
	// OAICA_API_KEY configured, router down) — local discovery must not
	// depend on cloud reachability, that defeats the point of local
	// self-host. Only the FINAL error (returned when there are zero
	// entries from either source) surfaces the cloud failure reason.
	if strings.TrimSpace(os.Getenv("OAICA_HOST")) == "" {
		for _, e := range oaicaLocalServerEntries() {
			entries = append(entries, oaicaModelEntry{
				ID:          e.Model + oaicaLocalTagSuffix,
				Description: "⚡ Local — running on this machine (" + e.Origin + "), no cloud calls, works offline",
				Stars:       5,
			})
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
