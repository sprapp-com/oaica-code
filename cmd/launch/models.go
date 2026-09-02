package launch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/config"
	"github.com/ollama/ollama/envconfig"
	"github.com/ollama/ollama/format"
	internalcloud "github.com/ollama/ollama/internal/cloud"
	"github.com/ollama/ollama/internal/modelref"
	"github.com/ollama/ollama/progress"
)

// ollamaCloudAliasCatalog is upstream Ollama's built-in launch catalog: the
// ":cloud" aliases Ollama sells on its own hosted service, plus two local
// pulls. This fork does NOT sell Ollama cloud — the picker is sourced live
// from the OAICA router (/v1/models), user remotes (~/.oaica/remotes.json),
// OpenRouter and the local daemon (see launcherClient.requestRecommendations
// and modelInventory.load). These entries are therefore NEVER offered as
// recommendations or picker rows. The slice is kept only to seed
// cloudModelLimits, so a genuine Ollama ":cloud" alias that a user has on
// their own local daemon still resolves its context/output limits
// (WithCloudLimits, CLAUDE_CODE_AUTO_COMPACT_WINDOW in tierPlan.envVars).
var ollamaCloudAliasCatalog = []ModelItem{
	{Name: "kimi-k2.6:cloud", Description: "State-of-the-art coding, long-horizon execution, and multimodal agent swarm capability", Recommended: true, Details: api.ModelDetails{ContextLength: 262_144}, MaxOutputTokens: 262_144},
	{Name: "qwen3.5:cloud", Description: "Reasoning, coding, and agentic tool use with vision", Recommended: true, Details: api.ModelDetails{ContextLength: 262_144}, MaxOutputTokens: 32_768},
	{Name: "glm-5.1:cloud", Description: "Reasoning and code generation", Recommended: true, Details: api.ModelDetails{ContextLength: 202_752}, MaxOutputTokens: 131_072},
	{Name: "minimax-m2.7:cloud", Description: "Fast, efficient coding and real-world productivity", Recommended: true, Details: api.ModelDetails{ContextLength: 204_800}, MaxOutputTokens: 128_000},
	{Name: "gemma4", Description: "Reasoning and code generation locally", Recommended: true, VRAMBytes: 12 * format.GigaByte},
	{Name: "qwen3.5", Description: "Reasoning, coding, and visual understanding locally", Recommended: true, VRAMBytes: 14 * format.GigaByte},
}

func displayVRAM(vramBytes int64) string {
	if vramBytes <= 0 {
		return ""
	}
	gb := float64(vramBytes) / format.GigaByte
	if gb == math.Trunc(gb) {
		return fmt.Sprintf("~%.0fGB", gb)
	}
	return fmt.Sprintf("~%.1fGB", gb)
}

// cloudModelLimit holds context and output token limits for a cloud model.
type cloudModelLimit struct {
	Context int
	Output  int
}

// extraCloudModelLimits maps Ollama cloud alias base names to token limits for
// aliases that are not already covered by ollamaCloudAliasCatalog.
// TODO(parthsareen): grab context/output limits from model info instead of hardcoding
var extraCloudModelLimits = map[string]cloudModelLimit{
	"cogito-2.1:671b":     {Context: 163_840, Output: 65_536},
	"deepseek-v3.1:671b":  {Context: 163_840, Output: 163_840},
	"deepseek-v3.2":       {Context: 163_840, Output: 65_536},
	"gemma4:31b":          {Context: 262_144, Output: 131_072},
	"glm-4.6":             {Context: 202_752, Output: 131_072},
	"glm-4.7":             {Context: 202_752, Output: 131_072},
	"glm-5":               {Context: 202_752, Output: 131_072},
	"glm-5.1":             {Context: 202_752, Output: 131_072},
	"glm-5.3-flash":       {Context: 202_752, Output: 131_072},
	"gpt-oss:120b":        {Context: 131_072, Output: 131_072},
	"gpt-oss:20b":         {Context: 131_072, Output: 131_072},
	"kimi-k2:1t":          {Context: 262_144, Output: 262_144},
	"kimi-k2.5":           {Context: 262_144, Output: 262_144},
	"kimi-k2.6":           {Context: 262_144, Output: 262_144},
	"kimi-k2-thinking":    {Context: 262_144, Output: 262_144},
	"nemotron-3-nano:30b": {Context: 1_048_576, Output: 131_072},
	"qwen3-coder:480b":    {Context: 262_144, Output: 65_536},
	"qwen3-coder-next":    {Context: 262_144, Output: 32_768},
	"qwen3-next:80b":      {Context: 262_144, Output: 32_768},
	"qwen3.5":             {Context: 262_144, Output: 32_768},
}

// cloudModelLimits holds the Ollama cloud alias limits: context/output token
// limits keyed by the base name of an Ollama ":cloud" alias. It is a lookup
// table only — nothing here is a recommendation, and lookupCloudModelLimit
// only consults it for names carrying an explicit cloud source tag, so OAICA
// router / user-remote model ids never match it.
var cloudModelLimits = mergeCloudModelLimits(cloudModelLimitsFromRecommendations(ollamaCloudAliasCatalog), extraCloudModelLimits)

var (
	dynamicCloudModelLimitsMu sync.RWMutex
	dynamicCloudModelLimits   = map[string]cloudModelLimit{}
)

// lookupCloudModelLimit returns the token limits for a cloud model.
// It normalizes explicit cloud source suffixes before checking the shared limit map.
func lookupCloudModelLimit(name string) (cloudModelLimit, bool) {
	base, stripped := modelref.StripCloudSourceTag(name)
	if stripped {
		dynamicCloudModelLimitsMu.RLock()
		l, ok := dynamicCloudModelLimits[base]
		dynamicCloudModelLimitsMu.RUnlock()
		if ok {
			return l, true
		}
		if l, ok := cloudModelLimits[base]; ok {
			return l, true
		}
	}
	return cloudModelLimit{}, false
}

func setDynamicCloudModelLimits(limits map[string]cloudModelLimit) {
	dynamicCloudModelLimitsMu.Lock()
	defer dynamicCloudModelLimitsMu.Unlock()
	if limits == nil {
		dynamicCloudModelLimits = map[string]cloudModelLimit{}
		return
	}
	cp := make(map[string]cloudModelLimit, len(limits))
	for k, v := range limits {
		cp[k] = v
	}
	dynamicCloudModelLimits = cp
}

func cloudModelLimitsFromRecommendations(recommendations []ModelItem) map[string]cloudModelLimit {
	limits := make(map[string]cloudModelLimit, len(recommendations))
	for _, rec := range recommendations {
		if !isCloudModelName(rec.Name) || rec.Details.ContextLength <= 0 || rec.MaxOutputTokens <= 0 {
			continue
		}
		base, stripped := modelref.StripCloudSourceTag(rec.Name)
		if !stripped || base == "" {
			continue
		}
		limits[base] = cloudModelLimit{
			Context: rec.Details.ContextLength,
			Output:  rec.MaxOutputTokens,
		}
	}
	return limits
}

func mergeCloudModelLimits(base map[string]cloudModelLimit, overlay map[string]cloudModelLimit) map[string]cloudModelLimit {
	out := make(map[string]cloudModelLimit, len(base)+len(overlay))
	for name, limit := range base {
		out[name] = limit
	}
	for name, limit := range overlay {
		out[name] = limit
	}
	return out
}

// missingModelPolicy controls how model-not-found errors should be handled.
type missingModelPolicy int

const (
	// missingModelPromptPull prompts the user to download missing local models.
	missingModelPromptPull missingModelPolicy = iota
	// missingModelAutoPull downloads missing local models without prompting.
	missingModelAutoPull
	// missingModelFail returns an error for missing local models without prompting.
	missingModelFail
)

// OpenBrowser opens the URL in the user's browser.
func OpenBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", url).Start()
	case "linux":
		// Skip on headless systems where no display server is available
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		_ = exec.Command("xdg-open", url).Start()
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
}

// ensureAuth ensures the user is signed in before cloud-backed models run.
func ensureAuth(ctx context.Context, client *api.Client, cloudModels map[string]bool, selected []string) error {
	var selectedCloudModels []string
	for _, m := range selected {
		if cloudModels[m] {
			selectedCloudModels = append(selectedCloudModels, m)
		}
	}
	if len(selectedCloudModels) == 0 {
		return nil
	}
	return ensureCloudAuth(ctx, client, strings.Join(selectedCloudModels, ", "))
}

func ensureCloudAuth(ctx context.Context, client *api.Client, modelList string) error {
	if disabled, known := cloudStatusDisabled(ctx, client); known && disabled {
		return errors.New(internalcloud.DisabledError("remote inference is unavailable"))
	}

	user, err := whoamiWithTimeout(ctx, client)
	if err == nil && user != nil && user.Name != "" {
		return nil
	}

	var aErr api.AuthorizationError
	if err != nil && !errors.As(err, &aErr) {
		return nil
	}
	if err == nil || aErr.SigninURL == "" {
		return fmt.Errorf("%s requires sign in", modelList)
	}

	if DefaultSignIn != nil {
		_, err := DefaultSignIn(modelList, aErr.SigninURL)
		if errors.Is(err, ErrCancelled) {
			return ErrCancelled
		}
		if err != nil {
			return fmt.Errorf("%s requires sign in", modelList)
		}
		return nil
	}

	yes, err := ConfirmPrompt(fmt.Sprintf("sign in to use %s?", modelList))
	if errors.Is(err, ErrCancelled) {
		return ErrCancelled
	}
	if err != nil {
		return err
	}
	if !yes {
		return ErrCancelled
	}

	fmt.Fprintf(os.Stderr, "\nTo sign in, navigate to:\n    %s\n\n", aErr.SigninURL)
	OpenBrowser(aErr.SigninURL)

	spinnerFrames := []string{"|", "/", "-", "\\"}
	frame := 0
	fmt.Fprintf(os.Stderr, "\033[90mwaiting for sign in to complete... %s\033[0m", spinnerFrames[0])

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\r\033[K")
			return ctx.Err()
		case <-ticker.C:
			frame++
			fmt.Fprintf(os.Stderr, "\r\033[90mwaiting for sign in to complete... %s\033[0m", spinnerFrames[frame%len(spinnerFrames)])

			if frame%10 == 0 {
				u, err := whoamiWithTimeout(ctx, client)
				if err == nil && u != nil && u.Name != "" {
					fmt.Fprintf(os.Stderr, "\r\033[K\033[A\r\033[K\033[1msigned in:\033[0m %s\n", u.Name)
					return nil
				}
			}
		}
	}
}

// showOrPullWithPolicy checks if a model exists and applies the provided missing-model policy.
//
// OAICA models never need this: they're served live by the api.oaica.com
// router, not pulled into a local Ollama registry that doesn't exist in
// this thin-client fork. client.Show() always 404s for them (no local
// server), which used to fall through into a real pull attempt —
// "pulling manifest" / "pull model manifest: file does not exist" — for
// every single launch. A model got INTO the picker in the first place by
// being present in oaicaLiveModels() (or being a valid "+"-composite of
// one), which is the only readiness check that applies here.
func showOrPullWithPolicy(ctx context.Context, client *api.Client, model string, policy missingModelPolicy, isCloudModel bool) error {
	// User-defined alias (~/.oaica/aliases.json): resolve to its real
	// target before any readiness check, same as resolveLaunchEndpoint —
	// otherwise a bare alias name never matches oaicaModelIsReady/the
	// remote check below and falls through to a bogus "run ollama pull"
	// error even though the real target is perfectly launchable.
	if target, ok := resolveModelAlias(model); ok {
		model = target
	}
	if oaicaModelIsReady(model) {
		return nil
	}
	// A user-defined remote model (from ~/.oaica/remotes.json, surfaced as
	// "<remote>/<model>" in the picker) is served live by its own endpoint —
	// it is never pulled into the local Ollama registry this fork doesn't
	// run. Without this short-circuit, client.Show() 404s and the flow falls
	// into pullMissingModel → "pull model manifest: file does not exist".
	if _, _, ok := findUserRemoteForModel(model); ok {
		return nil
	}
	// Explicit source prefixes ("router/<id>", "ollama/<id>", ...) are
	// resolved by tier_routing.go; they are never local models to pull.
	// Caught on .91 2026-08-27: `--model router/kat-awq` reached the pull
	// path ("pull model manifest: file does not exist").
	if hasSourcePrefix(model) {
		if _, err := resolveLaunchEndpoint(model); err != nil {
			return err
		}
		return nil
	}
	if _, err := client.Show(ctx, &api.ShowRequest{Model: model}); err == nil {
		return nil
	} else {
		if isCloudModel {
			if disabled, known := cloudStatusDisabled(ctx, client); known && disabled {
				return errors.New(internalcloud.DisabledError("remote inference is unavailable"))
			}
			var statusErr api.StatusError
			if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
				return fmt.Errorf("model %q not found", model)
			}
			return nil
		}

		// Not a router model, not a user remote, and the local daemon
		// either doesn't have it (404) or isn't running at all. If the
		// router REJECTED our credential, that is the actionable cause: a
		// fresh install with no OAICA_API_KEY used to surface either a raw
		// "connection refused" or an Ollama registry pull dying with "pull
		// model manifest: file does not exist", both hiding the real fix.
		// A router that is merely unreachable still fails open (local
		// models must keep working offline).
		if _, rerr := oaicaFetchCloudModelEntries(); isOaicaRouterAuthErr(rerr) {
			return fmt.Errorf("%q is not a local model, and %s rejected the API key (%w)\nSet OAICA_API_KEY or run `oaica signin`", model, oaicaLaunchHost(), rerr)
		}

		var statusErr api.StatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
			// Transport-level failure: no local daemon is running. Say what
			// was tried instead of leaking a bare dial error.
			return fmt.Errorf("%q is not served by %s and no local server answered at %s: %w", model, oaicaLaunchHost(), envconfig.Host(), err)
		}
	}

	switch policy {
	case missingModelAutoPull:
		return pullMissingModel(ctx, client, model)
	case missingModelFail:
		return fmt.Errorf("model %q not found; run 'ollama pull %s' first, or use --yes to auto-pull", model, model)
	default:
		return confirmAndPull(ctx, client, model)
	}
}

func confirmAndPull(ctx context.Context, client *api.Client, model string) error {
	if ok, err := ConfirmPrompt(fmt.Sprintf("Download %s?", model)); err != nil {
		return err
	} else if !ok {
		return errCancelled
	}
	fmt.Fprintf(os.Stderr, "\n")
	return pullMissingModel(ctx, client, model)
}

func pullMissingModel(ctx context.Context, client *api.Client, model string) error {
	if err := pullModel(ctx, client, model, false); err != nil {
		return fmt.Errorf("failed to pull %s: %w", model, err)
	}
	return nil
}

// prepareEditorIntegration persists models and applies editor-managed config files.
func prepareEditorIntegration(name string, editor Editor, models []LaunchModel) error {
	if err := editor.Edit(models); err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}
	if err := config.SaveIntegration(name, launchModelNames(models)); err != nil {
		return fmt.Errorf("failed to save: %w", err)
	}
	return nil
}

func prepareManagedSingleIntegration(name string, managed ManagedSingleModel, model string, models []LaunchModel) error {
	var err error
	if withModels, ok := managed.(ManagedModelListConfigurer); ok {
		err = withModels.ConfigureWithModels(model, models)
	} else {
		err = managed.Configure(model)
	}
	if err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}
	if err := config.SaveIntegration(name, []string{model}); err != nil {
		return fmt.Errorf("failed to save: %w", err)
	}
	return nil
}

func prepareManagedAutodiscoveryIntegration(name string, autodiscovery ManagedAutodiscoveryIntegration, model string) error {
	if err := autodiscovery.ConfigureAutodiscovery(); err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}
	if err := config.SaveIntegration(name, []string{model}); err != nil {
		return fmt.Errorf("failed to save: %w", err)
	}
	return nil
}

// buildModelList merges existing models with upstream Ollama's built-in
// catalog for selection UIs.
//
// Not used by any launch flow in this fork: the picker goes through
// buildModelListWithRecommendations with the live OAICA/user-remote list from
// launcherClient.recommendations, which never falls back to
// ollamaCloudAliasCatalog. Kept so the upstream merge/sort tests keep
// exercising buildModelListWithRecommendations against a fixed fixture.
func buildModelList(existing []modelInfo, preChecked []string, current string) (items []ModelItem, orderedChecked []string, existingModels, cloudModels map[string]bool) {
	return buildModelListWithRecommendations(existing, ollamaCloudAliasCatalog, preChecked, current)
}

func buildModelListWithRecommendations(existing []modelInfo, recommendations []ModelItem, preChecked []string, current string) (items []ModelItem, orderedChecked []string, existingModels, cloudModels map[string]bool) {
	existingModels = make(map[string]bool)
	cloudModels = make(map[string]bool)
	recommended := make(map[string]bool)
	var hasLocalModel, hasCloudModel bool

	recDesc := make(map[string]string)
	recByName := make(map[string]ModelItem)
	for _, rec := range recommendations {
		recommended[rec.Name] = true
		recDesc[rec.Name] = rec.Description
		recByName[rec.Name] = rec
	}

	for _, m := range existing {
		existingModels[m.Name] = true
		if m.Remote {
			cloudModels[m.Name] = true
			hasCloudModel = true
		} else {
			hasLocalModel = true
		}
		displayName := strings.TrimSuffix(m.Name, ":latest")
		existingModels[displayName] = true
		if rec, ok := recByName[displayName]; ok {
			items = append(items, modelItemFromInventory(displayName, m, copyModelRecommendationFields(displayName, rec)))
		} else {
			items = append(items, modelItemFromInventory(displayName, m, ModelItem{Name: displayName, Recommended: recommended[displayName], Description: recDesc[displayName]}))
		}
	}

	for _, rec := range recommendations {
		if existingModels[rec.Name] || existingModels[rec.Name+":latest"] {
			continue
		}
		items = append(items, rec)
		if isCloudModelName(rec.Name) {
			cloudModels[rec.Name] = true
		}
	}

	checked := make(map[string]bool, len(preChecked))
	for _, n := range preChecked {
		checked[n] = true
	}
	// the ollama/ picker prefix is display-level: a checked/*/current name
	// must match its prefixed inventory entry too.
	isChecked := func(name string) bool {
		if checked[name] {
			return true
		}
		for _, n := range preChecked {
			if launchModelMatches(n, name) {
				return true
			}
		}
		return false
	}

	if current != "" {
		matchedCurrent := false
		for _, item := range items {
			if launchModelMatches(item.Name, current) {
				current = item.Name
				matchedCurrent = true
				break
			}
		}
		if !matchedCurrent {
			for _, item := range items {
				if strings.HasPrefix(item.Name, current+":") {
					current = item.Name
					break
				}
			}
		}
	}

	if isChecked(current) {
		preChecked = append([]string{current}, slices.DeleteFunc(preChecked, func(m string) bool {
			// launchModelMatches, not ==: the current name may carry the
			// ollama/ picker prefix while preChecked holds the bare id.
			return launchModelMatches(current, m)
		})...)
	}

	notInstalled := make(map[string]bool)
	for i := range items {
		if !existingModels[items[i].Name] && !cloudModels[items[i].Name] {
			notInstalled[items[i].Name] = true
			var parts []string
			if items[i].Description != "" {
				parts = append(parts, items[i].Description)
			}
			if vram := displayVRAM(items[i].VRAMBytes); vram != "" {
				parts = append(parts, vram)
			}
			parts = append(parts, "(not downloaded)")
			items[i].Description = strings.Join(parts, ", ")
		}
	}

	recRank := make(map[string]int)
	for i, rec := range recommendations {
		recRank[rec.Name] = i + 1
	}

	// Pin a user's most-frequently-picked models above everything else,
	// including the OAICA recommendation section — that's the whole point
	// of surfacing them (fastest path back to what you actually use).
	freqRank := make(map[string]int)
	for i, name := range topFrequentModels(5) {
		freqRank[name] = i + 1
	}
	for i := range items {
		if freqRank[items[i].Name] > 0 && !strings.Contains(items[i].Description, "frequently used") {
			if items[i].Description != "" {
				items[i].Description = "frequently used · " + items[i].Description
			} else {
				items[i].Description = "frequently used"
			}
		}
	}

	if hasLocalModel || hasCloudModel {
		// Keep the Recommended section pinned to recommendation order. Checked
		// and default-model priority only apply within the More section.
		slices.SortStableFunc(items, func(a, b ModelItem) int {
			ac, bc := isChecked(a.Name), isChecked(b.Name)
			aNew, bNew := notInstalled[a.Name], notInstalled[b.Name]
			aFreq, bFreq := freqRank[a.Name] > 0, freqRank[b.Name] > 0
			if aFreq != bFreq {
				if aFreq {
					return -1
				}
				return 1
			}
			if aFreq && bFreq {
				return freqRank[a.Name] - freqRank[b.Name]
			}
			aRec, bRec := recRank[a.Name] > 0, recRank[b.Name] > 0
			if aRec != bRec {
				if aRec {
					return -1
				}
				return 1
			}
			if aRec && bRec {
				return recRank[a.Name] - recRank[b.Name]
			}
			if ac != bc {
				if ac {
					return -1
				}
				return 1
			}
			// Among checked non-recommended items - put the default first
			if ac && !aRec && current != "" {
				aCurrent := a.Name == current
				bCurrent := b.Name == current
				if aCurrent != bCurrent {
					if aCurrent {
						return -1
					}
					return 1
				}
			}
			if aNew != bNew {
				if aNew {
					return 1
				}
				return -1
			}
			return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		})
	}

	return items, preChecked, existingModels, cloudModels
}

func copyModelRecommendationFields(name string, rec ModelItem) ModelItem {
	rec.Name = name
	rec.Recommended = true
	return rec
}

func modelItemFromInventory(name string, info modelInfo, item ModelItem) ModelItem {
	item.Name = name
	item.ToolCapable = info.ToolCapable
	item.Capabilities = slices.Clone(info.Capabilities)
	item.Size = info.Size
	item.Details = info.Details
	item.Remote = info.Remote
	// OpenRouter's zero-cost models carry the ":free" suffix; label them
	// inline so they're spottable in the picker without opening the
	// pricing page (an existing description gets the tag appended).
	if strings.HasSuffix(name, ":free") && !strings.Contains(item.Description, "free") {
		if item.Description == "" {
			item.Description = "(free)"
		} else {
			item.Description += " (free)"
		}
	}
	if label := billingPlanLabel(name); label != "" && item.Description == "" {
		item.Description = label
	}
	return item
}

// billingPlanLabel tags a picker row with how it's billed, when the model id
// carries a known GLM/Z.AI naming pattern: distinguishes a shared aggregator
// key's flat-rate "Coding Plan" (all of zen's traffic rides one subscription,
// regardless of which model you pick) from a direct per-token Z.AI API key
// (2026-09-02 — the user's own opencode picker shows this distinction native;
// ours had none, so a GLM row here looked billing-agnostic when it isn't).
// Bare "<remote>/glm-*" names are the zen/opencode-go aggregator; an explicit
// "zai/glm-*" (a direct Z.AI remote, once one is configured in remotes.json)
// is the API-key path instead.
func billingPlanLabel(name string) string {
	rest, ok := strings.CutPrefix(name, "opencode-go/")
	if ok && strings.HasPrefix(rest, "glm-") {
		return "Coding Plan (zen — shared subscription)"
	}
	if rest, ok := strings.CutPrefix(name, "zai/"); ok && strings.HasPrefix(rest, "glm-") {
		return "API Plan (Z.AI — per-token key)"
	}
	// moonshot (Kimi) and minimax are built-in catalog providers
	// (user_remotes.go's catalogProviders) — same per-token-key auth as
	// zai above, just a different vendor. Labeled the same way so a
	// "kimi-k3"/minimax row in the picker doesn't look billing-agnostic
	// (2026-09-03, requested alongside the zai label already here).
	if _, ok := strings.CutPrefix(name, "moonshot/"); ok {
		return "API Plan (Moonshot/Kimi — per-token key)"
	}
	if _, ok := strings.CutPrefix(name, "minimax/"); ok {
		return "API Plan (MiniMax — per-token key)"
	}
	return ""
}

// isCloudModelName reports whether the model name has an explicit cloud source.
func isCloudModelName(name string) bool {
	return modelref.HasExplicitCloudSource(name)
}

// filterCloudItems removes cloud models from selection items.
func filterCloudItems(items []ModelItem) []ModelItem {
	filtered := items[:0]
	for _, item := range items {
		if !isCloudModelName(item.Name) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func isCloudModel(ctx context.Context, client *api.Client, name string) bool {
	if client == nil {
		return false
	}
	resp, err := client.Show(ctx, &api.ShowRequest{Model: name})
	if err != nil {
		return false
	}
	return resp.RemoteModel != ""
}

// cloudStatusDisabled returns whether cloud usage is currently disabled.
func cloudStatusDisabled(ctx context.Context, client *api.Client) (disabled bool, known bool) {
	status, err := client.CloudStatusExperimental(ctx)
	if err != nil {
		var statusErr api.StatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
			return false, false
		}
		return false, false
	}
	return status.Cloud.Disabled, true
}

// TODO(parthsareen): this duplicates the pull progress UI in cmd.PullHandler.
// Move the shared pull rendering to a small utility once the package boundary settles.
func pullModel(ctx context.Context, client *api.Client, model string, insecure bool) error {
	p := progress.NewProgress(os.Stderr)
	defer p.Stop()

	bars := make(map[string]*progress.Bar)
	var status string
	var spinner *progress.Spinner

	fn := func(resp api.ProgressResponse) error {
		if resp.Digest != "" {
			if resp.Completed == 0 {
				return nil
			}

			if spinner != nil {
				spinner.Stop()
			}

			bar, ok := bars[resp.Digest]
			if !ok {
				name, isDigest := strings.CutPrefix(resp.Digest, "sha256:")
				name = strings.TrimSpace(name)
				if isDigest {
					name = name[:min(12, len(name))]
				}
				bar = progress.NewBar(fmt.Sprintf("pulling %s:", name), resp.Total, resp.Completed)
				bars[resp.Digest] = bar
				p.Add(resp.Digest, bar)
			}

			bar.Set(resp.Completed)
		} else if status != resp.Status {
			if spinner != nil {
				spinner.Stop()
			}

			status = resp.Status
			spinner = progress.NewSpinner(status)
			p.Add(status, spinner)
		}

		return nil
	}

	request := api.PullRequest{Name: model, Insecure: insecure}
	return client.Pull(ctx, &request, fn)
}
