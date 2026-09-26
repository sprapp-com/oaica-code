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
	// LiveSource names the RUNTIME source a row was built from, when that
	// source is process state rather than configuration: liveSourceDaemon (the
	// local Ollama daemon's /api/tags list) or liveSourceLocal (a running
	// `oaica serve`, from local_servers.json plus its own /health probe).
	// Empty = the row comes from a file or a fetched document.
	//
	// The cache-hit path re-derives every row carrying one of these and drops
	// the cached copy, which is what makes `ollama pull`/`ollama rm` and a
	// crashed `oaica serve` show up on the very next launch. Only the PRODUCER
	// can label a row this way: the name alone cannot. "ollama/<id>" is both a
	// daemon row (a bare local model, prefixed for the picker) and an
	// ollama-cloud catalogue row; "<model>:local" is not a daemon row at all.
	// A name-shaped predicate got both of those wrong in opposite directions
	// (2026-09-26 audit).
	LiveSource string `json:"live_source,omitempty"`
}

// The two live row families, as stamped on LaunchModel.LiveSource.
const (
	liveSourceDaemon = "daemon"
	liveSourceLocal  = "local"
)

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
			// Runtime state is what a cache cannot hold: the daemon's own model
			// list (`ollama pull`/`ollama rm`), and a `oaica serve` that has
			// started or died since the cache was written. Both are cheap to
			// re-derive — a loopback /api/tags and a loopback /health — so they
			// are re-derived here and the cached copies dropped, leaving the
			// expensive parts (the per-remote sweeps and the router) to the
			// cache. `oaica model refresh`'s help promises a pull is
			// discoverable on the next launch; a picked "<model>:local" row
			// whose server is gone is worse than a miss, since
			// oaicaResolveHostForModel falls through to the CLOUD host for a
			// tag no live server matches — the menu would route to a backend it
			// does not name.
			models = mergeLiveRows(ctx, i.client, models)
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

	// The fingerprint of the configuration this full load is about to read from,
	// taken BEFORE the first source is touched. It is what savePickerCache
	// stamps onto the rows below, and it must be the configuration they were
	// actually built from: reading it at save time instead (the shape before
	// this) recorded a mid-load edit — `oaica remote add` in another terminal, a
	// concurrent launch, any of the cross-process writers the cache documents —
	// as the provenance of rows that predate it. The cache then validated
	// against the NEW configuration while holding the OLD rows, so the next
	// launch trusted it and served a menu for a configuration it had never seen
	// (2026-09-26 audit). Taken early, that same edit simply voids the cache.
	inputs := pickerInputFingerprint()

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
		models = append(models, LaunchModel{
			Name:       e.ID,
			Remote:     true,
			Upstream:   e.Upstream,
			LiveSource: liveSourceForEntry(e.ID),
		}.WithCloudLimits())
	}

	i.models = models
	i.err = nil
	i.loaded = true
	savePickerCache(models, inputs)

	return cloneLaunchModels(i.models), i.err
}

// liveSourceForEntry reports which live family a router/user-remote/cloud
// entry belongs to, by the id the producer gave it: a "<model>:local" entry is
// the local-serve family (see oaicaLiveModelEntriesErr). Everything else is
// file- or document-backed and carries no live source.
func liveSourceForEntry(id string) string {
	if strings.HasSuffix(id, oaicaLocalTagSuffix) {
		return liveSourceLocal
	}
	return ""
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
		lm.LiveSource = liveSourceDaemon
		out = append(out, lm)
	}
	return out, true
}

// localServePickerRows lists the `oaica serve` instances running on this
// machine as their "<model>:local" rows — the same rows the full load merges
// out of oaicaLiveModelEntriesErr, derived the same way (the registry file plus
// each origin's /health probe) so a cache hit and a forced load agree.
//
// Skipped when OAICA_HOST is set, matching the full load: pinning to one host
// on purpose should not mix this machine's own servers into the menu.
func localServePickerRows() []LaunchModel {
	if strings.TrimSpace(os.Getenv("OAICA_HOST")) != "" {
		return nil
	}
	entries := oaicaLocalServerEntries()
	out := make([]LaunchModel, 0, len(entries))
	for _, e := range entries {
		if e.Model == "" {
			continue
		}
		out = append(out, LaunchModel{
			Name:       e.Model + oaicaLocalTagSuffix,
			Remote:     true,
			LiveSource: liveSourceLocal,
		}.WithCloudLimits())
	}
	return out
}

// mergeLiveRows re-derives the live row families on a cache hit and replaces
// their cached copies, keeping every other cached row and its order.
//
// What counts as live is the row's source, not its name (see
// LaunchModel.LiveSource): the daemon family is dropped only when the daemon
// actually answered — an unreachable daemon is not evidence that its models
// were removed, and emptying that part of the menu for a transient blip is
// worse than a stale row. The local-serve family is recomputed
// unconditionally: local_servers.json plus a /health probe is readable with no
// daemon involved at all, so a `oaica serve` that died since the cache was
// written disappears even when the daemon is unreachable.
func mergeLiveRows(ctx context.Context, client *api.Client, cached []LaunchModel) []LaunchModel {
	daemonRows, daemonOK := daemonPickerRows(ctx, client)
	localRows := localServePickerRows()

	out := make([]LaunchModel, 0, len(cached)+len(daemonRows)+len(localRows))
	seen := make(map[string]bool, len(cached)+len(daemonRows)+len(localRows))
	drop := func(lm LaunchModel) bool {
		switch lm.LiveSource {
		case liveSourceDaemon:
			return daemonOK // replaced by the live read below
		case liveSourceLocal:
			return true // always recomputed from the registry + /health
		}
		return false
	}
	for _, m := range cached {
		if seen[m.Name] || drop(m) {
			continue
		}
		seen[m.Name] = true
		out = append(out, m)
	}
	for _, live := range [][]LaunchModel{daemonRows, localRows} {
		for _, m := range live {
			if seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			out = append(out, m)
		}
	}

	// The ollama-cloud catalogue is file-backed, not live — but it is one of
	// the picker cache's own fingerprint inputs (ollama_cloud.json), and a
	// model the daemon stopped serving may still be offered there under the
	// same name. The FULL load adds the daemon's row before the catalogue's,
	// so the name cached for such a model was the daemon's copy: dropping it
	// above left the row missing for up to pickerCacheTTL, invisible to the
	// picker's own inputs and restored only by a forced load (`ollama rm
	// gpt-oss` → the `ollama/gpt-oss` row vanished for an hour). Re-derive
	// the catalogue rows here under the same duplicate rule the full load
	// applies (model_inventory.go's entry loop).
	for _, e := range ollamaCloudEntries() {
		if e.ID == "" || seen[e.ID] {
			continue
		}
		if up := ollamaCloudUpstreamFor(e.ID); up != "" && (seen[up] || seen[e.ID+":cloud"]) {
			continue
		}
		seen[e.ID] = true
		out = append(out, LaunchModel{
			Name:       e.ID,
			Remote:     true,
			Upstream:   e.Upstream,
			LiveSource: liveSourceForEntry(e.ID),
		}.WithCloudLimits())
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
//   - api_key: the ROUTER credential `oaica signin` writes. The router's
//     catalogue is per-account (a pro key lists rows a free key does not), so
//     re-signing in or rotating the key changes the answer while no other
//     fingerprinted file moves. Recorded by content digest like every other
//     input, never verbatim — docs/ENTERPRISE.md's rule that a credential must
//     not reach a cache.
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
	// The synced cloud-limits catalog: its numbers are the window a cloud
	// row is sized against (and the CLAUDE_CODE_* pair a launch exports), so
	// `oaica model cloud-limits sync` changes the answer while no other input
	// moves — the same reason providerCatalogCachePath is here.
	if p, err := cloudLimitsCatalogCachePath(); err == nil {
		add(p)
	}
	if p, err := oaicaLocalServersRegistryPath(); err == nil {
		add(p)
	}
	if p, err := ollamaCloudCachePath(); err == nil {
		add(p)
	}
	if p, err := oaicaLaunchSavedAPIKeyPath(); err == nil {
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
	// The ROUTER credential itself, however it is spelled (OAICA_API_KEY, the
	// ~/.oaica/api_key file `oaica signin` writes, or OAICA_HOST's userinfo —
	// oaicaLaunchAPIKeyForEnv resolves all three). Unlike a provider key, its
	// identity and not just its presence decides the answer: the router's
	// catalogue is per-account, so a re-signed-in or rotated key turns a
	// working picker into an empty one until the TTL lapses (2026-09-26
	// audit). Hashed, never raw — same rule as OAICA_HOST above.
	if key := oaicaLaunchAPIKeyForEnv(); key != "" {
		sum := sha256.Sum256([]byte(key))
		out["cred:OAICA_API_KEY"] = fmt.Sprintf("%x", sum[:8])
	} else {
		out["cred:OAICA_API_KEY"] = "unset"
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

// savePickerCache writes the rows and the configuration they were BUILT FROM.
// inputs is a parameter, not computed here, and that is the whole point: it is
// taken before the load reads its sources, so a config edit made while the
// load was probing (a `oaica remote add` in another terminal, a concurrent
// launch) cannot be recorded as this file's provenance. Stamping the
// post-edit fingerprint onto pre-edit rows made the cache claim an answer it
// never had, which is the round-2 "the remote I just added is not on the menu"
// defect through a race window (2026-09-26 audit).
func savePickerCache(models []LaunchModel, inputs map[string]string) {
	path, err := pickerCachePath()
	if err != nil || len(models) == 0 {
		return
	}
	b, err := json.Marshal(pickerCacheFile{SavedAt: time.Now(), TTLSecond: pickerCacheTTL.Seconds(), Models: models, Inputs: inputs})
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

// launchNameForPickerName maps a picker selection to the name the launch path
// must carry. Stripping the display prefix is right for a daemon row ("ollama/
// gpt-oss" is a local model the daemon knows by its bare id), but NOT for an
// ollama-cloud catalogue row: that row is named "ollama/gpt-oss" and documents
// the daemon-side name "gpt-oss:cloud" in LaunchModel.Upstream, and sending the
// stripped bare id asked the daemon for a LOCAL model instead — failing "not
// pulled on the local daemon" (with a multi-GB pull offer) or silently running
// a local model of the same name, while the cloud alias right there in the row
// launched fine (2026-09-26 audit). Only the ROW can say which it is, so the
// row decides; a user remote literally named "ollama" keeps its namespace.
func launchNameForPickerName(models []LaunchModel, name string) string {
	if _, _, isRemote := findUserRemoteForModel(name); isRemote {
		return name
	}
	if row, ok := findLaunchModel(models, name); ok && row.Upstream != "" {
		return row.Upstream
	}
	return stripOllamaPickerNames([]string{name})[0]
}

// launchNamesForPickerSelections applies launchNameForPickerName to a whole
// selection, reading the inventory the picker was built from. The inventory is
// already loaded (and cached) by the time a selection is made, so this costs
// no probe; if it cannot be read, the lexical strip is the fallback.
func (c *launcherClient) launchNamesForPickerSelections(ctx context.Context, names []string) []string {
	models, err := c.modelInventory().Load(ctx)
	if err != nil || len(models) == 0 {
		return stripOllamaPickerNames(names)
	}
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = launchNameForPickerName(models, name)
	}
	return out
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
	// The alias hop runs FIRST, and that order is the fix. A row that merely
	// shares the alias's own spelling is not what the alias means: the daemon
	// serving a model called `gemma3` does not make a user's alias `gemma3`
	// (pointing at box's `gemma3-ft`) that model. Matched exactly first, the
	// collision won — the REFUSALS were handed the daemon row and permitted the
	// launch — while the WRITE path resolved the alias (childModelIDFor), so the
	// store got the box's model id beside the daemon's base URL and wire shape:
	// exactly the mixed identity the hop exists to stop, one name collision away
	// (2026-09-27 audit, round 25). model_alias.go states the rule this now
	// follows: "an alias always wins if defined ... not a different thing that
	// happens to share the bare id".
	//
	// A user alias is transparent, and the row that serves its target is the row
	// that serves the alias. Without this hop every caller saw a name no row
	// carries: the writers fell back to the picker spelling (round 23), and the
	// REFUSALS — which ask this function whether the row is one the local daemon
	// serves — found nothing and returned nil, so an alias pointing at another
	// endpoint bypassed the guard that exists to refuse exactly that. The ChatGPT
	// app was written the remote's model id beside the daemon's base URL
	// (2026-09-27 audit, round 24, both auditors).
	if target, ok := resolveModelAlias(name); ok {
		target = strings.TrimSpace(target)
		if target != "" && target != name {
			if model, ok := findLaunchModelExact(models, target); ok {
				return model, true
			}
			// The alias's target is not in the inventory. Fall through to the
			// alias's own spelling: it is the only row this launch has.
		}
	}
	return findLaunchModelExact(models, name)
}

// findLaunchModelExact is findLaunchModel without the alias hop: the row whose
// own name is this one.
func findLaunchModelExact(models []LaunchModel, name string) (LaunchModel, bool) {
	for _, model := range models {
		if launchModelMatches(model.Name, name) {
			resolved := cloneLaunchModel(model)
			// The daemon (and every downstream caller) knows the bare id;
			// "ollama/<name>" is picker display only — for a DAEMON row. A user
			// remote literally named "ollama" produces the same shape, and its
			// namespace IS its identity: stripping it left a name no remote
			// lookup could claim (nothing contains "/" any more), so the row
			// fell through to "the daemon" and a store that dials the daemon was
			// written with a model only the remote serves. Same rule as
			// stripOllamaPickerNames and launchNameForPickerName
			// (2026-09-27 audit, round 21).
			if _, _, isRemote := findUserRemoteForModel(resolved.Name); !isRemote {
				resolved.Name = strings.TrimPrefix(resolved.Name, ollamaPickerPrefix)
			}
			return resolved, true
		}
	}
	return LaunchModel{}, false
}

// displayBareName folds the display-only spellings of the LOCAL daemon's ids
// into the bare id: "ollama/<id>" (the picker's display prefix) and
// "daemon/<id>" (resolveLaunchEndpoint's daemon pin) both name the daemon's
// "<id>", and the writers store the bare id, so a launch spelled with the
// prefix read back as drift and reconfigured the integration on every run
// (2026-09-27 audit, round 24).
//
// Two exclusions hold the line findLaunchModel drew in round 21:
//
//   - a user remote literally named "ollama" (or "daemon") owns that namespace
//     — its namespace IS its identity, and resolveRemoteEndpoint answers for
//     it, so the name is returned untouched;
//   - router/ and oaica/ are NOT folded: they pin the router, which is a
//     different endpoint than the daemon, and this comparison has no way to
//     name that difference. Left unfolded, such a pair reads as drift and the
//     integration reconfigures — noisy but never silently pointed elsewhere.
func displayBareName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	rest, ok := "", false
	for _, p := range []string{ollamaPickerPrefix, "daemon/"} {
		if r, cut := strings.CutPrefix(name, p); cut {
			rest, ok = r, true
			break
		}
	}
	if !ok || rest == "" {
		return name
	}
	if _, isRemote := resolveRemoteEndpoint(name); isRemote {
		return name
	}
	return rest
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

// launchModelWriteIDs is launchModelNames' counterpart in the vocabulary the
// integrations' stores hold: the id each row is written AS (launchModelWriteID
// — the backend id when the row's Name is a display label). The two spellings
// are not interchangeable, and a comparison that reads one against the other
// answers "different" for a row that did not change (2026-09-27 audit, round
// 26).
func launchModelWriteIDs(models []LaunchModel) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if id := launchModelWriteID(model); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// selectionRows is a selection of NAMES as the ROWS the picker was built from:
// the inventory row for each name (findLaunchModel's own resolution, alias hop
// included), or a bare row carrying the name itself when the inventory has no
// such row — the shape a store holds for a model it resolves directly. The
// order of the names is preserved, and names that resolve to ONE row contribute
// that row once, which is what the writer keeps (see the dedupe below).
//
// It replaces the ids-only form that stood here. An empty name cannot be an id,
// so that form DROPPED it and returned a shorter list, which a store holding
// the very rows the launch would write then read as drift; and an id is not
// enough to name a row anyway — the provider block or the endpoint recorded
// beside it is half the identity, and only a ROW carries that far enough for
// the editor to apply its own rule (2026-09-27 audit, round 27).
func selectionRows(inventory []LaunchModel, names []string) []LaunchModel {
	rows := make([]LaunchModel, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		row, ok := findLaunchModel(inventory, name)
		if !ok {
			row = LaunchModel{Name: name}
		}
		// Two spellings of one model — the prefixed picker name beside the bare
		// override — resolve to the SAME row, and the writer they are compared
		// against keeps one entry per resolved row (resolveLaunchModels' own
		// seen-by-name rule). Asked about both, a store holding the one entry
		// read as drift forever and every launch took the configure path for a
		// configuration it had already written (2026-09-27 audit, round 29,
		// A-F4).
		if seen[row.Name] {
			continue
		}
		seen[row.Name] = true
		rows = append(rows, row)
	}
	return rows
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

// launchModelEndpointKey names the endpoint a picker row is routed to. Stores
// that can express exactly ONE endpoint — one base URL, one wire, one
// credential — need this to tell which rows belong in that store at all.
//
// The Remote flag is not the endpoint: an ollama-cloud row ("glm-5.1:cloud")
// carries Remote=true and is still served BY the local daemon, which proxies
// it. A `oaica serve` row ("<model>:local") is reached at its own origin, which
// no single-endpoint store can name from the row alone, so it is its own key.
func launchModelEndpointKey(model LaunchModel) string {
	if model.LiveSource == liveSourceLocal {
		return "serve:" + model.Name
	}
	// The ROW's daemon-side identity, before the name: an ollama-cloud
	// catalogue row is named bare by the time it reaches a writer
	// (findLaunchModel strips the "ollama/" picker prefix) while the daemon
	// knows it as "<id>:cloud" (LaunchModel.Upstream). Resolving that bare name
	// let an unrelated remote serving the same bare id claim a row the daemon
	// serves — the ids collide by design, the shipped catalogues and ollama.com
	// use the same names — and the refusals then rejected the cloud model with a
	// reason that was false for it (2026-09-27 audit, round 21).
	if isCloudModelName(model.Upstream) {
		return "daemon"
	}
	// The remote arm runs before the name-shaped cloud check, because the ROUTER
	// resolves user remotes first (resolveLaunchEndpoint): a remote's own model
	// named "<id>:cloud" — an ollama box with a cloud model pulled serves exactly
	// those ids — was keyed to the daemon, so the refusals stayed silent and the
	// store was written with a namespaced name the daemon does not serve. Only
	// the Upstream arm above outranks the remote, because it is the picker row's
	// identity rather than a spelling (2026-09-27 audit, round 22).
	if ep, ok := resolveRemoteEndpoint(model.Name); ok {
		return "remote:" + strings.TrimRight(ep.BaseURL, "/")
	}
	// The ROUTER's own namespace, keyed as its own endpoint rather than the
	// daemon's. A router SKU is not a model the local daemon has — posting its
	// id to the daemon does not resolve there — but nothing in the name says so
	// to this function, which keyed it "daemon" and therefore told the
	// single-endpoint writers (the ChatGPT app, the DeepSeek Harness, muse)
	// that it was a row they could write: each names one base URL, the daemon's,
	// so the router's own id went into the store beside the daemon's endpoint
	// and the launch reported success for a model the endpoint does not serve
	// (2026-09-27 audit, round 24, both auditors). Placed after the remote arm
	// for the same reason resolveLaunchEndpoint places its own prefix handling
	// there: a user remote named "oaica" owns its namespace.
	if routerPinnedRow(model) {
		return "router"
	}
	if isCloudModelName(model.Name) {
		return "daemon"
	}
	return "daemon"
}

// routerPinnedRow is routerPinnedName asked of a ROW rather than a name, and a
// row's provenance outranks its spelling.
//
// LiveSource is the writer that produced the row: liveSourceDaemon means the
// local daemon's own /api/tags listed this model. The daemon listing `oaica-…`
// IS the daemon having it, so the name-shaped pin cannot apply — a user who
// pulled a model literally named `oaica-small-7b` had it refused as a router
// SKU, with a message saying the local daemon does not have it at all, while
// resolveLaunchEndpoint resolves that very name to the daemon and the first
// inference would have worked. The pin stays exactly where the name is the only
// evidence — the router's own catalogue rows, which no writer labels
// (2026-09-27 audit, round 25).
func routerPinnedRow(model LaunchModel) bool {
	if model.LiveSource == liveSourceDaemon {
		return false
	}
	return routerPinnedName(model.Name)
}

// routerPinnedName reports whether a name pins the OAICA router: the explicit
// source prefixes resolveLaunchEndpoint's router arm understands ("router/",
// "oaica/"), or the router's own bare "oaica-<sku>" ids.
//
// The bare form is how the router's SKUs reach every list that carries them —
// the picker's "OAICA Models" rows and the wizard's recommended section keep
// them unprefixed (tier_wizard's tierItemName leaves "oaica-*" as-is, and its
// pinned section selects exactly this prefix as "actual router SKUs"), so the
// name is the only thing there is to go on. A user remote is consulted first
// and wins: its namespace is its own, and `oaica/kat` naming a remote called
// "oaica" is that remote's model.
func routerPinnedName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if _, ok := resolveRemoteEndpoint(name); ok {
		return false
	}
	return strings.HasPrefix(name, "router/") ||
		strings.HasPrefix(name, "oaica/") ||
		strings.HasPrefix(name, "oaica-")
}

// daemonRoutedModel reports whether a picker row is one the LOCAL DAEMON
// serves — a local model, or an ollama-cloud model the daemon proxies.
//
// This is the predicate the single-endpoint stores need, not LaunchModel.Remote:
// the stores that name one endpoint and one credential (the DeepSeek Harness,
// the ChatGPT app, muse) all name that daemon, so the rows they cannot express
// are the ones routed elsewhere. Remote is a superset — it is set for a cloud
// row the daemon serves just as much as for a user remote at its own origin —
// and keying a refusal on it rejected a cloud model with a message telling the
// user to "launch a daemon-backed model", which is what they had done
// (2026-09-27 audit, round 20).
func daemonRoutedModel(model LaunchModel) bool {
	return launchModelEndpointKey(model) == "daemon"
}

// nonDaemonRowReason names, for those refusals, why the row cannot be written:
// where it is actually reached, and what the store would do with it instead.
func nonDaemonRowReason(model LaunchModel) string {
	if model.LiveSource == liveSourceLocal {
		return "the model is served by `oaica serve` at its own origin, and its \"<model>:local\" name posted to the local daemon does not resolve there"
	}
	if routerPinnedRow(model) {
		return "the model is served by the OAICA router at its own origin with your OAICA credential, and the local daemon does not have it at all"
	}
	return "the model is reached at its own origin, and its namespaced name posted to the local daemon does not resolve there"
}

// rejectServedModels refuses an `oaica serve` row for an integration launch.
//
// A "<model>:local" row is a model THIS MACHINE serves from its own process, on
// its own origin, behind its own credential. It is not a model the local daemon
// has (posting the tagged name to the daemon does not resolve there) and not a
// user remote either. Every integration writer resolves a row as
// "user remote, else the daemon" (resolveRemoteEndpoint), and the daemon is the
// fallback for every family it does not know — so such a row was written as a
// DAEMON endpoint under the row's "<model>:local" id, the launch reported
// success, and the first inference 404'd. Refusing says so instead, and names
// the path that does work: the model on its own, which routes it through its
// own origin (resolveLaunchEndpoint's local-serve arm). Teaching the writers
// the local-serve shape is the follow-up; it is one endpoint more per foreign
// config format, and a silent wrong endpoint is the worse failure.
//
// The row also became reachable only in round 20, when hasLocalModel stopped
// skipping the local-serve family — before that the picker reopened on it.
func rejectServedModels(integration string, models []LaunchModel) error {
	for _, model := range models {
		if model.LiveSource != liveSourceLocal {
			continue
		}
		return fmt.Errorf("%s cannot be pointed at %q: the model is served by `oaica serve` on its own origin with its own credential, and %s's configuration names the local daemon for every row it writes — the daemon does not serve this model. Launch it on its own (`oaica launch --model %s`) to route it through its own origin, or add it to %s's own settings", integration, model.Name, integration, model.Name, integration)
	}
	return nil
}

// rejectUnexpressibleModels refuses, for a STORE WRITER, every row the store
// cannot name: the rows whose endpoint is neither the local daemon nor a
// configured user remote.
//
// Every writer behind prepareEditorIntegration and
// prepareManagedSingleIntegration resolves a row as "user remote, else the
// daemon" (resolveRemoteEndpoint and its row-aware form), and the daemon is the
// fallback for anything that resolver does not claim. A row served by `oaica
// serve` at its own origin and a row served by the OAICA router at ITS own
// origin — with the OAICA credential, and not present on the daemon at all —
// were both written as daemon models: the store then names an endpoint that has
// never heard of the model, the launch reports success, and the first inference
// 404s. rejectServedModels caught the first family and this asked about
// LiveSource alone, so `oaica-<sku>` and `router/<sku>` rows went through the
// same writers unnoticed (2026-09-27 audit, round 25).
//
// The row's own endpoint key decides, not a name shape: a model the daemon
// genuinely serves under a router-shaped name keys "daemon" (routerPinnedRow)
// and is written as before. The launch path's own refusal (launch.go) stays
// narrower on purpose — a Runner may reach its model through a translation
// proxy that does carry these families (`oaica launch claude` does) — so this
// belongs at the store writers, where the endpoint is written down.
func rejectUnexpressibleModels(integration string, models []LaunchModel) error {
	for _, model := range models {
		if model.Name == "" {
			continue
		}
		switch key := launchModelEndpointKey(model); {
		case key == "daemon", strings.HasPrefix(key, "remote:"):
			continue
		}
		return fmt.Errorf("%s cannot be pointed at %q: %s, and %s's configuration names the local daemon for every row it writes — the daemon does not serve this model. Launch it on its own (`oaica launch --model %s`) to route it through its own origin, or add it to %s's own settings", integration, model.Name, nonDaemonRowReason(model), integration, model.Name, integration)
	}
	return nil
}

// singleEndpointModels keeps the rows a store that names ONE endpoint can
// serve: the ones routed to the same endpoint as the primary, which is the row
// that decides that store's base URL, wire and credential.
//
// The list these writers are handed is not the user's selection:
// managedSingleConfigureModels hands the launch target plus every row the
// picker offers, with the remote rows flagged. Writing the rest of the menu
// into the store advertises models the configured endpoint does not serve, so
// they are offered, selected, and posted to the wrong host — for the ChatGPT
// app's catalogue, the harness settings and OMP's models.yml alike. Refusing
// the launch instead was worse: one row in the menu that the user never picked
// refused an ordinary local launch (2026-09-27 audit, round 19). The primary is
// what each store checks for a selection it cannot express at all; everything
// else is filtered to its endpoint here.
func singleEndpointModels(primary string, models []LaunchModel) []LaunchModel {
	primaryRow, ok := findLaunchModel(models, primary)
	if !ok {
		primaryRow = fallbackLaunchModel(primary)
	}
	want := launchModelEndpointKey(primaryRow)

	servable := make([]LaunchModel, 0, len(models))
	for _, model := range models {
		if model.Name == "" || launchModelEndpointKey(model) != want {
			continue
		}
		servable = append(servable, model)
	}
	return servable
}
