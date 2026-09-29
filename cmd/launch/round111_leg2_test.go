package launch

// round111_leg2_test.go — leg 2, round 111 (2026-09-29 audit), F111-L2-1..4.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// F111-L2-1: round 110's directLaunchEnv scrubs the names openclawCredentialEnvNames
// derives, and on a remotes.json that did not parse that function returned only the
// router key's names, never the built-in providers'. Every other reader of the store
// falls back to the built-ins on that error, and the built-in providers stay
// launchable in that state, so one typo or a truncated write left their real keys in
// the environment of every codex, copilot, kimi and openclaw child.
func TestMine111ACorruptRemotesFileStillScrubsTheBuiltInProviderKeys(t *testing.T) {
	for name, body := range map[string]string{
		"truncated":         `{"remotes":[{"name":"box","base_url":"http://box:1/v1","api_key_env":"BOX_KEY"`,
		"wrong-typed field": `{"remotes":[{"name":"box","base_url":"http://box:1/v1","weight":"3"}]}`,
	} {
		setLaunchTestHome(t, t.TempDir())
		writeRemotes(t, body)
		t.Setenv("Z_AI_API_KEY", "sk-zai-REAL")
		t.Setenv("OPENROUTER_API_KEY", "sk-or-REAL")
		t.Setenv("OAICA_API_KEY", "sk-oaica-REAL")
		env := strings.Join(directLaunchEnv("OPENAI_API_KEY=intended"), "\n")
		for _, leak := range []string{"sk-zai-REAL", "sk-or-REAL", "sk-oaica-REAL"} {
			if strings.Contains(env, leak) {
				t.Errorf("%s remotes.json: the child's environment carries %q — an unreadable store must not stop the built-in providers' keys being scrubbed (2026-09-29 audit, round 111, F111-L2-1)", name, leak)
			}
		}
	}
}

// F111-L2-2: round 109 put credentialSafeRedirect on the credentialed clients and the
// catalog and model syncs kept a bare http.Client, so a --url mirror that carries its
// key in the URL, or as userinfo, had that URL handed to a redirect target as Referer.
func TestMine111TheSyncFetchesDoNotLeakTheMirrorURLAsAReferer(t *testing.T) {
	var mu sync.Mutex
	var referer string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		referer = r.Header.Get("Referer")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(target.Close)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api.json", http.StatusFound)
	}))
	t.Cleanup(mirror.Close)
	for name, fetch := range map[string]func(){
		"catalog sync": func() {
			setLaunchTestHome(t, t.TempDir())
			_, _, _, _ = fetchCatalogBody(mirror.URL+"/api.json?key=SECRETQ", "", filepath.Join(t.TempDir(), "c.json"))
		},
		"model sync": func() {
			setLaunchTestHome(t, t.TempDir())
			_, _, _ = fetchModelCatalog(mirror.URL+"/models.json?key=SECRETQ", mirror.URL+"/models.json")
		},
	} {
		mu.Lock()
		referer = ""
		mu.Unlock()
		fetch()
		mu.Lock()
		got := referer
		mu.Unlock()
		if strings.Contains(got, "SECRETQ") {
			t.Errorf("%s: the redirect target received Referer %q — the mirror's URL carries its key (2026-09-29 audit, round 111, F111-L2-2)", name, got)
		}
	}
	_ = os.Getenv
}

// F111-L2-3: credentialSafeRedirect compared the Host string only, so an https to http
// redirect on the same host kept the key on a cleartext hop.
func TestMine111ASchemeDowngradeDropsTheCredential(t *testing.T) {
	for name, c := range map[string]struct {
		to   string
		keep bool
	}{
		"https to http, same host":           {"http://api.vendor.com/x", false},
		"https to https, same host":          {"https://api.vendor.com/y", true},
		"https to other host":                {"https://other.vendor.com/y", false},
		"same explicit port, scheme differs": {"http://api.vendor.com:8080/y", false},
	} {
		from := "https://api.vendor.com/x"
		if strings.Contains(name, "explicit port") {
			from = "https://api.vendor.com:8080/x"
		}
		via, _ := http.NewRequest("GET", from, nil)
		req, _ := http.NewRequest("GET", c.to, nil)
		req.Header.Set("X-Api-Key", "sk-secret")
		if err := credentialSafeRedirect(req, []*http.Request{via}); err != nil {
			t.Fatal(err)
		}
		if kept := req.Header.Get("X-Api-Key") != ""; kept != c.keep {
			t.Errorf("%s: key kept=%v, want %v (2026-09-29 audit, round 111, F111-L2-3)", name, kept, c.keep)
		}
	}
}

// F111-L2-4: modelsPathIsCrossHost compared Hostname() only, so an absolute models_path
// on another port of the same host, or on cleartext http for an https base, was treated
// as the row's own host and the row's Bearer key went there.
func TestMine111AModelsPathOnAnotherPortOrSchemeIsCrossHost(t *testing.T) {
	for name, c := range map[string]struct {
		base, path string
		cross      bool
	}{
		"same origin":           {"https://api.vendor.com/v1", "https://api.vendor.com/v1/models", false},
		"default port spelled":  {"https://api.vendor.com/v1", "https://api.vendor.com:443/models", false},
		"another port":          {"https://api.vendor.com/v1", "https://api.vendor.com:8443/models", true},
		"cleartext for https":   {"https://api.vendor.com/v1", "http://api.vendor.com/models", true},
		"another host":          {"https://api.vendor.com/v1", "https://evil.example.com/models", true},
		"http base, http path":  {"http://192.168.0.5:8080/v1", "http://192.168.0.5:8080/models", false},
		"http base, other port": {"http://192.168.0.5:8080/v1", "http://192.168.0.5:9090/models", true},
	} {
		r := userRemote{Name: "x", BaseURL: c.base, ModelsPath: c.path}
		if got := r.modelsPathIsCrossHost(); got != c.cross {
			t.Errorf("%s: modelsPathIsCrossHost=%v, want %v (2026-09-29 audit, round 111, F111-L2-4)", name, got, c.cross)
		}
	}
}
