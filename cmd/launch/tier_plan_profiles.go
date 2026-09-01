package launch

// tier_plan_profiles.go — named tier profiles ("plans"): one flag,
// `--plan <name>`, resolves to a full opus/sonnet split the same way
// `--model X --sonnet-model Y` already does, without the caller needing to
// remember or type both model ids every launch. This is the "own
// /opusplan" the product needs: Claude Code's own --model opusplan picks
// (Opus, Sonnet) from Anthropic's catalog by a fixed name; a plan here
// picks (PrimaryModel, SonnetModel) from OUR catalog (self-hosted OAICA
// SKUs, user remotes, or router models) by a name we define.
//
// Plans live at ~/.oaica/plans.json, same directory/atomic-write
// convention as model_manifest.go's models.json, kept in a separate file
// because a plan is a preference (which models to send opus/sonnet-tier
// requests to) and a manifest entry is a fact (what a model actually is)
// — mixing them would force "add a model" and "define a plan" into one
// error-prone edit.
//
// Resolution happens entirely in extractPlanFlag, upstream of
// buildTierPlan: a plan is just a named shortcut for --model/--sonnet-model,
// so every downstream mechanism (proxy routing, tool-format gating, context
// probing) is reused unchanged — no new code path to keep in sync.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxPlanNameLength bounds a plan name entering plans.json (see PlanSet).
const maxPlanNameLength = 64

// TierPlanProfile is one named plan: which model serves Opus/Haiku-tier
// requests, and (optionally) which model serves Sonnet/subagent-tier
// requests. Empty SonnetModel means "same as Model", matching
// buildTierPlan's own default when --sonnet-model is omitted.
type TierPlanProfile struct {
	Model       string `json:"model"`
	SonnetModel string `json:"sonnet_model,omitempty"`
	// HaikuModel extends the schema (2026-09-02) the same way SonnetModel
	// did: optional, missing = empty = today's default (Haiku pinned to
	// Model), so plan files written before this change load unchanged.
	HaikuModel string `json:"haiku_model,omitempty"`
	// OversizeModel and RoutePolicy extend the schema (2026-08-31) the same
	// way the launch flags did: both optional, missing = empty = today's
	// defaults (no oversize leg, local-first policy), so plan files written
	// before this change load unchanged.
	OversizeModel string `json:"oversize_model,omitempty"`
	RoutePolicy   string `json:"route_policy,omitempty"`
	Description   string `json:"description,omitempty"`
}

type tierPlanProfiles struct {
	Version  int                        `json:"version"`
	Profiles map[string]TierPlanProfile `json:"profiles"`
	// LastUsed is the plan name the wizard last saved (empty in files
	// written before the field existed) — the plan-save prompt's
	// Enter-to-reuse default. Kept (and still written) for files before
	// LastUsedByRepo existed; PlanLastUsed now prefers the per-repo entry.
	LastUsed string `json:"last_used,omitempty"`
	// LastUsedByRepo keys the wizard's Enter-to-reuse default by the
	// directory the launch ran from, so repo A's last-saved plan is not
	// offered as repo B's default. Profiles themselves stay global — a plan
	// name is still resolvable everywhere via --plan — only the default
	// is scoped.
	LastUsedByRepo map[string]string `json:"last_used_by_repo,omitempty"`
}

const tierPlanProfilesVersion = 1

func tierPlanProfilesPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "plans.json"), nil
}

// TierPlanProfilesPath is the exported form of tierPlanProfilesPath, for
// cmd/cmd.go's CLI to print in "no plans defined" messages.
func TierPlanProfilesPath() (string, error) {
	return tierPlanProfilesPath()
}

func loadTierPlanProfiles() (*tierPlanProfiles, error) {
	path, err := tierPlanProfilesPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &tierPlanProfiles{Version: tierPlanProfilesVersion, Profiles: map[string]TierPlanProfile{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var p tierPlanProfiles
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if p.Profiles == nil {
		p.Profiles = map[string]TierPlanProfile{}
	}
	return &p, nil
}

func (p *tierPlanProfiles) save() error {
	path, err := tierPlanProfilesPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if p.Version == 0 {
		p.Version = tierPlanProfilesVersion
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// Chmod after the fact too: the 0o600 mode arg only applies at creation,
	// and a pre-existing looser file (audit 2026-09-01) stayed world-readable.
	return os.Chmod(path, 0o600)
}

// PlanSet creates or replaces a named plan.
func PlanSet(name string, profile TierPlanProfile) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("plan name is required")
	}
	// Bound the name: it becomes a map key / CLI argument everywhere
	// (`--plan <name>`), and the wizard prompt admits arbitrary length —
	// unbounded into plans.json, one fat-fingered paste would corrupt
	// every future plan listing.
	if len(name) > maxPlanNameLength {
		return fmt.Errorf("plan name %d chars exceeds the %d-char limit", len(name), maxPlanNameLength)
	}
	if strings.TrimSpace(profile.Model) == "" {
		return errors.New("--model is required")
	}
	if profile.RoutePolicy != "" {
		if _, err := parseRoutePolicy(profile.RoutePolicy); err != nil {
			return fmt.Errorf("route_policy %q is not one of local-first, remote-first, auto, local-only, remote-only", profile.RoutePolicy)
		}
	}
	p, err := loadTierPlanProfiles()
	if err != nil {
		return err
	}
	if p.Profiles == nil {
		p.Profiles = map[string]TierPlanProfile{}
	}
	p.Profiles[name] = profile
	p.LastUsed = name
	if cwd, err := os.Getwd(); err == nil {
		if p.LastUsedByRepo == nil {
			p.LastUsedByRepo = map[string]string{}
		}
		p.LastUsedByRepo[cwd] = name
	}
	return p.save()
}

// PlanLastUsed returns the plan name to offer as the wizard's
// Enter-to-reuse default: the last plan saved FROM THIS DIRECTORY (plans
// are per-repo in practice — each repo's launches share a model split
// that another repo's shouldn't inherit). Falls back to the legacy global
// LastUsed when no per-repo entry exists yet (or getwd failed), so files
// written before LastUsedByRepo keep their old behavior.
func PlanLastUsed() (string, error) {
	p, err := loadTierPlanProfiles()
	if err != nil {
		return "", err
	}
	name := p.LastUsed
	if cwd, err := os.Getwd(); err == nil {
		if n, ok := p.LastUsedByRepo[cwd]; ok {
			name = n
		}
	}
	if _, ok := p.Profiles[name]; !ok {
		return "", nil // removed since; no stale Enter default
	}
	return name, nil
}

// PlanRemove deletes a named plan, reporting whether it existed.
func PlanRemove(name string) (bool, error) {
	p, err := loadTierPlanProfiles()
	if err != nil {
		return false, err
	}
	if _, ok := p.Profiles[name]; !ok {
		return false, nil
	}
	delete(p.Profiles, name)
	return true, p.save()
}

// PlanGet resolves a named plan, or an error naming the plans file if not found.
func PlanGet(name string) (TierPlanProfile, error) {
	p, err := loadTierPlanProfiles()
	if err != nil {
		return TierPlanProfile{}, err
	}
	prof, ok := p.Profiles[name]
	if !ok {
		path, _ := tierPlanProfilesPath()
		return TierPlanProfile{}, fmt.Errorf("no plan named %q in %s — create one with `oaica plan set %s --model <id>`", name, path, name)
	}
	return prof, nil
}

// PlanSortedNames returns plan names in stable alphabetical order.
func PlanSortedNames() ([]string, error) {
	p, err := loadTierPlanProfiles()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(p.Profiles))
	for n := range p.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// extractPlanFlag pulls a launcher-level "--plan <name>" (or "--plan=<name>")
// out of the passthrough args, same convention as extractSonnetModel. Not
// forwarded to the child claude binary.
func extractPlanFlag(args []string) (plan string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--plan" && i+1 < len(args):
			plan = args[i+1]
			i++
		case strings.HasPrefix(a, "--plan="):
			plan = strings.TrimPrefix(a, "--plan=")
		default:
			rest = append(rest, a)
		}
	}
	return plan, rest
}

// resolvePlanModels turns a --plan name into (model, sonnetModel), applying
// it over whatever --model/--sonnet-model already resolved to. An explicit
// --model always wins over a plan's Model (an explicit flag is a stronger
// signal than a stored default) — but if the caller passed no model at all
// (model == ""), the plan supplies one. --sonnet-model, if explicitly
// passed, always wins over the plan's SonnetModel the same way.
func resolvePlanModels(planName, model, sonnetModel, haikuModel string) (resolvedModel, resolvedSonnet, resolvedHaiku string, err error) {
	if planName == "" {
		return model, sonnetModel, haikuModel, nil
	}
	prof, err := PlanGet(planName)
	if err != nil {
		return "", "", "", err
	}
	resolvedModel = model
	if resolvedModel == "" {
		resolvedModel = prof.Model
	}
	resolvedSonnet = sonnetModel
	if resolvedSonnet == "" {
		resolvedSonnet = prof.SonnetModel
	}
	resolvedHaiku = haikuModel
	if resolvedHaiku == "" {
		resolvedHaiku = prof.HaikuModel
	}
	return resolvedModel, resolvedSonnet, resolvedHaiku, nil
}

// resolvePlanTier layers a plan's stored oversize model + route policy over
// the launch flags, mirroring resolvePlanModels' precedence: an explicit
// flag always wins, the plan only fills what the caller left empty. With no
// --plan (or a plan stored before these fields existed) everything passes
// through unchanged, so the downstream tierPlan wiring in tier_routing.go's
// Run — which already applies flags > remotes.json route_policy >
// local-first — behaves byte-identically for old plans.
func resolvePlanTier(planName, policyArg, oversizeModel string) (resolvedPolicy, resolvedOversize string, err error) {
	if planName == "" {
		return policyArg, oversizeModel, nil
	}
	prof, err := PlanGet(planName)
	if err != nil {
		return "", "", err
	}
	resolvedPolicy = policyArg
	if resolvedPolicy == "" {
		resolvedPolicy = prof.RoutePolicy
	}
	resolvedOversize = oversizeModel
	if resolvedOversize == "" {
		resolvedOversize = prof.OversizeModel
	}
	return resolvedPolicy, resolvedOversize, nil
}
