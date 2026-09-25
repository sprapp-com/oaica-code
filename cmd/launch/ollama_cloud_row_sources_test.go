package launch

// ollama_cloud_row_sources_test.go — the ollama-cloud catalogue is a row
// PRODUCER, not a decoration (round-5 audit, 2026-09-26).
//
// Two defects, one premise: `ollama/gpt-oss` is a display id, and the daemon
// name behind it ("gpt-oss:cloud", LaunchModel.Upstream) is the launchable
// one.
//
//   - The launch path sent the stripped BARE id. stripOllamaPickerNames is
//     lexical and cannot tell a daemon row ("ollama/gpt-oss" → local model
//     "gpt-oss") from a catalogue row (→ daemon-side "gpt-oss:cloud"), so
//     selecting an Ollama-cloud row asked the daemon for a local model: it
//     failed "not pulled on the local daemon" with a multi-GB pull offer, or
//     silently ran a local model of that name, while the cloud alias the row
//     itself documents launched fine.
//   - The cache-hit merge re-derived only the daemon and `oaica serve`
//     families, so dropping a daemon row the daemon no longer serves left
//     nothing in its place even when the catalogue — one of the picker
//     cache's own fingerprint inputs — still lists the name. The row vanished
//     for up to pickerCacheTTL (1h) and came back only on a forced load.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// writeOllamaCloudCache puts a fresh ollama-cloud catalogue cache (the file the
// scrape writes) in the test HOME, so ollamaCloudEntries never hits the network.
func writeOllamaCloudCache(t *testing.T, ids ...string) {
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

// cloudAliasDaemon is a daemon that knows the CLOUD alias (e.g. "gpt-oss:cloud")
// but has no local model of the bare name, and records every id it is asked
// about.
func cloudAliasDaemon(t *testing.T, cloudName string, asked *[]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[]}`)
		case "/api/show":
			b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
			var req struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(b, &req)
			*asked = append(*asked, req.Model)
			if req.Model == cloudName {
				fmt.Fprintf(w, `{"model":%q}`, cloudName)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"model not found"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OLLAMA_HOST", srv.URL)
}

// The launch name for a picker selection is the name the backend knows: the
// row's Upstream when it has one, the stripped bare id otherwise.
func TestOllamaCloudRowLaunchesItsDocumentedDaemonName(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "") // the ollama-cloud merge only happens unpinned
	stubCloudFetch(t, []oaicaModelEntry{{ID: "router-row"}}, nil)
	prev := ollamaCloudEntriesFn
	ollamaCloudEntriesFn = ollamaCloudEntries // a fresh on-disk cache means no network
	t.Cleanup(func() { ollamaCloudEntriesFn = prev })
	writeRemotes(t, `{"remotes":[]}`)

	writeOllamaCloudCache(t, "gpt-oss")
	var asked []string
	cloudAliasDaemon(t, "gpt-oss:cloud", &asked)

	models, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	row, ok := findLaunchModel(models, ollamaCloudPickerPrefix+"gpt-oss")
	if !ok {
		t.Fatalf("premise changed: the ollama-cloud catalogue row is not in the inventory: %v", invNames(models))
	}
	if row.Upstream != "gpt-oss:cloud" {
		t.Fatalf("premise changed: the row does not carry its documented daemon name: %+v", row)
	}

	sent := launchNameForPickerName(models, ollamaCloudPickerPrefix+"gpt-oss")

	// CONTROL: the name the row documents is launchable on this daemon, so the
	// row is not an aspirational entry.
	upEp, upErr := resolveLaunchEndpoint(row.Upstream)
	if upErr != nil {
		t.Fatalf("premise changed: the row's own documented upstream %q does not resolve on this daemon: %v",
			row.Upstream, upErr)
	}
	if sent != row.Upstream {
		sentEp, sentErr := resolveLaunchEndpoint(sent)
		t.Errorf("the picker row %q documents the daemon name %q (LaunchModel.Upstream) and that name launches "+
			"(source=%q), but the launch path sends %q (source=%q, err=%v): selecting this row fails with "+
			"\"not pulled on the local daemon\" — or runs a LOCAL model of that name while the row says Ollama "+
			"cloud. Daemon was asked for %v", row.Name, row.Upstream, upEp.Source, sent, sentEp.Source, sentErr, asked)
	}

	// The counterpart: a daemon row's own name is the bare id, not some
	// invented upstream.
	if _, ok := findLaunchModel(models, "gpt-oss:cloud"); ok {
		t.Fatalf("premise changed: a bare id now resolves to a row with an Upstream")
	}
	if got := launchNameForPickerName(models, "anything-else"); got != "anything-else" {
		t.Errorf("a name with no row was rewritten to %q, want it passed through", got)
	}
}

// listSwitchDaemon is an Ollama daemon whose /api/tags list can be swapped
// between calls (`ollama pull` / `ollama rm` while the picker cache is warm).
func listSwitchDaemon(t *testing.T, list *[]string, mu *sync.Mutex) *api.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		names := append([]string(nil), *list...)
		mu.Unlock()
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

// A cache hit re-derives the live families and drops the cached copy of a row
// the daemon no longer serves — right, and deliberate. What must not happen is
// the name disappearing entirely when another producer still offers it: the
// ollama-cloud catalogue lists "gpt-oss", so the picker keeps the row (served
// via the cloud account) instead of losing it for an hour.
func TestCacheHitKeepsTheCatalogueRowTheDaemonStoppedServing(t *testing.T) {
	withTempOaicaHome(t)
	t.Setenv("OAICA_HOST", "")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "router-row"}}, nil)
	prev := ollamaCloudEntriesFn
	ollamaCloudEntriesFn = ollamaCloudEntries // a fresh on-disk cache means no network
	t.Cleanup(func() { ollamaCloudEntriesFn = prev })
	writeRemotes(t, `{"remotes":[]}`)
	writeOllamaCloudCache(t, "gpt-oss")

	var mu sync.Mutex
	list := []string{"gpt-oss"}
	client := listSwitchDaemon(t, &list, &mu)

	first, err := newModelInventory(client).Load(context.Background())
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	row, ok := findLaunchModel(first, ollamaCloudPickerPrefix+"gpt-oss")
	if !ok {
		t.Fatalf("premise changed: no ollama/gpt-oss row from either producer: %v", invNames(first))
	}
	if row.LiveSource != liveSourceDaemon {
		t.Fatalf("premise changed: the row is not the daemon's copy (LiveSource=%q): %+v", row.LiveSource, row)
	}
	if _, _, ok := loadPickerCache(); !ok {
		t.Fatalf("premise changed: no valid picker cache was written by the first load")
	}

	// `ollama rm gpt-oss` — gone from this box. Nothing in ~/.oaica changes,
	// so the picker cache still validates.
	mu.Lock()
	list = []string{}
	mu.Unlock()

	second, err := newModelInventory(client).Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if _, _, ok := loadPickerCache(); !ok {
		t.Fatalf("premise changed: the picker cache no longer validates, so this test does not exercise the cache-hit merge")
	}
	// CONTROL: a forced load still lists the row, so it is genuinely available
	// — this is not a case of the model being gone.
	forced, ferr := newModelInventory(client).Refresh(context.Background())
	if !invHas(forced, ollamaCloudPickerPrefix+"gpt-oss") {
		t.Fatalf("premise changed: a forced load no longer lists ollama/gpt-oss either: %v (err=%v)", invNames(forced), ferr)
	}

	if !invHas(second, ollamaCloudPickerPrefix+"gpt-oss") {
		t.Errorf("the cache-hit picker list is %v but a forced load lists %v: after `ollama rm gpt-oss` the row "+
			"vanished for up to pickerCacheTTL (1h) although the ollama-cloud catalogue — one of the cache's own "+
			"fingerprint inputs — still offers it. The cache-hit merge drops the cached daemon row and must re-derive "+
			"the catalogue's rows in its place", invNames(second), invNames(forced))
	}
}
