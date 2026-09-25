package launch

// picker_cache_invariants_test.go — regression pins for the picker inventory
// and its cross-process cache: which rows a cache hit may keep (file- and
// document-backed ones), which it must re-derive (the daemon's /api/tags, the
// live `oaica serve` rows behind a /health probe), what voids a cache
// (a configuration edit, including the router credential), and what the cache
// file may contain (no credential).
//
// Each test was written as a failing reproduction of one defect during the
// 2026-09-26 audit and now pins the fixed behaviour. Two of them assert the
// FIXED direction of a premise the audit originally recorded as broken; each
// says so where it matters.
//
//	rtk proxy go test ./cmd/launch/ -run 'CacheHit|PickerCache|HostUserinfo|SavedRouterKey' -count=1

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
	"sync"
	"testing"
	"time"

	"github.com/ollama/ollama/api"
)

// auditR3CDaemon starts a live daemon that answers /api/tags with the given
// model names — the one runtime source mergeLiveRows re-reads on a cache hit.
func auditR3CDaemon(t *testing.T, names ...string) *api.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, fmt.Sprintf(`{"name":%q}`, n))
		}
		fmt.Fprintf(w, `{"models":[%s]}`, strings.Join(parts, ","))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewClient(u, srv.Client())
}

// auditR3CHealthServer answers 200 on /health, which is what makes an entry in
// ~/.oaica/local_servers.json count as a LIVE `oaica serve`.
func auditR3CHealthServer(t *testing.T) string {
	t.Helper()
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(health.Close)
	return health.URL
}

func auditR3CRegisterLocalServer(t *testing.T, model, origin string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".oaica", "local_servers.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	reg := fmt.Sprintf(`[{"model":%q,"origin":%q,"pid":1,"started_at":"now"}]`, model, origin)
	if err := os.WriteFile(path, []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func auditR3CLoad(t *testing.T, client *api.Client, label string) []LaunchModel {
	t.Helper()
	models, err := newModelInventory(client).Load(context.Background())
	if err != nil {
		t.Fatalf("%s: Load: %v", label, err)
	}
	return models
}

// A running `oaica serve <model>` is offered as the distinctly-tagged
// "<model>:local" row (oaica_models.go). It is produced from
// ~/.oaica/local_servers.json plus a /health probe — never from the daemon's
// /api/tags list that mergeLiveRows re-reads. The cache-hit merge used to
// treat any ":local" name as a daemon row, so the cached row was dropped
// and the live daemon read cannot put it back: the local entry is gone from
// every consumer of Load() (the tier wizard's full model list at
// launch.go:871, agent_routing.go's agentModelMeta, Resolve) until something
// unrelated voids the cache.
func TestAuditR3CCacheHitDropsTheLocalServeRow(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "") // :local rows only merge in unpinned
	stubCloudFetch(t, []oaicaModelEntry{{ID: "cloud-a", Description: "router row"}}, nil)
	stubUserRemoteModels(t, nil, nil)
	writeRemotes(t, `{"remotes":[]}`)
	auditR3CRegisterLocalServer(t, "kat-awq", auditR3CHealthServer(t))
	client := auditR3CDaemon(t, "local-daemon-model")

	first := auditR3CLoad(t, client, "first")
	if !invHas(first, "kat-awq"+oaicaLocalTagSuffix) {
		t.Fatalf("premise broken: the full load did not produce the :local row: %v", invNames(first))
	}
	if _, statErr := os.Stat(invPickerCachePath(t)); statErr != nil {
		t.Fatalf("premise broken: no cache written: %v", statErr)
	}
	// Nothing under ~/.oaica changes between the two launches: same
	// local_servers.json, same remotes.json, same everything.
	second := auditR3CLoad(t, client, "second")

	if !invHas(second, "kat-awq"+oaicaLocalTagSuffix) {
		forced, ferr := newModelInventory(client).Refresh(context.Background())
		t.Errorf("the running `oaica serve kat-awq` entry vanished from the second launch's rows: %v "+
			"(the full load still finds it: %v, err=%v). The cache-hit merge must classify rows by "+
			"their SOURCE, not their name: \"<model>:local\" comes from "+
			"local_servers.json + a /health probe and is never in the daemon's /api/tags list.",
			invNames(second), invNames(forced), ferr)
	}
}

// Same defect, second family: the ollama-cloud catalog rows are named
// "ollama/<id>" (ollamaCloudPickerPrefix) straight out of
// ~/.oaica/cache/models/ollama-cloud.json — not out of the daemon. A name-prefix
// predicate matched them as daemon rows, so a cache hit dropped them and the live daemon read
// re-adds only what the daemon itself has (a cloud model the user has not
// pulled is not there).
func TestAuditR3CCacheHitDropsTheOllamaCloudRows(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "cloud-a", Description: "router row"}}, nil)
	prev := ollamaCloudEntriesFn
	ollamaCloudEntriesFn = ollamaCloudEntries // a fresh on-disk cache means no network
	t.Cleanup(func() { ollamaCloudEntriesFn = prev })
	stubUserRemoteModels(t, nil, nil)
	writeRemotes(t, `{"remotes":[]}`)

	cloudPath, err := ollamaCloudCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cloudPath), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(ollamaCloudCache{SavedAt: time.Now(), TTLSecond: ollamaCloudCacheTTL.Seconds(), IDs: []string{"gpt-oss"}})
	if err := os.WriteFile(cloudPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	client := auditR3CDaemon(t, "local-daemon-model")
	first := auditR3CLoad(t, client, "first")
	if !invHas(first, ollamaCloudPickerPrefix+"gpt-oss") {
		t.Fatalf("premise broken: the full load did not produce the ollama-cloud row: %v", invNames(first))
	}

	second := auditR3CLoad(t, client, "second")
	if !invHas(second, ollamaCloudPickerPrefix+"gpt-oss") {
		forced, ferr := newModelInventory(client).Refresh(context.Background())
		t.Errorf("the ollama-cloud catalog row ollama/gpt-oss vanished from the second launch's rows: %v "+
			"(the full load still finds it: %v, err=%v). Its source file did not change, and the daemon's "+
			"/api/tags list does not carry it — only a name-prefix predicate like the one that used to stand here removes it.",
			invNames(second), invNames(forced), ferr)
	}
}

// The third row family in the same cache: a ":local" row whose server has since
// DIED. The /health probe in oaicaLocalServerEntries is the only thing that
// removes a crashed `oaica serve` (the registry file is written by the server
// and only rewritten by a clean shutdown), and it is runtime state the cache
// cannot hold. The merge used to be skipped entirely when the daemon did not
// answer, so the dead row was painted for up to pickerCacheTTL — and picking it
// is worse than a failure: oaicaResolveHostForModel falls through to the CLOUD
// host when no live server matches the ":local" tag (oaica_models.go:132-137),
// so the row silently launches a different backend than it names.
func TestAuditR3CCacheHitKeepsADeadLocalServeRow(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "cloud-a", Description: "router row"}}, nil)
	stubUserRemoteModels(t, nil, nil)
	writeRemotes(t, `{"remotes":[]}`)

	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	auditR3CRegisterLocalServer(t, "kat-awq", health.URL)

	first := auditR3CLoad(t, nil, "first")
	if !invHas(first, "kat-awq"+oaicaLocalTagSuffix) {
		t.Fatalf("premise broken: %v", invNames(first))
	}
	if _, statErr := os.Stat(invPickerCachePath(t)); statErr != nil {
		t.Fatalf("premise broken: no cache written: %v", statErr)
	}

	// `oaica serve kat-awq` dies (kill -9: no cleanup, so local_servers.json
	// still names it). Nothing under ~/.oaica changes.
	health.Close()

	second := auditR3CLoad(t, deadClient(t), "second")
	forced, ferr := newModelInventory(deadClient(t)).Refresh(context.Background())

	if invHas(second, "kat-awq"+oaicaLocalTagSuffix) {
		t.Errorf("the picker still offers kat-awq%s after that server died: %v (a forced load drops it: %v, "+
			"err=%v). Nothing re-probes /health on the cache-hit path, and the merge is skipped entirely when "+
			"the daemon is unreachable — so a dead origin stays on the menu for up to pickerCacheTTL, and "+
			"selecting it routes to the cloud host instead (oaicaResolveHostForModel's documented fall-through).",
			oaicaLocalTagSuffix, invNames(second), invNames(forced), ferr)
	}
}

// The other direction of the same predicate: a daemon model whose name carries
// a "/" (Ollama's hugggingface syntax, "hf.co/<user>/<repo>") keeps its own
// name in daemonPickerRows (the ollama/ prefix is only added to bare names), so
// a name-prefix predicate did NOT recognize it — the cached row survived even
// though the live /api/tags read just said the model is gone. The help text promises the
// opposite: "the local daemon's list is re-read live even when the cache is
// used, so a 'ollama pull' or 'ollama rm' shows up on the next launch as well".
func TestAuditR3CCacheHitKeepsARemovedNamespacedDaemonRow(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "http://127.0.0.1:1") // pinned: no :local / ollama-cloud merge
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
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := api.NewClient(u, srv.Client())

	const hf = "hf.co/bartowski/Llama-3.2-1B-Instruct-GGUF"
	daemonModels = []string{hf}
	first := auditR3CLoad(t, client, "first")
	if !invHas(first, hf) {
		t.Fatalf("premise broken: the daemon's namespaced model is not in the rows: %v", invNames(first))
	}

	// `ollama rm hf.co/bartowski/...`: the daemon no longer lists it. Nothing
	// under ~/.oaica changes (a daemon list is runtime state with no file).
	daemonModels = nil
	second := auditR3CLoad(t, client, "second")

	if invHas(second, hf) {
		t.Errorf("after `ollama rm %s` the picker still offers it: %v. The live daemon read that this "+
			"cache-hit path performs returned an empty list, yet the cached row survived the merge — "+
			"the merge must recognize a daemon row by its source, not by a name shape: names "+
			"daemonPickerRows passes through untouched because they contain a \"/\" were not "+
			"recognized before.",
			hf, invNames(second))
	}
}

// The router credential: OAICA_API_KEY, or the ~/.oaica/api_key file `oaica
// signin` writes (oaicaLaunchAPIKeyForEnv falls back to it). Neither is part of
// pickerCacheInputPaths/pickerEnvFingerprint — while the SAME credential riding
// in OAICA_HOST's userinfo is hashed into the fingerprint, and every
// providerCatalog() row's credential presence is recorded. A re-signed-in or
// rotated router key therefore leaves the router's rows frozen for up to
// pickerCacheTTL, which is the exact "I logged in and the picker still shows
// nothing" shape the fingerprint exists to prevent.
func TestAuditR3CSavedRouterKeyIsNotAFingerprintInput(t *testing.T) {
	withTempOaicaHome(t)
	stubUserRemoteModels(t, nil, nil)
	writeRemotes(t, `{"remotes":[]}`)
	t.Setenv("OAICA_API_KEY", "") // the file is the credential under test

	keyPath := filepath.Join(os.Getenv("HOME"), ".oaica", "api_key")
	if err := os.WriteFile(keyPath, []byte("free-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The router's catalogue is per-credential.
	requests := 0
	routes := map[string]string{
		"Bearer free-key": `{"data":[{"id":"free-row"}]}`,
		"Bearer pro-key":  `{"data":[{"id":"free-row"},{"id":"pro-row"}]}`,
	}
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		requests++
		body, ok := routes[r.Header.Get("Authorization")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"code":"invalid_api_key"}}`)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(router.Close)
	t.Setenv("OAICA_HOST", router.URL)

	first := auditR3CLoad(t, nil, "first")
	if !invHas(first, "free-row") || invHas(first, "pro-row") {
		t.Fatalf("premise broken: rows for the free key = %v", invNames(first))
	}
	// The env spelling of the same credential is an input: — pickerEnvFingerprint carries a hashed "cred:OAICA_API_KEY".
	t.Setenv("OAICA_API_KEY", "pro-key")
	if v := pickerEnvFingerprint()["cred:OAICA_API_KEY"]; v == "" || v == "unset" {
		t.Errorf("the env half does not cover OAICA_API_KEY (%q)", v)
	}
	t.Setenv("OAICA_API_KEY", "")

	// The user signs in again (or rotates the key): ~/.oaica/api_key changes,
	// nothing else does. `oaica signin` writes this file and nothing else.
	if err := os.WriteFile(keyPath, []byte("pro-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests = 0
	// The in-process router memo would answer the next load from the free key
	// regardless of what the picker cache does; clear it so the assertion below
	// observes the cache, not the memo.
	cloudEntriesCache.Lock()
	cloudEntriesCache.host = ""
	cloudEntriesCache.expiresAt = time.Time{}
	cloudEntriesCache.Unlock()

	second := auditR3CLoad(t, nil, "second")
	forced, ferr := newModelInventory(nil).Refresh(context.Background())

	if !invHas(second, "pro-row") {
		t.Errorf("after ~/.oaica/api_key changed free-key -> pro-key the picker still shows %v and made %d "+
			"router request(s) (a forced load returns %v, err=%v): the router credential is not part of the "+
			"cache fingerprint, so the rows are served for the old account until the TTL lapses.",
			invNames(second), requests, invNames(forced), ferr)
	}
}

// Checked and clean: concurrent writers cannot publish a mixed file. The two
// real writers are a background refresh in a long-lived `oaica launch` (stale
// cache path, model_inventory.go:142) and a second `oaica launch` in another
// terminal; both go through fileutil.WriteFileAtomic (unique temp + rename).
// This test is expected to PASS — every read must observe one writer's WHOLE
// list, never a blend of two, and never a parse failure.
func TestAuditR3CConcurrentCacheWritersNeverPublishAMixedFile(t *testing.T) {
	withTempOaicaHome(t)
	sets := [][]LaunchModel{
		{{Name: "a1", Remote: true}, {Name: "a2", Remote: true}, {Name: "a3", Remote: true}},
		{{Name: "b1", Remote: true}, {Name: "b2", Remote: true}, {Name: "b3", Remote: true}, {Name: "b4", Remote: true}, {Name: "b5", Remote: true}},
	}
	start := make(chan struct{})
	writersDone := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for n := 0; n < 60; n++ {
				savePickerCache(sets[i%2], pickerInputFingerprint())
			}
		}(i)
	}
	go func() { wg.Wait(); close(writersDone) }()
	close(start)

	seen := map[string]int{}
	reads := 0
	for {
		select {
		case <-writersDone:
			// Drain a few more reads so the last published file is seen too.
			for n := 0; n < 5; n++ {
				auditR3CReadCache(t, seen)
			}
			goto report
		default:
		}
		auditR3CReadCache(t, seen)
		reads++
	}
report:
	if len(seen) == 0 {
		t.Fatalf("no read ever observed a published cache in %d reads — nothing proven", reads)
	}
	t.Logf("published caches observed: %v", seen)
	for key := range seen {
		if key != "a1,a2,a3" && key != "b1,b2,b3,b4,b5" {
			t.Errorf("a read returned a list no writer ever wrote: %q", key)
		}
	}
}

func auditR3CReadCache(t *testing.T, seen map[string]int) {
	t.Helper()
	models, _, ok := loadPickerCache()
	if !ok {
		return
	}
	names := invNames(models)
	if len(names) == 0 {
		t.Fatalf("a read returned a cache with zero row names")
	}
	seen[strings.Join(names, ",")]++
	prefix := names[0][:1]
	for _, n := range names {
		if n[:1] != prefix {
			t.Fatalf("read a MIXED list from the cache: %v", names)
		}
	}
}

// Checked and clean: no credential reaches the cache file. Every secret shape a
// load touches is planted here — an inline remotes.json api_key, an exported
// provider key, and a router key in OAICA_HOST's userinfo (the one the
// fingerprint does hash) — and none of them may appear in the bytes of
// ~/.oaica/picker_cache.json. This test is expected to PASS; it exists to pin
// the leak rule (docs/ENTERPRISE.md) against the fingerprint and the rows.
func TestAuditR3CCacheFileLeaksNoCredential(t *testing.T) {
	withTempOaicaHome(t)
	const (
		inlineKey = "sk-inline-leak-12345"
		envKey    = "sk-env-leak-67890"
		hostKey   = "router-cred-leak-11111"
		userinfo  = "leak-user:leak-pass"
	)
	writeRemotes(t, fmt.Sprintf(`{"remotes":[{"name":"box","base_url":"http://127.0.0.1:9/v1","api_key":%q}]}`, inlineKey))
	stubUserRemoteModels(t, []LaunchModel{{Name: "box/m1", Remote: true}}, nil)
	stubCloudFetch(t, []oaicaModelEntry{{ID: "cloud-a"}}, nil)
	t.Setenv("AUDITBOX_KEY", envKey)
	invSyncedCatalogWithGatedBox(t)
	t.Setenv("OAICA_HOST", "http://"+userinfo+"@"+hostKey+".invalid")

	models := auditR3CLoad(t, nil, "load")
	if len(models) == 0 {
		t.Fatal("premise broken: no rows, so no cache file")
	}
	b, err := os.ReadFile(invPickerCachePath(t))
	if err != nil {
		t.Fatalf("premise broken: %v", err)
	}
	for _, secret := range []string{inlineKey, envKey, hostKey, "leak-pass", "leak-user"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the picker cache file contains %q: %s", secret, b)
		}
	}
}

// The fingerprint is computed at SAVE time (savePickerCache), while the rows it
// labels were built from the configuration as it was read at the START of the
// load — a window that spans the whole probe (remote sweeps + router, seconds).
// A configuration edit inside that window is therefore recorded as the cache's
// own fingerprint: the cache claims to have been built from a configuration it
// never saw, is trusted, and serves the previous configuration's rows. That is
// the round-2 defect ("a just-added remote is invisible to the next launch")
// through a race window instead of through a missing input — and the writers
// are exactly the cross-process commands the cache documents: `oaica remote
// add`, `oaica auth login`, `oaica model add`, `oaica signin`, or a second
// launch.
func TestAuditR3CCacheCanBeSavedUnderAFingerprintItWasNotBuiltFrom(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "http://127.0.0.1:1")
	stubCloudFetch(t, nil, nil)

	remotesPath := filepath.Join(t.TempDir(), "remotes.json")
	t.Setenv("OAICA_REMOTES_FILE", remotesPath)
	before := `{"remotes":[{"name":"oldbox","base_url":"http://127.0.0.1:9/v1"}]}`
	after := `{"remotes":[{"name":"newbox","base_url":"http://127.0.0.1:9/v1"}]}`
	if err := os.WriteFile(remotesPath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	stubUserRemoteModels(t, []LaunchModel{{Name: "oldbox/m1", Remote: true}}, nil)

	// A second process edits remotes.json mid-load. The router fetch is the
	// last network step before the cache is written, so hooking it makes the
	// window deterministic.
	prev := oaicaFetchCloudModelEntries
	oaicaFetchCloudModelEntries = func() ([]oaicaModelEntry, error) {
		if err := os.WriteFile(remotesPath, []byte(after), 0o600); err != nil {
			return nil, err
		}
		return nil, nil
	}
	t.Cleanup(func() { oaicaFetchCloudModelEntries = prev })

	first := auditR3CLoad(t, nil, "first")
	if !invHas(first, "oldbox/m1") {
		t.Fatalf("premise broken: rows = %v", invNames(first))
	}

	// The cache written a moment ago carries the fingerprint of the PRE-edit
	// file, so it must NOT validate — the edit made
	// during the load voids the cache instead of being recorded as its
	// provenance.
	if _, _, ok := loadPickerCache(); ok {
		t.Fatalf("the cache still validates after remotes.json was rewritten mid-load — its rows predate the file it claims to have been built from")
	}
	stubUserRemoteModels(t, []LaunchModel{{Name: "newbox/m1", Remote: true}}, nil)
	second := auditR3CLoad(t, nil, "second")

	if invHas(second, "oldbox/m1") || !invHas(second, "newbox/m1") {
		forced, ferr := newModelInventory(nil).Refresh(context.Background())
		t.Errorf("remotes.json was rewritten to newbox during the previous load; the next launch "+
			"serves %v and the cache still validates (a forced load returns %v, err=%v). Rows from the "+
			"pre-edit configuration are labelled with the post-edit fingerprint, so the cache is trusted "+
			"for a configuration it was never built from.",
			invNames(second), invNames(forced), ferr)
	}
}

// The same credential in the one place the fingerprint DOES cover, for
// contrast: OAICA_HOST userinfo is hashed into the fingerprint, so repointing
// at a different key on the same host changes the cache's env half. The
// asymmetry is what makes the previous test a gap rather than a design choice.
func TestAuditR3CHostUserinfoCredentialIsFingerprinted(t *testing.T) {
	withTempOaicaHome(t)
	stubUserRemoteModels(t, nil, nil)
	writeRemotes(t, `{"remotes":[]}`)
	t.Setenv("OAICA_API_KEY", "")

	t.Setenv("OAICA_HOST", "http://free-key@127.0.0.1:11434")
	free := pickerEnvFingerprint()["env:OAICA_HOST"]
	t.Setenv("OAICA_HOST", "http://pro-key@127.0.0.1:11434")
	pro := pickerEnvFingerprint()["env:OAICA_HOST"]

	if free == pro || free == "" {
		t.Errorf("OAICA_HOST userinfo is not part of the fingerprint (%q vs %q): a rotated router credential "+
			"repointing at the same host would be served from the cache", free, pro)
	}

	// ~/.oaica/api_key — the same credential, the file
	// `oaica signin` writes — IS a fingerprint input, by path (content digest)
	// and by value (hashed "cred:OAICA_API_KEY").
	keyPath := filepath.Join(os.Getenv("HOME"), ".oaica", "api_key")
	if err := os.WriteFile(keyPath, []byte("free-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := pickerInputFingerprint()
	if _, ok := before[keyPath]; !ok {
		t.Errorf("~/.oaica/api_key is not a fingerprint input: %v", before)
	}
	if err := os.WriteFile(keyPath, []byte("pro-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := pickerInputFingerprint()
	if after[keyPath] == before[keyPath] {
		t.Errorf("changing ~/.oaica/api_key did not change its own fingerprint entry (%q)", after[keyPath])
	}
	for k, v := range before {
		if k == keyPath || k == "cred:OAICA_API_KEY" {
			continue
		}
		if after[k] != v {
			t.Errorf("changing ~/.oaica/api_key changed an unrelated fingerprint entry at %s (%q -> %q)", k, v, after[k])
		}
	}
}
