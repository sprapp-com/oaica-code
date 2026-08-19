package launch

// User-defined remotes — bring your own inference endpoint.
//
// Anyone running their own box (llama-server, prism_server, vLLM, an OpenAI
// gateway) can list it in ~/.oaica/remotes.json and have its models appear in
// the SAME picker as local and OAICA-hosted ones. Nothing routes through
// api.sprapp.com, which is a convenience router, not a licence gate — see
// docs/architecture/SELF_HOSTED_REMOTE.md.
//
//	{
//	  "remotes": [
//	    { "name": "mybox",  "base_url": "https://kat.example.com", "api_key": "sk-..." },
//	    { "name": "lan",    "base_url": "http://192.168.1.50:8080", "api_key_env": "LAN_KEY" }
//	  ]
//	}
//
// api_key_env is preferred: it keeps the secret out of the file. If both are
// set the env var wins, so a shared/committed config can name a variable each
// user supplies privately.
//
// Failure of ONE remote never hides the others (or local models): each is
// queried independently and errors are collected, not propagated. A box that
// is asleep should cost you its own entry, not the whole menu.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type userRemote struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key"`
	APIKeyEnv string `json:"api_key_env"`
	// Version is the OpenAI API version prefix appended to base_url when
	// building endpoint URLs (e.g. "/v1/chat/completions"). Defaults to "v1".
	// z.ai is the notable exception: it uses "v4"
	// (https://api.z.ai/api/paas/v4/chat/completions). Leave empty for the
	// common OpenAI "v1" layout.
	Version string `json:"version"`
}

type userRemotesFile struct {
	Remotes []userRemote `json:"remotes"`
}

// key resolves the bearer, preferring the environment so secrets need not be
// written to disk.
func (r userRemote) key() string {
	if env := strings.TrimSpace(r.APIKeyEnv); env != "" {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	return strings.TrimSpace(r.APIKey)
}

func userRemotesPath() string {
	if p := strings.TrimSpace(os.Getenv("OAICA_REMOTES_FILE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".oaica", "remotes.json")
}

// Built-in z.ai provider. Exporting Z_AI_API_KEY (e.g. in ~/.bashrc) is enough
// to make z.ai appear in the picker — no remotes.json edit. Uses the official
// z.ai platform OpenAI-compatible endpoint under its "v4" API version, so GLM
// models (e.g. glm-5.3) are discoverable and selectable.
const (
	zaiName    = "zai"
	zaiBaseURL = "https://api.z.ai/api/paas"
	zaiEnvKey  = "Z_AI_API_KEY"
)

// builtinRemotes returns remotes that oaica knows about without config, active
// only while their credential env var is set.
func builtinRemotes() []userRemote {
	if os.Getenv(zaiEnvKey) == "" {
		return nil
	}
	return []userRemote{{
		Name:      zaiName,
		BaseURL:   zaiBaseURL,
		APIKeyEnv: zaiEnvKey,
		Version:   "v4",
	}}
}

// loadUserRemotes returns the configured remotes. A missing file is normal and
// yields no error — most users have none. Built-in providers (z.ai) are merged
// in so a key exported in the shell is enough to surface them.
func loadUserRemotes() ([]userRemote, error) {
	path := userRemotesPath()
	if path == "" {
		return builtinRemotes(), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return builtinRemotes(), nil
		}
		return nil, err
	}
	var f userRemotesFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make([]userRemote, 0, len(f.Remotes))
	for _, r := range f.Remotes {
		r.Name = strings.TrimSpace(r.Name)
		r.BaseURL = strings.TrimRight(strings.TrimSpace(r.BaseURL), "/")
		if r.Name == "" || r.BaseURL == "" {
			continue // skip malformed entries rather than fail the whole file
		}
		out = append(out, r)
	}
	return append(out, builtinRemotes()...), nil
}

// findUserRemoteForModel splits a "<remote>/<model>" picker name and returns
// the matching configured userRemote plus the bare upstream model id (the
// part after the first "/"), or ok=false if the prefix matches no remote.
// A remote named "deepseek" + model "deepseek/deepseek-v4-flash" →
// (deepseekRemote, "deepseek-v4-flash", true). The bare id is what the
// upstream OpenAI-compatible endpoint expects; the namespaced picker name
// is an oaica-only convention.
func findUserRemoteForModel(name string) (userRemote, string, bool) {
	idx := strings.Index(name, "/")
	if idx <= 0 {
		return userRemote{}, "", false
	}
	prefix := name[:idx]
	bare := name[idx+1:]
	remotes, err := loadUserRemotes()
	if err != nil {
		return userRemote{}, "", false
	}
	for _, r := range remotes {
		if r.Name == prefix {
			return r, bare, true
		}
	}
	return userRemote{}, "", false
}

// remoteBaseURL normalizes a remote's base_url to a form without a trailing
// "/" or "/v1" version prefix. Remotes are configured both ways — some include
// the OpenAI version prefix in base_url ("https://api.deepseek.com/v1"), some
// don't ("http://192.168.1.50:8080"). openAIBase appends "/<version>" exactly
// once, so a trailing /v1 here would produce /v1/v1/<endpoint> (404).
func remoteBaseURL(r userRemote) string {
	b := strings.TrimRight(strings.TrimSpace(r.BaseURL), "/")
	b = strings.TrimSuffix(b, "/v1")
	return b
}

// openAIBase returns the remote's full OpenAI base including its API version
// prefix, the value endpoint URLs are appended to. Almost all OpenAI-compatible
// endpoints version under "v1" ("https://api.deepseek.com/v1/chat/completions");
// z.ai versions under "v4" ("https://api.z.ai/api/paas/v4/chat/completions").
// The version defaults to "v1"; set userRemote.Version to override.
func (r userRemote) openAIBase() string {
	v := strings.Trim(strings.TrimSpace(r.Version), "/")
	if v == "" {
		v = "v1"
	}
	return remoteBaseURL(r) + "/" + v
}

// fetchRemoteModels lists one remote's models endpoint (e.g. /v1/models).
// Short timeout: a sleeping box must not stall the picker.
func fetchRemoteModels(r userRemote) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, r.openAIBase()+"/models", nil)
	if err != nil {
		return nil, err
	}
	if k := r.key(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := (&http.Client{Timeout: 6 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", r.Name, resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(payload.Data))
	for _, d := range payload.Data {
		if id := strings.TrimSpace(d.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// userRemoteLaunchModels queries every configured remote and returns picker
// entries named "<remote>/<model>". Errors are returned alongside the models,
// never instead of them.
func userRemoteLaunchModels() ([]LaunchModel, []error) {
	remotes, err := loadUserRemotes()
	if err != nil {
		return nil, []error{err}
	}
	var (
		models []LaunchModel
		errs   []error
	)
	for _, r := range remotes {
		ids, ferr := fetchRemoteModels(r)
		if ferr != nil {
			errs = append(errs, ferr)
			continue
		}
		for _, id := range ids {
			// Namespaced so two boxes serving the same model stay distinct,
			// and so the picker shows WHERE a model runs.
			display := id
			if i := strings.LastIndex(display, "/"); i >= 0 {
				display = display[i+1:] // llama-server reports a FILE PATH
			}
			display = strings.TrimSuffix(display, ".gguf")
			models = append(models, LaunchModel{
				Name:   r.Name + "/" + display,
				Remote: true,
			}.WithCloudLimits())
		}
	}
	return models, errs
}
