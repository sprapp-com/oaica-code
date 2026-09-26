package launch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ollama/ollama/cmd/internal/fileutil"
	"github.com/ollama/ollama/envconfig"
)

// Droid implements Runner and Editor for Droid integration
type Droid struct{}

// droidSettings represents the Droid settings.json file (only fields we use)
type droidSettings struct {
	CustomModels           []modelEntry    `json:"customModels"`
	SessionDefaultSettings sessionSettings `json:"sessionDefaultSettings"`
}

type sessionSettings struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
}

type modelEntry struct {
	Model           string `json:"model"`
	DisplayName     string `json:"displayName"`
	BaseURL         string `json:"baseUrl"`
	APIKey          string `json:"apiKey"`
	Provider        string `json:"provider"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
	SupportsImages  bool   `json:"supportsImages"`
	ID              string `json:"id"`
	Index           int    `json:"index"`
}

func (d *Droid) String() string { return "Droid" }

func (d *Droid) Run(model string, _ []LaunchModel, args []string) error {
	forceTools, args := extractForceTools(args)
	if err := gateOpenAITools(model, forceTools); err != nil {
		return err
	}

	if _, err := exec.LookPath("droid"); err != nil {
		return fmt.Errorf("droid is not installed, install from https://docs.factory.ai/cli/getting-started/quickstart")
	}

	cmd := exec.Command("droid", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (d *Droid) Paths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	p := filepath.Join(home, ".factory", "settings.json")
	if _, err := os.Stat(p); err == nil {
		return []string{p}
	}
	return nil
}

func (d *Droid) Edit(models []LaunchModel) error {
	if len(models) == 0 {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	settingsPath := filepath.Join(home, ".factory", "settings.json")
	// ~/.factory/settings.json is Factory's own document and oaica rewrites it
	// whole from a snapshot of it, so the read, the merge and the publish run
	// under that store's lock: read outside and two overlapping
	// `oaica launch droid` commands each publish a snapshot taken before the
	// other's entries landed, and the launch that renames last silently decides
	// the file while both report success. The store belongs to another program,
	// so the lock is keyed under ~/.oaica/locks (foreignStoreLockBase) rather
	// than dropped inside ~/.factory (2026-09-26 audit, thirteenth round).
	return fileutil.WithFileLock(foreignStoreLockBase(settingsPath), func() error {
		return writeDroidSettings(settingsPath, models)
	})
}

// writeDroidSettings is the load-mutate-save half of Droid.Edit, run under the
// store's lock.
func writeDroidSettings(settingsPath string, models []LaunchModel) error {
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return err
	}

	// Read file once, unmarshal twice:
	// map preserves unknown fields for writing back (including extra fields in model entries)
	settingsMap := make(map[string]any)
	var settings droidSettings
	if data, err := os.ReadFile(settingsPath); err == nil {
		// UseNumber: the map is written back whole below, and the fields it
		// carries that oaica does not model are the user's own. Decoding into
		// map[string]any makes every number in it a float64, so an integer
		// above 2^53 comes back as a different number and 1.0 comes back as 1
		// (2026-09-26 audit, tenth round).
		// decodeJSONObject, not a bare Decode: a document that IS `null`
		// decodes into a nil map, which the write below then panics on
		// (2026-09-27 audit, round 19).
		doc, derr := decodeJSONObject(data)
		if derr != nil {
			return fmt.Errorf("failed to parse settings file: %w, at: %s", derr, settingsPath)
		}
		settingsMap = doc
		json.Unmarshal(data, &settings) // ignore error, zero values are fine
	}

	settingsMap = updateDroidSettings(settingsMap, settings, models)

	data, err := json.MarshalIndent(settingsMap, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteWithBackup(settingsPath, data, "droid")
}

func updateDroidSettings(settingsMap map[string]any, settings droidSettings, models []LaunchModel) map[string]any {
	// Keep only the models this integration did NOT write — the user's own
	// entries, extra fields intact. Everything oaica wrote is rebuilt below, so
	// a re-launch REPLACES the entry it owns instead of appending a second copy
	// of it (2026-09-26 audit, tenth round).
	//
	// An owned entry's own extra fields are kept too, under its picker name:
	// the rebuild below writes a fixed struct, so a model oaica registered and
	// the user then tuned in Droid's UI came back with every field Droid had
	// added to that entry — temperature, reasoning effort, whatever the app
	// writes — silently gone, on a re-launch the user did not ask to change
	// anything (2026-09-26 audit, round 16). This is what writeDroidSettings'
	// comment ("map preserves unknown fields for writing back (including extra
	// fields in model entries)") already claimed and only the foreign path did.
	var foreignModels []any
	ownedByPicker := make(map[string]map[string]any)
	if rawModels, ok := settingsMap["customModels"].([]any); ok {
		for _, raw := range rawModels {
			m, ok := raw.(map[string]any)
			if !ok {
				continue // malformed entry: nothing to preserve or rebuild
			}
			picker, owned := droidOwnedEntry(droidString(m["apiKey"]), droidString(m["id"]), droidString(m["model"]), droidString(m["baseUrl"]))
			if owned {
				if picker != "" {
					ownedByPicker[picker] = m
				}
				continue
			}
			foreignModels = append(foreignModels, raw)
		}
	}

	// Build new Ollama model entries with sequential indices (0, 1, 2, ...)

	var newModels []any
	var defaultModelID string
	for i, model := range models {
		maxOutput := 64000
		if model.MaxOutputTokens > 0 {
			maxOutput = model.MaxOutputTokens
		}
		modelID := fmt.Sprintf("custom:%s-%d", model.Name, i)
		entry := modelEntry{
			Model:           model.Name,
			DisplayName:     model.Name,
			BaseURL:         envconfig.ConnectableHost().String() + "/v1",
			APIKey:          droidDaemonKey,
			Provider:        "generic-chat-completion-api",
			MaxOutputTokens: maxOutput,
			SupportsImages:  model.HasCapability("vision"),
			ID:              modelID,
			Index:           i,
		}
		if ep, ok := resolveRemoteEndpoint(model.Name); ok {
			entry.Model = ep.UpstreamModel
			entry.DisplayName = ep.UpstreamModel
			entry.BaseURL = ep.BaseURL
			entry.APIKey = ep.Token
		}
		// Fields this file does not model, from the entry it is replacing: the
		// rebuild owns the fields above, and everything else the app put in
		// that entry stays.
		entryMap := droidEntryMap(entry)
		for k, v := range ownedByPicker[model.Name] {
			if _, isOurs := entryMap[k]; !isOurs {
				entryMap[k] = v
			}
		}
		newModels = append(newModels, entryMap)
		if i == 0 {
			defaultModelID = modelID
		}
	}

	settingsMap["customModels"] = append(newModels, foreignModels...)

	// Update session default settings (preserve unknown fields in the nested object)
	sessionSettings, ok := settingsMap["sessionDefaultSettings"].(map[string]any)
	if !ok {
		sessionSettings = make(map[string]any)
	}
	sessionSettings["model"] = defaultModelID

	if !isValidReasoningEffort(settings.SessionDefaultSettings.ReasoningEffort) {
		sessionSettings["reasoningEffort"] = "none"
	}

	settingsMap["sessionDefaultSettings"] = sessionSettings
	return settingsMap
}

func (d *Droid) Models() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	data, err := os.ReadFile(filepath.Join(home, ".factory", "settings.json"))
	if err != nil {
		return nil
	}

	var settings droidSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil
	}

	// Report the picker names of the entries this integration wrote — the names
	// the launcher saves and compares the live config against. The stored
	// "model" is only that name for a daemon entry: a user-remote entry stores
	// the bare upstream id, so reporting it verbatim would answer with a name
	// nothing in the launcher recognises, and every launch would look like an
	// unconfigured one.
	var result []string
	for _, m := range settings.CustomModels {
		if picker, owned := droidOwnedEntry(m.APIKey, m.ID, m.Model, m.BaseURL); owned && picker != "" {
			result = append(result, picker)
		}
	}
	return result
}

// droidEntryMap renders a built entry as a map, so fields this file does not
// model can be merged over it without a second copy of modelEntry's JSON
// shape. UseNumber, like writeDroidSettings' read: the round-trip must not turn
// an integer into a float64.
func droidEntryMap(entry modelEntry) map[string]any {
	m := make(map[string]any, 9)
	b, err := json.Marshal(entry)
	if err != nil {
		return m
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return map[string]any{}
	}
	return m
}

// droidDaemonKey is the apiKey this file writes for a model served by the local
// daemon. Droid sends apiKey as the request's bearer, so it is only a marker as
// long as nothing has to replace it — the daemon is unauthenticated, and a user
// remote is not.
const droidDaemonKey = "ollama"

// droidOwnedEntry reports whether an existing customModels entry was written by
// this integration (and may therefore be rebuilt), plus the picker name it was
// written for. An entry it does not recognise is the user's: it is preserved
// untouched, extra fields included.
//
// The daemon shape is recognised by its apiKey. A user-remote entry cannot be:
// it holds the remote's token, which Droid sends as the bearer, and a token is
// not a marker — it is replaced whenever it rotates, and an entry holding the
// PREVIOUS token would read as a foreign one and be kept beside the new copy.
// The pair above the credential identifies it instead: the id this file writes
// ("custom:<picker name>-<index>", the index deliberately ignored so a model
// keeps its identity when the list around it changes) names a picker, and the
// entry stores the endpoint that picker resolves to.
func droidOwnedEntry(apiKey, id, model, baseURL string) (string, bool) {
	if apiKey == droidDaemonKey {
		// "ollama" is also the apiKey a user types when they point Droid at
		// their OWN local server — it is what the tool's own docs suggest — so
		// the daemon marker alone does not make an entry ours. An entry with
		// that apiKey and an id this file did not write (or that names a
		// different model than it stores) is the user's, and a launch that does
		// not select it would otherwise DELETE it from their settings
		// (2026-09-27 audit, round 19). oaica's own daemon entries always carry
		// both: the id is "custom:<picker>-<index>" and the model is the picker
		// itself.
		picker := droidPickerFromID(id)
		if picker == "" || model == "" || picker != model {
			return "", false
		}
		return picker, true
	}
	picker := droidPickerFromID(id)
	if picker == "" || model == "" {
		return "", false
	}
	// The endpoint is matched against the configured remotes directly, NOT via
	// resolveRemoteEndpoint: a launch saved under a bare remote id
	// ("deepseek-chat") would send that resolver through its bare-id sweep of
	// every configured remote — once per stored entry, inside a config merge.
	remote, ok := droidRemoteForBase(baseURL)
	if !ok {
		return "", false
	}
	// The picker has to name that remote's model, in either spelling a launch
	// can save: "<remote>/<upstream>" for a namespaced picker, or the bare
	// upstream id itself.
	if prefix, bare, namespaced := strings.Cut(picker, "/"); namespaced {
		if prefix != remote.Name || bare != model {
			return "", false
		}
	} else if picker != model {
		return "", false
	}
	return picker, true
}

// droidLegacyIDSuffix is the segment the upstream `ollama config droid` put in
// front of the index — "custom:<picker name>-[Ollama]-<index>" — until upstream
// 771d9280e (2026-01-23) shortened it. Entries in that shape are in the wild,
// and they are ollama's own (its ownership test was a substring match on this
// marker), so they are recognised as oaica's and replaced rather than kept
// beside a second copy of the same model.
const droidLegacyIDSuffix = "-[Ollama]"

// droidPickerFromID reverses the id this file writes for a launched model —
// "custom:<picker name>-<index>", or the older "custom:<picker name>-[Ollama]-
// <index>" — and returns "" for an id it did not write. Only a trailing
// "-<digits>" (and the legacy marker before it) is stripped, so a name that
// ends in a number, or carries ":"/"-"/"/" of its own, round-trips.
func droidPickerFromID(id string) string {
	rest, ok := strings.CutPrefix(id, "custom:")
	if !ok {
		return ""
	}
	dash := strings.LastIndex(rest, "-")
	if dash <= 0 || dash == len(rest)-1 {
		return ""
	}
	for _, r := range rest[dash+1:] {
		if r < '0' || r > '9' {
			return ""
		}
	}
	name := strings.TrimSuffix(rest[:dash], droidLegacyIDSuffix)
	if name == "" {
		return ""
	}
	return name
}

// droidRemoteForBase returns the configured remote serving baseURL — the
// endpoint a stored entry names.
func droidRemoteForBase(baseURL string) (userRemote, bool) {
	remotes, err := loadUserRemotes()
	if err != nil {
		return userRemote{}, false
	}
	for _, r := range remotes {
		if droidSameURL(r.openAIBase(), baseURL) {
			return r, true
		}
	}
	return userRemote{}, false
}

// droidSameURL compares two endpoint URLs, ignoring a trailing "/" — the one
// difference a hand-edited base_url picks up for free (loadUserRemotes trims
// the stored value; openAIBase appends a version segment to it).
func droidSameURL(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}

// droidString is the string value of a field read back from settings.json, or
// "" for a missing/other-typed one.
func droidString(v any) string {
	s, _ := v.(string)
	return s
}

var validReasoningEfforts = []string{"high", "medium", "low", "none"}

func isValidReasoningEffort(effort string) bool {
	return slices.Contains(validReasoningEfforts, effort)
}
