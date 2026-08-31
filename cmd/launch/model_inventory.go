package launch

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ollama/ollama/api"
	modelpkg "github.com/ollama/ollama/types/model"
)

// LaunchModel is the model metadata Launch passes to integration config
// writers after resolving selected model names through the per-run inventory.
type LaunchModel struct {
	Name            string
	Remote          bool
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

	// LOCAL models first, and independently of the router. A self-hosted user
	// may have no router at all; a hosted user's router may be down. Neither
	// should empty the picker -- previously ANY router error set i.models=nil,
	// so a dead origin (or a missing key) hid every locally pulled model too.
	models := make([]LaunchModel, 0, 8)
	seen := make(map[string]bool)
	if i.client != nil {
		if lst, lerr := i.client.List(ctx); lerr == nil && lst != nil {
			for _, m := range lst.Models {
				lm := launchModelFromListResponse(m)
				if lm.Name == "" || seen[lm.Name] {
					continue
				}
				// Already-namespaced ids (hf.co/..., "<remote>/<id>",
				// ":local" tags, anything with a "/") keep their name; bare
				// daemon models get the ollama/ picker prefix.
				if !strings.Contains(lm.Name, "/") && !strings.HasSuffix(lm.Name, ":local") {
					lm.Name = ollamaPickerPrefix + lm.Name
				}
				seen[lm.Name] = true
				models = append(models, lm)
			}
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
		seen[e.ID] = true
		models = append(models, LaunchModel{Name: e.ID, Remote: true}.WithCloudLimits())
	}

	i.models = models
	i.err = nil
	i.loaded = true

	return cloneLaunchModels(i.models), i.err
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
