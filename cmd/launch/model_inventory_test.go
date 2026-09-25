package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
	savePickerCache(models)
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
	savePickerCache([]LaunchModel{{Name: "old/model", Remote: true}})
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
