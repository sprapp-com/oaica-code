package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/api"
	modelpkg "github.com/ollama/ollama/types/model"
)

func TestModelInventoryResolveRefreshesLocalMiss(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		calls++
		if calls == 1 {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		fmt.Fprint(w, `{"models":[{"name":"new-model","size":123,"details":{"context_length":65536,"embedding_length":1024},"capabilities":["vision","tools"]}]}`)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	inventory := newModelInventory(api.NewClient(u, srv.Client()))

	got := inventory.Resolve(context.Background(), []string{"new-model"})
	if calls != 2 {
		t.Fatalf("List calls = %d, want 2", calls)
	}
	if len(got) != 1 {
		t.Fatalf("Resolve returned %d models, want 1", len(got))
	}
	if got[0].Name != "new-model" {
		t.Fatalf("Name = %q, want new-model", got[0].Name)
	}
	if got[0].ContextLength != 65_536 || got[0].EmbeddingLength != 1_024 {
		t.Fatalf("metadata = context %d embedding %d, want refreshed metadata", got[0].ContextLength, got[0].EmbeddingLength)
	}
	if !got[0].HasCapability(modelpkg.CapabilityVision) || !got[0].ToolCapable {
		t.Fatalf("capabilities = %v toolCapable=%v, want refreshed capabilities", got[0].Capabilities, got[0].ToolCapable)
	}
}

func TestModelInventoryResolveDoesNotRefreshCloudMiss(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		calls++
		fmt.Fprint(w, `{"models":[]}`)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	inventory := newModelInventory(api.NewClient(u, srv.Client()))

	got := inventory.Resolve(context.Background(), []string{"glm-5.1:cloud"})
	if calls != 1 {
		t.Fatalf("List calls = %d, want 1", calls)
	}
	if len(got) != 1 {
		t.Fatalf("Resolve returned %d models, want 1", len(got))
	}
	if got[0].Name != "glm-5.1:cloud" || !got[0].Remote {
		t.Fatalf("resolved model = %#v, want cloud fallback", got[0])
	}
	if got[0].ContextLength <= 0 || got[0].MaxOutputTokens <= 0 {
		t.Fatalf("cloud limits not applied: %#v", got[0])
	}
}

func TestPickerCacheRoundTripAndTTL(t *testing.T) {
	withTempOaicaHome(t)
	models := []LaunchModel{{Name: "oaica-35b-a3b-vision", Remote: true}, {Name: "ollama/kat-awq"}}
	savePickerCache(models, pickerInputFingerprint())
	got, stale, ok := loadPickerCache()
	if !ok || stale || len(got) != 2 || got[0].Name != "oaica-35b-a3b-vision" {
		t.Fatalf("round trip: ok=%v stale=%v models=%v", ok, stale, got)
	}
	// Stale cache within the grace window: loads, marked stale (a background
	// refresh will run), NOT a miss. Writing the file by hand means writing the
	// input fingerprint too — a cache with none is a miss by design (see
	// TestPickerCacheWithoutAFingerprintIsNotTrusted).
	b, _ := json.Marshal(pickerCacheFile{SavedAt: time.Now().Add(-2 * time.Hour), TTLSecond: time.Hour.Seconds(), Models: models, Inputs: pickerInputFingerprint()})
	path, _ := pickerCachePath()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, stale, ok = loadPickerCache()
	if !ok || !stale || len(got) != 2 {
		t.Fatalf("grace-window cache: ok=%v stale=%v models=%v", ok, stale, got)
	}
	// Beyond grace: full miss.
	b, _ = json.Marshal(pickerCacheFile{SavedAt: time.Now().Add(-7 * time.Hour), TTLSecond: time.Hour.Seconds(), Models: models, Inputs: pickerInputFingerprint()})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := loadPickerCache(); ok {
		t.Fatal("beyond-grace cache must not load")
	}
}

// The picker cache is a cache of a QUESTION — "what does this configuration
// offer" — so an edit to the configuration voids the answer, not merely ages
// it. Without that, `oaica remote add box` followed by `oaica launch` showed a
// menu with no rows for the new box for up to an hour, which is exactly what
// the shipped help text says cannot happen ("there is no cross-process cache,
// so a plain 'ollama pull' or a remotes.json edit is visible on the very next
// launch with no action needed").
func TestPickerCacheIsVoidedByARemotesEdit(t *testing.T) {
	withTempOaicaHome(t)
	writeRemotes(t, `{"remotes":[]}`)
	stubUserRemoteModels(t, nil, nil)

	// A cache written by a launch a moment ago: the config as it was, which
	// knows nothing of the box about to be added.
	savePickerCache([]LaunchModel{{Name: "old/model", Remote: true}}, pickerInputFingerprint())
	if _, stale, ok := loadPickerCache(); !ok || stale {
		t.Fatalf("premise broken: a fresh cache must load (ok=%v stale=%v)", ok, stale)
	}

	// The user adds a remote — a change to a file the cache was derived from.
	writeRemotes(t, `{"remotes":[{"name":"mybox","base_url":"http://127.0.0.1:9/v1","api_key_env":"MYBOX_KEY"}]}`)
	stubUserRemoteModels(t, []LaunchModel{{Name: "mybox/kat-awq", Remote: true}}, nil)

	models, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var names []string
	for _, m := range models {
		names = append(names, m.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "mybox/kat-awq") {
		t.Fatalf("picker rows = %v — the just-added remote is missing from the very next launch", names)
	}
}

// A cache written by an older build carries no fingerprint, and so cannot say
// what it was built from. It must be treated as a miss rather than trusted:
// "I do not know" is not "nothing changed".
func TestPickerCacheWithoutAFingerprintIsNotTrusted(t *testing.T) {
	withTempOaicaHome(t)
	path, err := pickerCachePath()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(pickerCacheFile{
		SavedAt: time.Now(), TTLSecond: time.Hour.Seconds(),
		Models: []LaunchModel{{Name: "old/model", Remote: true}},
	})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := loadPickerCache(); ok {
		t.Fatal("a fingerprint-less cache loaded — it cannot be known to match the current configuration")
	}
}

// ---------------------------------------------------------------------------
// The picker's cross-process cache (~/.oaica/picker_cache.json) is an
// optimization with a promise attached: cmd/cmd.go's `model refresh` help text
// says an edited remotes.json, a provider login, a catalog sync, a model
// add/scan, a local `oaica serve`, or a changed OAICA_HOST voids it, so the next
// launch sees the new state. These tests pin that promise — one per way the
// rows can change while the process that built the cache is gone.

// invNames/invHas are the two assertions every test below makes about rows.
func invNames(models []LaunchModel) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.Name)
	}
	return out
}

func invHas(models []LaunchModel, name string) bool {
	for _, m := range models {
		if m.Name == name {
			return true
		}
	}
	return false
}

func invPickerCachePath(t *testing.T) string {
	t.Helper()
	p, err := pickerCachePath()
	if err != nil {
		t.Fatalf("pickerCachePath: %v", err)
	}
	return p
}

// TestModelInventoryPulledModelAppearsOnTheNextLoad: a daemon's model list is
// runtime state with no file behind it, so the fingerprint cannot see it. The
// cache-hit path therefore re-reads the daemon and merges its live rows in,
// instead of trusting the cached list for the rest of the hour — otherwise a
// just-pulled model stays invisible (and `ollama rm` keeps offering a model
// that is gone) while the help text says neither can happen.
func TestModelInventoryPulledModelAppearsOnTheNextLoad(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "http://127.0.0.1:1") // pinned: no local-server/ollama-cloud merge
	stubCloudFetch(t, []oaicaModelEntry{{ID: "cloud-a", Description: "router row"}}, nil)
	stubUserRemoteModels(t, nil, nil)
	writeRemotes(t, `{"remotes":[]}`)

	var daemonModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		parts := make([]string, 0, len(daemonModels))
		for _, n := range daemonModels {
			parts = append(parts, fmt.Sprintf(`{"name":%q}`, n))
		}
		fmt.Fprintf(w, `{"models":[%s]}`, strings.Join(parts, ","))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	client := api.NewClient(u, srv.Client())

	daemonModels = []string{"kat-awq"}
	first, err := newModelInventory(client).Load(context.Background())
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if !invHas(first, ollamaPickerPrefix+"kat-awq") {
		t.Fatalf("first Load = %v, want the daemon's own model", invNames(first))
	}
	if _, statErr := os.Stat(invPickerCachePath(t)); statErr != nil {
		t.Fatalf("no cross-process cache was written by that Load: %v", statErr)
	}

	// The user pulls a new model. Nothing under ~/.oaica changes — runtime
	// state, not configuration — so the fingerprint still matches.
	daemonModels = []string{"kat-awq", "brand-new-model"}

	second, err := newModelInventory(client).Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if !invHas(second, ollamaPickerPrefix+"brand-new-model") {
		t.Errorf("after `ollama pull brand-new-model` the picker shows %v — the pulled model is missing, "+
			"although the help text promises a pull is visible on the very next launch", invNames(second))
	}
}

// TestModelInventoryLocalServeAppearsOnTheNextLoad: starting `oaica serve` adds
// rows from ~/.oaica/local_servers.json, which is now fingerprinted like the
// other configuration files.
func TestModelInventoryLocalServeAppearsOnTheNextLoad(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "") // OAICA_HOST pinned skips the :local merge entirely
	stubCloudFetch(t, []oaicaModelEntry{{ID: "cloud-a"}}, nil)
	stubUserRemoteModels(t, nil, nil)
	writeRemotes(t, `{"remotes":[]}`)

	first, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("first Load produced no rows (no cache would be written)")
	}

	// The user starts `oaica serve kat-awq`: the health probe must answer 200.
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer health.Close()
	home, _ := os.UserHomeDir()
	reg := fmt.Sprintf(`[{"model":"kat-awq","origin":%q,"pid":1,"started_at":"now"}]`, health.URL)
	if err := os.WriteFile(filepath.Join(home, ".oaica", "local_servers.json"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if !invHas(second, "kat-awq"+oaicaLocalTagSuffix) {
		forced, ferr := newModelInventory(nil).Refresh(context.Background())
		t.Errorf("after `oaica serve kat-awq` the picker shows %v — the local server's rows are missing, "+
			"because local_servers.json is not fingerprinted. A forced load returns %v (err=%v).",
			invNames(second), invNames(forced), ferr)
	}
}

// TestModelInventoryRouterHostChangeVoidsTheCache: OAICA_HOST selects the
// catalog the rows come from. It is process env, so the fingerprint carries an
// env half — without it a cache built against host A keeps being served after
// the user repoints at host B, offering rows (and a host) B does not serve.
func TestModelInventoryRouterHostChangeVoidsTheCache(t *testing.T) {
	withTempOaicaHome(t)
	stubUserRemoteModels(t, nil, nil)
	writeRemotes(t, `{"remotes":[]}`)

	newRouter := func(model string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, model)
		}))
	}
	hostA := newRouter("host-one-model")
	defer hostA.Close()
	hostB := newRouter("host-two-model")
	defer hostB.Close()

	t.Setenv("OAICA_HOST", hostA.URL)
	first, err := newModelInventory(nil).Load(context.Background())
	if err != nil || !invHas(first, "host-one-model") {
		t.Fatalf("Load against host A = %v, %v", invNames(first), err)
	}

	t.Setenv("OAICA_HOST", hostB.URL)
	second, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if invHas(second, "host-one-model") || !invHas(second, "host-two-model") {
		t.Errorf("after repointing OAICA_HOST at %s the picker shows %v — rows from the OLD router are served, "+
			"because OAICA_HOST is not part of the cache's fingerprint", hostB.URL, invNames(second))
	}
}

// invSyncedCatalogWithGatedBox writes a synced overlay row gated on
// AUDITBOX_KEY via the opencode auth store, pointing at a dead port so its rows
// come from the declared list with no network sweep.
func invSyncedCatalogWithGatedBox(t *testing.T) {
	t.Helper()
	path, err := oaicaOverlayCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := `{"version":0,"providers":[{"name":"auditbox","base_url":"http://127.0.0.1:9/v1",` +
		`"api_key_env":"AUDITBOX_KEY","auth_via":"opencode","models":{"m1":{"context":1000,"output":100}}}]}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// invLoadAfterCredentialAppears pins the two ways a credential can appear
// between launches — an exported key, and the external auth store a provider
// reads through — both of which gate whole providers' rows.
func invLoadAfterCredentialAppears(t *testing.T, grant func(t *testing.T)) {
	t.Helper()
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "http://127.0.0.1:1")
	t.Setenv("AUDITBOX_KEY", "")
	ocAuth := filepath.Join(t.TempDir(), "opencode-auth.json")
	t.Setenv("OPENCODE_AUTH_FILE", ocAuth) // does not exist yet
	stubCloudFetch(t, []oaicaModelEntry{{ID: "cloud-a"}}, nil)
	writeRemotes(t, `{"remotes":[]}`)
	invSyncedCatalogWithGatedBox(t)

	first, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if invHas(first, "auditbox/m1") {
		t.Fatalf("auditbox rows present before any credential: %v", invNames(first))
	}

	grant(t)

	second, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if !invHas(second, "auditbox/m1") {
		forced, ferr := newModelInventory(nil).Refresh(context.Background())
		t.Errorf("a new credential is not reflected in the picker: rows = %v (auditbox/m1 missing). "+
			"Credential presence gates the provider but is not part of the fingerprint. A forced load returns %v (err=%v).",
			invNames(second), invNames(forced), ferr)
	}
}

func TestModelInventoryExportedProviderKeyVoidsTheCache(t *testing.T) {
	invLoadAfterCredentialAppears(t, func(t *testing.T) {
		t.Setenv("AUDITBOX_KEY", "sk-live")
	})
}

func TestModelInventoryExternalAuthLoginVoidsTheCache(t *testing.T) {
	invLoadAfterCredentialAppears(t, func(t *testing.T) {
		p := os.Getenv("OPENCODE_AUTH_FILE")
		if err := os.WriteFile(p, []byte(`{"auditbox":{"type":"api","key":"sk-live"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	})
}

// TestModelInventoryFutureDatedCacheIsNotTrusted: age = time.Since(SavedAt) is
// negative when the clock was ahead as the cache was written (a wrong RTC
// corrected by NTP, a VM snapshot, a dual-boot box). A negative age passed both
// the TTL and the grace check and never triggered the background refresh, so
// the menu was frozen until someone deleted the file.
func TestModelInventoryFutureDatedCacheIsNotTrusted(t *testing.T) {
	withTempOaicaHome(t)
	path := invPickerCachePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	future := 72 * time.Hour
	b, _ := json.Marshal(pickerCacheFile{
		SavedAt:   time.Now().Add(future), // written while the clock was 3 days ahead
		TTLSecond: pickerCacheTTL.Seconds(),
		Models:    []LaunchModel{{Name: "stale/model", Remote: true}},
		Inputs:    pickerInputFingerprint(),
	})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	_, stale, ok := loadPickerCache()
	if ok && !stale {
		t.Errorf("a cache dated %s in the future is served as FRESH (ok=%v stale=%v): a negative age never "+
			"expires against pickerCacheGrace and never triggers the background refresh", future, ok, stale)
	}
}

// TestModelInventorySameSizeMtimePreservedEditVoidsTheCache: the fingerprint
// was mtime+size, so a same-size edit that keeps the mtime (touch -r, rsync -a,
// cp -p, a restore, a dotfiles manager, or any filesystem with 1-second
// granularity) was invisible and the old rows were served. It is a content
// digest now.
func TestModelInventorySameSizeMtimePreservedEditVoidsTheCache(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "http://127.0.0.1:1")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "cloud-a"}}, nil)

	home, _ := os.UserHomeDir()
	remotesPath := filepath.Join(home, ".oaica", "remotes.json")
	t.Setenv("OAICA_REMOTES_FILE", remotesPath)
	before := `{"remotes":[{"name":"aaa","base_url":"http://127.0.0.1:9/v1"}]}`
	after := `{"remotes":[{"name":"bbb","base_url":"http://127.0.0.1:9/v1"}]}`
	if len(before) != len(after) {
		t.Fatalf("fixture: lengths differ (%d vs %d)", len(before), len(after))
	}
	if err := os.WriteFile(remotesPath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(remotesPath)
	if err != nil {
		t.Fatal(err)
	}
	stubUserRemoteModels(t, []LaunchModel{{Name: "aaa/model", Remote: true}}, nil)

	first, err := newModelInventory(nil).Load(context.Background())
	if err != nil || !invHas(first, "aaa/model") {
		t.Fatalf("first Load = %v, %v", invNames(first), err)
	}

	// Same byte count, mtime restored to what it was.
	if err := os.WriteFile(remotesPath, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(remotesPath, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	stubUserRemoteModels(t, []LaunchModel{{Name: "bbb/model", Remote: true}}, nil)

	second, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if invHas(second, "aaa/model") || !invHas(second, "bbb/model") {
		t.Errorf("remotes.json changed (aaa -> bbb, same size, mtime preserved) but the picker shows %v: "+
			"an mtime+size fingerprint cannot see that edit", invNames(second))
	}
}

// TestModelInventoryNewOllamaCloudModelAppearsOnTheNextLoad: with OAICA_HOST
// unset the inventory appends "ollama/<id>" rows from the ollama-cloud catalog
// cache — a file, like the others, so it is fingerprinted too.
func TestModelInventoryNewOllamaCloudModelAppearsOnTheNextLoad(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "") // the ollama-cloud merge only happens unpinned
	stubCloudFetch(t, []oaicaModelEntry{{ID: "cloud-a"}}, nil)
	// stubCloudFetch nils the ollama-cloud seam; put the real one back. A fresh
	// on-disk cache below means it never touches the network.
	prev := ollamaCloudEntriesFn
	ollamaCloudEntriesFn = ollamaCloudEntries
	t.Cleanup(func() { ollamaCloudEntriesFn = prev })
	stubUserRemoteModels(t, nil, nil)
	writeRemotes(t, `{"remotes":[]}`)

	writeCloud := func(ids ...string) {
		t.Helper()
		path, err := ollamaCloudCachePath()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(ollamaCloudCache{SavedAt: time.Now(), TTLSecond: ollamaCloudCacheTTL.Seconds(), IDs: ids})
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeCloud("aa-id")
	first, err := newModelInventory(nil).Load(context.Background())
	if err != nil || !invHas(first, ollamaCloudPickerPrefix+"aa-id") {
		t.Fatalf("first Load = %v, %v", invNames(first), err)
	}

	// The scrape picked up Ollama's newest cloud model.
	writeCloud("aa-id", "bb-id")
	second, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if !invHas(second, ollamaCloudPickerPrefix+"bb-id") {
		t.Errorf("the ollama-cloud catalog now lists bb-id but the picker shows %v — the ollama-cloud cache "+
			"was not fingerprinted, so its new rows stayed hidden", invNames(second))
	}
}

// TestModelInventoryFingerprintCoversTheDocumentedInputs is the premise every
// test above rests on: one Load writes a fingerprint naming each input the
// refresh help text promises to notice.
func TestModelInventoryFingerprintCoversTheDocumentedInputs(t *testing.T) {
	withTempOaicaHome(t)
	writeRemotes(t, `{"remotes":[]}`)
	fp := pickerInputFingerprint()
	// Every path the input list names must have a digest (or be recorded as
	// absent — a missing file is still an input state).
	for _, p := range pickerCacheInputPaths() {
		if _, ok := fp[p]; !ok {
			t.Errorf("fingerprint does not cover %s", p)
		}
	}
	// The list itself must name each documented input, not just whatever a
	// Load happens to touch: a file absent from it is a file whose edit the
	// cache cannot see. Paths come from the same resolvers the rest of the
	// client uses, so a moved or renamed store is covered by construction.
	covered := map[string]bool{}
	for _, p := range pickerCacheInputPaths() {
		covered[p] = true
	}
	localServers, lsErr := oaicaLocalServersRegistryPath()
	cloudCache, ccErr := ollamaCloudCachePath()
	manifest, mfErr := modelManifestPath()
	catalog, catErr := oaicaOverlayCachePath()
	license, licErr := licenseFilePath()
	for _, e := range []error{lsErr, ccErr, mfErr, catErr, licErr} {
		if e != nil {
			t.Fatalf("resolver: %v", e)
		}
	}
	for _, p := range []string{userRemotesPath(), authStorePath(), localServers, cloudCache, manifest, catalog, license} {
		if !covered[p] {
			t.Errorf("the picker's input list does not cover %s: %v", p, pickerCacheInputPaths())
		}
	}
	for _, p := range opencodeAuthPaths() {
		if !covered[p] {
			t.Errorf("the picker's input list does not cover the external auth store %s", p)
		}
	}
	_ = filepath.Base // filepath is used by the fixtures above
	if _, ok := fp["env:OAICA_HOST"]; !ok {
		t.Errorf("fingerprint does not cover OAICA_HOST: %v", fp)
	}
}
