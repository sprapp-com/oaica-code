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

// loadUserRemotes returns the configured remotes. A missing file is normal and
// yields no error — most users have none.
func loadUserRemotes() ([]userRemote, error) {
	path := userRemotesPath()
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
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
	return out, nil
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
// don't ("http://192.168.1.50:8080"). Callers append "/v1/<endpoint>" exactly
// once, so a trailing /v1 here would produce /v1/v1/<endpoint> (404).
func remoteBaseURL(r userRemote) string {
	b := strings.TrimRight(strings.TrimSpace(r.BaseURL), "/")
	b = strings.TrimSuffix(b, "/v1")
	return b
}

// fetchRemoteModels lists one remote's /v1/models. Short timeout: a sleeping
// box must not stall the picker.
func fetchRemoteModels(r userRemote) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, remoteBaseURL(r)+"/v1/models", nil)
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
