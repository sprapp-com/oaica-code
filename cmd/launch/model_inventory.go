package launch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/internal/fileutil"
	modelpkg "github.com/ollama/ollama/types/model"
)

// LaunchModel is the model metadata Launch passes to integration config
// writers after resolving selected model names through the per-run inventory.
type LaunchModel struct {
	Name   string
	Remote bool
	// Recommended: an OAICA router recommendation (picker "OAICA Models"
	// section). Populated on the wizard's full-inventory path so the
	// secondary step can lead our models and mark them.
	Recommended     bool
	ToolCapable     bool
	Capabilities    []modelpkg.Capability
	ContextLength   int
	MaxOutputTokens int
	EmbeddingLength int
	Size            int64
	Details         api.ModelDetails
	// Protocol descriptor for a user-remote model (see RemoteDescriptor).
	// Zero values for local/cloud entries — those route through the daemon.
	Wire         string
	ToolFormat   string
	ToolReliable bool
	// Upstream is the name the backend actually knows when Name is a
	// display-only picker id (ollama-cloud catalog: "ollama/gpt-oss" →
	// upstream "gpt-oss:cloud"). Empty = Name is itself upstream.
	Upstream string
}

type modelInfo = LaunchModel

// ModelInfo re-exports launcher model inventory details for callers.
type ModelInfo = LaunchModel

func (m LaunchModel) HasCapability(capability modelpkg.Capability) bool {
	return slices.Contains(m.Capabilities, capability)
}

func (m LaunchModel) WithCloudLimits() LaunchModel {
	if limit, ok := lookupCloudModelLimit(m.Name); ok {
		if m.ContextLength <= 0 {
			m.ContextLength = limit.Context
		}
		if m.MaxOutputTokens <= 0 {
			m.MaxOutputTokens = limit.Output
		}
	}
	return m
}

type modelInventory struct {
	client *api.Client

	mu     sync.Mutex
	loaded bool
	models []LaunchModel
	err    error
}

func newModelInventory(client *api.Client) *modelInventory {
	return &modelInventory{client: client}
}

func (i *modelInventory) Load(ctx context.Context) ([]LaunchModel, error) {
	return i.load(ctx, false)
}

func (i *modelInventory) Refresh(ctx context.Context) ([]LaunchModel, error) {
	return i.load(ctx, true)
}

// load sources the inventory from the OAICA router (/v1/models) rather
// than Ollama's native local-server List() API, which this thin-client
// fork never runs (see oaica_models.go's doc comment). Unlike Ollama's
// response, ours carries no size/context/capability metadata — those
// fields are left at zero value, which downstream code already treats as
// "unknown" (WithCloudLimits falls back to lookupCloudModelLimit, which
// simply won't match our model names and leaves them as-is).
// ollamaPickerPrefix names daemon models in the picker. Local Ollama
// models ("kat-awq", "gpt-oss:20b", ...) carry no provider hint, so
// typing "ollama" in the picker filter matched nothing (2026-09-01) for
// every model that isn't an OAICA SKU or user remote. They surface as
// "ollama/<name>" — matching resolveLaunchEndpoint's pre-existing
// "ollama/<id>" daemon-source vocabulary and the modelref-unspecial
// "/" namespace — while both the prefixed AND the bare name keep
// resolving (launchModelMatches), and a resolved entry reports the
// bare name upstream (findLaunchModel strips the prefix, so the daemon
// never sees "ollama/...").
const ollamaPickerPrefix = "ollama/"

func (i *modelInventory) load(ctx context.Context, force bool) ([]LaunchModel, error) {
	if i == nil {
		return nil, nil
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	if i.loaded && !force {
		return cloneLaunchModels(i.models), i.err
	}

	// Disk fast path: a cache file renders the picker from the last
	// successful inventory with ZERO network calls. Fresh within
	// pickerCacheTTL; within the grace window the stale list paints the
	// menu instantly and a background goroutine refreshes the real
	// inventory (rewriting the cache for the next launch); beyond grace we
	// fall through to the full load.
	if !force {
		models, stale, ok := loadPickerCache()
		if ok {
			// The daemon's own list is the one source a cache cannot hold. It
			// is runtime state with no file behind it, so `ollama pull` and
			// `ollama rm` would be invisible for up to pickerCacheTTL — while
			// `oaica model refresh`'s help promises a pull is discoverable on
			// the next launch. It is also the one source that is cheap to
			// re-read (a loopback /api/tags), so re-read it here and merge it
			// in, leaving the expensive parts — the per-remote sweeps and the
			// router — to the cache.
			models = mergeLiveDaemonRows(ctx, i.client, models)
			i.models = models
			i.err = nil
			i.loaded = true
			if stale {
				go func() {
					bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					_, _ = i.load(bgCtx, true) // force: bypass the just-returned cache
				}()
			}
			return cloneLaunchModels(i.models), nil
		}
	}

	// LOCAL models first, and independently of the router. A self-hosted user
	// may have no router at all; a hosted user's router may be down. Neither
	// should empty the picker -- previously ANY router error set i.models=nil,
	// so a dead origin (or a missing key) hid every locally pulled model too.
	models := make([]LaunchModel, 0, 8)
	seen := make(map[string]bool)
	if rows, _ := daemonPickerRows(ctx, i.client); len(rows) > 0 {
		for _, lm := range rows {
			if seen[lm.Name] {
				continue
			}
			seen[lm.Name] = true
			models = append(models, lm)
		}
	}

	// USER-DEFINED remotes (~/.oaica/remotes.json) -- anyone's own box. These
	// need no router and no OAICA account; a failure of one is collected, not
	// propagated, so a sleeping box costs only its own entry.
	if userModels, uerrs := userRemoteLaunchModels(); len(userModels) > 0 || len(uerrs) > 0 {
		for _, um := range userModels {
			if um.Name == "" || seen[um.Name] {
				continue
			}
			seen[um.Name] = true
			models = append(models, um)
		}
	}

	// CLOUD/remote models from the router. A failure here is NOT fatal when we
	// already have local ones -- it is reported, but the local list still
	// shows, so `oaica launch` stays usable fully offline.
	entries, err := oaicaLiveModelEntriesErr()
	if err != nil {
		i.models = models
		i.loaded = true
		if len(models) == 0 {
			i.err = fmt.Errorf("OAICA router: %w (check OAICA_API_KEY / OAICA_HOST)", err)
			return nil, i.err
		}
		// Degrade to local-only rather than failing outright.
		i.err = nil
		return cloneLaunchModels(i.models), nil
	}
	for _, e := range entries {
		if e.ID == "" || seen[e.ID] {
			continue
		}
		// An ollama-cloud catalog entry duplicates a model we already have
		// under its bare upstream id (the router list added "glm-5.3:cloud"
		// first) or under the daemon's own "ollama/<name>:cloud" entry
		// (local List processed before us): keep the earlier entry.
		if up := ollamaCloudUpstreamFor(e.ID); up != "" && (seen[up] || seen[e.ID+":cloud"]) {
			continue
		}
		seen[e.ID] = true
		models = append(models, LaunchModel{Name: e.ID, Remote: true, Upstream: e.Upstream}.WithCloudLimits())
	}

	i.models = models
	i.err = nil
	i.loaded = true
	savePickerCache(models)

	return cloneLaunchModels(i.models), i.err
}

// daemonPickerRows reads the local daemon's model list and names the rows the
// way the picker shows them: bare daemon models get the ollama/ prefix,
// already-namespaced ids (hf.co/..., "<remote>/<id>", ":local" tags, anything
// with a "/") keep their name. ok=false means the daemon did not answer — not
// an error, since a self-hosted user may have no daemon and a hosted user's
// may be down; the caller decides what to do without it.
func daemonPickerRows(ctx context.Context, client *api.Client) ([]LaunchModel, bool) {
	if client == nil {
		return nil, false
	}
	lst, err := client.List(ctx)
	if err != nil || lst == nil {
		return nil, false
	}
	out := make([]LaunchModel, 0, len(lst.Models))
	for _, m := range lst.Models {
		lm := launchModelFromListResponse(m)
		if lm.Name == "" {
			continue
		}
		if !strings.Contains(lm.Name, "/") && !strings.HasSuffix(lm.Name, oaicaLocalTagSuffix) {
			lm.Name = ollamaPickerPrefix + lm.Name
		}
		out = append(out, lm)
	}
	return out, true
}

// isDaemonRow reports whether a picker row came from the local daemon (or the
// router's ":local" merge of a live `oaica serve`), i.e. the rows
// mergeLiveDaemonRows replaces with a live read.
func isDaemonRow(name string) bool {
	return strings.HasPrefix(name, ollamaPickerPrefix) || strings.HasSuffix(name, oaicaLocalTagSuffix)
}

// mergeLiveDaemonRows replaces the cached daemon rows with a live read of the
// daemon's list, keeping every other cached row and its order. A daemon that
// does not answer leaves the cached list untouched — the cache is still the
// better answer for everything else, and an unreachable daemon is not a reason
// to empty the menu.
func mergeLiveDaemonRows(ctx context.Context, client *api.Client, cached []LaunchModel) []LaunchModel {
	live, ok := daemonPickerRows(ctx, client)
	if !ok {
		return cached
	}
	out := make([]LaunchModel, 0, len(cached)+len(live))
	seen := make(map[string]bool, len(cached)+len(live))
	for _, m := range cached {
		if isDaemonRow(m.Name) || seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		out = append(out, m)
	}
	for _, m := range live {
		if seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		out = append(out, m)
	}
	return out
}

// pickerCacheTTL bounds how long the disk cache is trusted as fresh. One
// launch cycle per hour pays the full probe cost; everything in between
// opens the picker from the file. pickerCacheGrace is how long a STALE
// cache still paints the menu instantly (with a background refresh kicked
// off) before we give up and do the full load synchronously — a menu that
// is a few hours old beats no menu for several seconds.
const (
	pickerCacheTTL   = time.Hour
	pickerCacheGrace = 6 * time.Hour
)

type pickerCacheFile struct {
	SavedAt   time.Time     `json:"saved_at"`
	TTLSecond float64       `json:"ttl_seconds"`
	Models    []LaunchModel `json:"models"`
	// Inputs fingerprints the config files this list was derived from. A cache
	// is only a cache of a question already answered, and the question is
	// "what does THIS configuration offer" — so the answer is void the moment
	// the configuration changes. Without this, `oaica remote add` (or a
	// provider login, or a catalog sync) was invisible to the next launch for
	// up to pickerCacheTTL, and for up to pickerCacheGrace before anything
	// noticed: the user edits remotes.json, launches, and their brand-new
	// remote is simply not on the menu.
	Inputs map[string]string `json:"inputs,omitempty"`
}

// pickerCacheInputPaths are the files whose contents decide which rows the
// inventory contains. Paths come from the same helpers the loaders use, so an
// override (OAICA_REMOTES_FILE, OPENCODE_AUTH_FILE, OAICA_MODELS_DIR) is
// fingerprinted rather than bypassed.
//
// Each was added because a row appeared (or vanished) with no other file
// changing, so the cache kept painting the old menu for up to pickerCacheTTL:
//
//   - authStorePath/userRemotesPath/licenseFilePath/providerCatalogCachePath:
//     the original four (a login, a remote add, a licence, a catalog sync).
//   - localServersPath: `oaica serve <model>` writes it, and each live entry
//     becomes a "<model>:local" row.
//   - ollamaCloudCachePath: its ids become the "ollama/<id>" rows; the scrape
//     can learn about a new cloud model while the picker's own cache is still
//     fresh, and the two windows used to stack.
//   - the model manifest: `oaica model add`/`sync`/`scan` rows are inventory
//     rows.
//   - opencodeAuthPaths: the credential that GATES a whole provider's rows
//     can be written by `opencode auth login`, a file read through a helper
//     exactly like authStorePath() — which was fingerprinted.
func pickerCacheInputPaths() []string {
	out := make([]string, 0, 8)
	add := func(p string) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	add(userRemotesPath())
	add(authStorePath())
	if p, err := licenseFilePath(); err == nil {
		add(p)
	}
	if p, err := providerCatalogCachePath(); err == nil {
		add(p)
	}
	if p, err := oaicaLocalServersRegistryPath(); err == nil {
		add(p)
	}
	if p, err := ollamaCloudCachePath(); err == nil {
		add(p)
	}
	if p, err := modelManifestPath(); err == nil {
		add(p)
	}
	for _, p := range opencodeAuthPaths() {
		add(p)
	}
	return out
}

// pickerEnvFingerprint records the state that decides rows but lives in the
// environment rather than a file: which router OAICA_HOST points at, and which
// providers currently have a credential.
//
// Env state cannot be fingerprinted by path, so it is fingerprinted by VALUE —
// and only ever by a hash. OAICA_HOST can carry a key in its userinfo or query
// string (docs/ENTERPRISE.md's rule: a credential must not reach a cache), so
// the raw value never goes into the cache file; the digest answers the only
// question the fingerprint asks ("same host as last time?").
//
// Both halves are load-bearing. Without OAICA_HOST, repointing at a different
// router kept serving the old router's rows — the picker offered a model, and
// a launch POSTed it, to a host that had never heard of it. Without credential
// presence, `export Z_AI_API_KEY=…` in a new shell (or `opencode auth login
// <provider>`, whose store IS fingerprinted but whose effect here is the same)
// left the newly-credentialed provider's rows hidden for an hour — the exact
// "I logged in and the picker still shows nothing" complaint this cache was
// already fixed once for.
func pickerEnvFingerprint() map[string]string {
	out := map[string]string{}
	host := strings.TrimSpace(os.Getenv("OAICA_HOST"))
	if host != "" {
		sum := sha256.Sum256([]byte(host))
		out["env:OAICA_HOST"] = fmt.Sprintf("%x", sum[:8])
	} else {
		out["env:OAICA_HOST"] = "unset"
	}
	for _, e := range providerCatalog() {
		if e.APIKeyEnv == "" {
			continue
		}
		// The NAME of the variable that is set, not its value: a variable name
		// is not a secret, and the name is all the fingerprint needs.
		out["cred:"+e.Name] = keyEnvNameSet(e.APIKeyEnv)
	}
	return out
}

// pickerInputFingerprint records each input's identity by CONTENT, not by
// mtime+size. Same-size edits with a preserved mtime are not exotic — `cp -p`,
// `rsync -a`, a dotfiles manager, a backup restore, a tar extract, any
// filesystem with one-second timestamp granularity — and each of them left the
// picker painting the previous configuration's rows until the TTL lapsed.
// These are a handful of small config files, read once per cache write.
//
// A missing file is recorded as absent rather than skipped: a config file
// APPEARING is just as much a change as one being edited.
func pickerInputFingerprint() map[string]string {
	paths := pickerCacheInputPaths()
	if len(paths) == 0 {
		return nil
	}
	out := make(map[string]string, len(paths)+4)
	for _, p := range paths {
		out[p] = fileContentDigest(p)
	}
	for k, v := range pickerEnvFingerprint() {
		out[k] = v
	}
	return out
}

// fileContentDigest is the content identity of one input file: "absent", or
// "<size>:<sha256, truncated>". Truncated because the digest only has to
// distinguish configurations, and 16 hex chars of SHA-256 does that with no
// realistic collision risk while keeping the cache file readable.
func fileContentDigest(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "absent"
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%d:%x", len(b), sum[:8])
}

// pickerInputsUnchanged reports whether the cache's fingerprint still matches
// the files on disk. A cache with no fingerprint at all (written by an older
// build) is not trusted: it cannot say what it was built from.
func pickerInputsUnchanged(f pickerCacheFile) bool {
	now := pickerInputFingerprint()
	if len(f.Inputs) == 0 || len(now) != len(f.Inputs) {
		return false
	}
	for k, v := range f.Inputs {
		if now[k] != v {
			return false
		}
	}
	return true
}

// pickerCachePath lives under ~/.oaica next to plans.json/remotes.json.
func pickerCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "picker_cache.json"), nil
}

func savePickerCache(models []LaunchModel) {
	path, err := pickerCachePath()
	if err != nil || len(models) == 0 {
		return
	}
	b, err := json.Marshal(pickerCacheFile{SavedAt: time.Now(), TTLSecond: pickerCacheTTL.Seconds(), Models: models, Inputs: pickerInputFingerprint()})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	// Unique temp + rename: the swap is what keeps a crash from leaving a
	// half-written cache, and the unique name is what keeps two oaica
	// processes from writing through ONE temp buffer (2026-09-26 audit).
	_ = fileutil.WriteFileAtomic(path, b, 0o600)
}

func loadPickerCache() ([]LaunchModel, bool, bool) {
	path, err := pickerCachePath()
	if err != nil {
		return nil, false, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, false
	}
	var f pickerCacheFile
	if json.Unmarshal(b, &f) != nil || len(f.Models) == 0 {
		return nil, false, false
	}
	if !pickerInputsUnchanged(f) {
		// The configuration moved under the cache. Not merely stale: WRONG for
		// the question being asked, so it must not paint the menu at all —
		// falling through to the full load is what makes a just-added remote
		// visible on the very next launch.
		return nil, false, false
	}
	ttl := f.TTLSecond
	if ttl <= 0 {
		ttl = pickerCacheTTL.Seconds()
	}
	age := time.Since(f.SavedAt)
	if age < 0 {
		// Dated in the future: the clock was ahead when the cache was written
		// (a wrong RTC later corrected by NTP, a VM snapshot restored, a
		// dual-boot box). Age is negative, which passes BOTH the grace check
		// below and the freshness check after it, so the cache would be served
		// as fresh for as long as the clock was wrong — days — with no
		// background refresh ever kicked off (stale=false). The menu is frozen
		// and only deleting the file unfreezes it. A cache cannot be newer than
		// the process reading it, so treat it as not a cache at all.
		return nil, false, false
	}
	if age > pickerCacheGrace {
		return nil, false, false
	}
	return f.Models, age > time.Duration(ttl*float64(time.Second)), true
}

func (i *modelInventory) Resolve(ctx context.Context, names []string) []LaunchModel {
	names = dedupeModelList(names)
	if len(names) == 0 {
		return nil
	}

	// Fast path: every requested name is already an explicit "<remote>/<model>"
	// user-remote picker name. Load() probes ALL configured remotes (plus local
	// ollama and the cloud router) to build the full inventory — with many
	// remotes configured and some slow/unreachable, that adds several seconds
	// of dead-weight latency to every launch for a name we can already resolve
	// with zero network calls (findUserRemoteForModel does a config lookup
	// only; it never validates reachability, so Load() wouldn't have told us
	// anything more here anyway). Falls back to the full inventory the moment
	// any name ISN'T a user-remote picker name (local/cloud mixed in, etc.).
	if resolved, ok := resolveUserRemoteModelsDirect(names); ok {
		return resolved
	}

	models, err := i.Load(ctx)
	if err != nil {
		models = nil
	}

	resolved, localMiss := resolveLaunchModels(names, models)
	if localMiss {
		if refreshed, err := i.Refresh(ctx); err == nil {
			resolved, _ = resolveLaunchModels(names, refreshed)
		}
	}
	return resolved
}

// resolveUserRemoteModelsDirect resolves every name directly against
// ~/.oaica/remotes.json, with no network calls — mirrors exactly what
// userRemoteLaunchModels() (used inside Load()) would produce for a
// user-remote entry (same Descriptor()-derived fields, same WithCloudLimits()
// call), just without fetching every configured remote's /v1/models first.
// ok=false the instant one name isn't a user-remote picker name, so the
// caller falls back to the real (slower, complete) inventory.
func resolveUserRemoteModelsDirect(names []string) ([]LaunchModel, bool) {
	out := make([]LaunchModel, 0, len(names))
	for _, name := range names {
		remote, _, ok := findUserRemoteForModel(name)
		if !ok {
			return nil, false
		}
		d := remote.Descriptor()
		out = append(out, LaunchModel{
			Name:         name,
			Remote:       true,
			Wire:         d.Wire,
			ToolFormat:   d.ToolFormat,
			ToolReliable: d.ToolReliable,
		}.WithCloudLimits())
	}
	return out, true
}

// stripOllamaPickerNames removes the ollama/ picker prefix from selected
// names — display-level only, and a user remote literally named "ollama"
// keeps its namespace (its <remote>/<id> names win over our prefix).
func stripOllamaPickerNames(names []string) []string {
	for i, name := range names {
		rest, ok := strings.CutPrefix(name, ollamaPickerPrefix)
		if !ok {
			continue
		}
		if _, _, isRemote := findUserRemoteForModel(name); isRemote {
			continue
		}
		names[i] = rest
	}
	return names
}

func resolveLaunchModels(names []string, models []LaunchModel) ([]LaunchModel, bool) {
	resolved := make([]LaunchModel, 0, len(names))
	localMiss := false
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if model, ok := findLaunchModel(models, name); ok {
			// The ollama/ picker prefix is display-only, so a bare override
			// ("gemma4") and its prefixed inventory entry resolve to the
			// SAME bare name — drop the duplicate.
			if seen[model.Name] {
				continue
			}
			seen[model.Name] = true
			resolved = append(resolved, model.WithCloudLimits())
			continue
		}
		if !isCloudModelName(name) {
			localMiss = true
		}
		resolved = append(resolved, fallbackLaunchModel(name))
	}
	return resolved, localMiss
}

func launchModelFromListResponse(model api.ListModelResponse) LaunchModel {
	return LaunchModel{
		Name:            model.Name,
		Remote:          model.RemoteModel != "",
		ToolCapable:     slices.Contains(model.Capabilities, modelpkg.CapabilityTools),
		Capabilities:    append([]modelpkg.Capability(nil), model.Capabilities...),
		ContextLength:   model.Details.ContextLength,
		EmbeddingLength: model.Details.EmbeddingLength,
		Size:            model.Size,
		Details:         model.Details,
	}.WithCloudLimits()
}

func fallbackLaunchModel(name string) LaunchModel {
	return LaunchModel{Name: name, Remote: isCloudModelName(name)}.WithCloudLimits()
}

func findLaunchModel(models []LaunchModel, name string) (LaunchModel, bool) {
	for _, model := range models {
		if launchModelMatches(model.Name, name) {
			resolved := cloneLaunchModel(model)
			// The daemon (and every downstream caller) knows the bare id;
			// "ollama/<name>" is picker display only.
			resolved.Name = strings.TrimPrefix(resolved.Name, ollamaPickerPrefix)
			return resolved, true
		}
	}
	return LaunchModel{}, false
}

func launchModelMatches(candidate, name string) bool {
	if candidate == name {
		return true
	}
	// The ollama/ picker prefix is display-level: a bare saved name must
	// still resolve to the prefixed inventory entry, and vice versa.
	if rest, ok := strings.CutPrefix(candidate, ollamaPickerPrefix); ok && (rest == name || strings.TrimSuffix(rest, ":latest") == name) {
		return true
	}
	if rest, ok := strings.CutPrefix(name, ollamaPickerPrefix); ok && (candidate == rest || strings.TrimSuffix(candidate, ":latest") == rest) {
		return true
	}
	return strings.TrimSuffix(candidate, ":latest") == strings.TrimPrefix(strings.TrimSuffix(name, ":latest"), ollamaPickerPrefix)
}

func cloneLaunchModel(model LaunchModel) LaunchModel {
	model.Capabilities = append([]modelpkg.Capability(nil), model.Capabilities...)
	model.Details.Families = append([]string(nil), model.Details.Families...)
	return model
}

func cloneLaunchModels(models []LaunchModel) []LaunchModel {
	cloned := make([]LaunchModel, len(models))
	for i, model := range models {
		cloned[i] = cloneLaunchModel(model)
	}
	return cloned
}

func launchModelNames(models []LaunchModel) []string {
	names := make([]string, 0, len(models))
	for _, model := range models {
		if model.Name != "" {
			names = append(names, model.Name)
		}
	}
	return names
}

func launchModelsFromNames(names []string) []LaunchModel {
	models := make([]LaunchModel, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		models = append(models, fallbackLaunchModel(name))
	}
	return models
}
