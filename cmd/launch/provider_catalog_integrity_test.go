package launch

// provider_catalog_integrity_test.go — permanent guards for the two failures
// the 2026-09-26 audit found in the provider directory, plus the catalog-wide
// invariants that caught them.
//
// The failures, both of which reached real hosts:
//
//  1. A synced copy of the provider directory replaced an embedded row
//     WHOLESALE. A host that synced before a field existed lost that field — a
//     synced zhipu row dropped `version`, so the row resolved to
//     ".../api/paas/v4/v1/chat/completions" and 404'd every request while the
//     picker listed its models. A copy that predates an endpoint fix likewise
//     kept pointing at the old one. Both directions of the same bug: a document
//     of unknown age was allowed to overwrite a known one.
//  2. Two rows named an endpoint the vendor does not serve (perplexity,
//     github-copilot), each one version segment too deep.
//
// The mechanism that now prevents the first is the overlay's per-key,
// per-field merge (catalog_overlay.go): a synced document corrects what it
// NAMES and cannot blank what it omits. The version-bump gate this file used to
// exercise is gone with the wholesale-replace layer it belonged to — a
// correction lands at read time now, with no bump to make. The endpoint
// invariants below (tests 2-4) are unchanged.

import (
	"fmt"
	"strings"
	"testing"
)

// embeddedCatalogRows reads the go:embed that ships in the binary — not
// providerCatalog(), which would let a models.dev payload synced on the
// developer's own machine mask or alter what is under test.
func embeddedCatalogRows(t *testing.T) []providerCatalogEntry {
	t.Helper()
	rows := parseOverlayBytes(oaicaOverlayEmbedded).Providers
	if len(rows) == 0 {
		t.Fatal("the embedded overlay parsed to nothing — the embed or the parse is broken")
	}
	return rows
}

func embeddedRow(t *testing.T, name string) providerCatalogEntry {
	t.Helper()
	for _, e := range embeddedCatalogRows(t) {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("the embedded overlay has no row %q — did the embed change?", name)
	return providerCatalogEntry{}
}

// writeSyncedProviderCache plants an overlay copy exactly as `oaica remote
// sync` writes it, for the duration of one test.
//
// The document still carries a "version" member because the real file does; it
// is read by nothing (the merge is per key), which the tests below pin by
// running the same correction through several of them.
func writeSyncedProviderCache(t *testing.T, fileVersion int, providersJSON string) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	writeOverlayCache(t, fmt.Sprintf(`{"version":%d,"providers":[%s]}`, fileVersion, providersJSON))
}

func mergedCatalogRow(t *testing.T, name string) providerCatalogEntry {
	t.Helper()
	for _, e := range providerCatalog() {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("%q is missing from the merged catalog", name)
	return providerCatalogEntry{}
}

// A synced overlay corrects the fields it names and cannot touch the rest: the
// version-bump gate this replaced decided WHOLESALE redefinition, so a row
// synced before a field existed lost that field. Here the document states an
// endpoint for three rows — one of them an endpoint our shipped row no longer
// uses — and says nothing about the version, the wire or the tool format.
//
// The rows below are byte-for-byte what the pre-2026-09-26 hosted file shipped:
// the version inside base_url, no "version" field. That shape is why the SHIPPED
// overlay keeps its version OUT of base_url (zhipu: "…/api/paas" + "v4"): a
// base_url that carries the version while the row's Version field states it too
// resolves to "…/v4/v4", because openAIBase appends the version to any base that
// is not already the whole root. A document can state whichever shape it likes,
// but the two fields have to agree — see the correction test below.
func TestProviderCatalog_SyncedOverlayCorrectsOnlyWhatItNames(t *testing.T) {
	stale := strings.Join([]string{
		`{"name":"zhipu","base_url":"https://open.bigmodel.cn/api/paas","api_key_env":"ZHIPU_API_KEY"}`,
		`{"name":"zai-coding-plan","base_url":"https://api.z.ai/api/coding/paas/v4","version":"none","api_key_env":"Z_AI_API_KEY"}`,
		`{"name":"brand-new-plan","base_url":"https://api.example.com/v1","api_key_env":"BRAND_NEW_KEY"}`,
	}, ",")
	// The version the document declares decides nothing now: the merge is per
	// key at read time, so all three of these are "the newer document" whatever
	// number they carry.
	for _, fileVersion := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("document_version_%d", fileVersion), func(t *testing.T) {
			writeSyncedProviderCache(t, fileVersion, stale)

			zhipu := mergedCatalogRow(t, "zhipu")
			if zhipu.Version != "v4" {
				t.Errorf("merged zhipu has version=%q — the synced copy blanked the field the shipped row carries", zhipu.Version)
			}
			if got, want := zhipu.EndpointBase()+"/chat/completions",
				"https://open.bigmodel.cn/api/paas/v4/chat/completions"; got != want {
				t.Errorf("merged zhipu resolves to %s, want %s", got, want)
			}

			// The correction itself landed — that is the whole point of the
			// layer, and the endpoint below is the one our row stopped using.
			plan := mergedCatalogRow(t, "zai-coding-plan")
			if want := "https://api.z.ai/api/coding/paas/v4"; plan.EndpointBase() != want {
				t.Errorf("merged zai-coding-plan resolves to %s, want the synced correction %s", plan.EndpointBase(), want)
			}
			// Fields the document simply does not carry survive.
			if want := embeddedRow(t, "zai-coding-plan"); plan.ToolFormat != want.ToolFormat || plan.Wire != want.Wire {
				t.Errorf("merged zai-coding-plan lost fields the shipped row carries: tool_format=%q wire=%q, want %q/%q",
					plan.ToolFormat, plan.Wire, want.ToolFormat, want.Wire)
			}

			// Additions are the point of sync, and must still land.
			if got := mergedCatalogRow(t, "brand-new-plan"); got.Name != "brand-new-plan" {
				t.Errorf("a provider the synced copy adds was dropped: %+v", got)
			}
		})
	}
}

// A correction may state its own version, and does not need a bump to do it:
// the field wins on the same terms as every other string field, and the
// endpoint resolves from the corrected pair.
func TestProviderCatalog_CorrectionStatesItsOwnVersion(t *testing.T) {
	writeSyncedProviderCache(t, 1,
		`{"name":"zhipu","base_url":"https://new.example.com/openai","version":"none","api_key_env":"ZHIPU_API_KEY"}`)

	got := mergedCatalogRow(t, "zhipu")
	if want := "https://new.example.com/openai"; got.EndpointBase() != want {
		t.Errorf("the corrected row resolves to %s, want %s (the document stated version %q)", got.EndpointBase(), want, got.Version)
	}
}

// Every shipped row must resolve to the base its vendor documents. This is the
// table that caught perplexity and github-copilot resolving one version
// segment too deep (both 404 on every request), and it keeps the six rows whose
// version moved out of base_url from drifting back.
//
// Base values come from the vendor's own reference and from models.dev's
// api.json (the same registry opencode's provider data is drawn from) — see
// the audit report for the unauthenticated probes behind each.
func TestProviderCatalog_ResolvedBasesMatchTheVendors(t *testing.T) {
	want := map[string]string{
		// Version segments that are part of the path, not appended after it.
		"zhipu":                  "https://open.bigmodel.cn/api/paas/v4",
		"zhipuai-coding-plan":    "https://open.bigmodel.cn/api/coding/paas/v4",
		"tencent-coding-plan":    "https://api.lkeap.cloud.tencent.com/coding/v3",
		"tencent-token-plan":     "https://api.lkeap.cloud.tencent.com/plan/v3",
		"volcengine-coding-plan": "https://ark.cn-beijing.volces.com/api/coding/v3",
		"zai":                    "https://api.z.ai/api/paas/v4",
		"zai-coding-plan":        "https://api.z.ai/api/anthropic/v1",
		// Roots that must have nothing appended (version "none").
		"google":         "https://generativelanguage.googleapis.com/v1beta/openai",
		"perplexity":     "https://api.perplexity.ai",
		"github-copilot": "https://api.githubcopilot.com",
		// First-party and aggregator rows with an ordinary /v1.
		"minimax":                "https://api.minimax.io/v1",
		"minimax-coding-plan":    "https://api.minimax.io/anthropic/v1",
		"minimax-cn-coding-plan": "https://api.minimax.cn/anthropic/v1",
		"kimi-code-plan-global":  "https://api.kimi.ai/coding/v1",
		"kimi-code-plan-cn":      "https://api.kimi.com/coding/v1",
		"stepfun-ai-step-plan":   "https://api.stepfun.ai/step_plan/v1",
		"stepfun-step-plan":      "https://api.stepfun.com/step_plan/v1",
		"scnet-token-plan":       "https://api.scnet.cn/api/llm/v1",
		"kuae-cloud-coding-plan": "https://coding-plan-endpoint.kuaecloud.net/v1",
		"umans-ai-coding-plan":   "https://api.code.umans.ai/v1",
		"xiaomi-token-plan-ams":  "https://token-plan-ams.xiaomimimo.com/v1",
		"xiaomi-token-plan-cn":   "https://token-plan-cn.xiaomimimo.com/v1",
		"xiaomi-token-plan-sgp":  "https://token-plan-sgp.xiaomimimo.com/v1",
		"alibaba-coding-plan":    "https://coding-intl.dashscope.aliyuncs.com/v1",
		"alibaba-coding-plan-cn": "https://coding.dashscope.aliyuncs.com/v1",
		"alibaba-token-plan":     "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1",
		"alibaba-token-plan-cn":  "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
		"opencode":               "https://opencode.ai/zen/v1",
		"opencode-go":            "https://opencode.ai/zen/go/v1",
		"zenifra":                "https://ai.zenifra.com/v1",
		"zenmux":                 "https://zenmux.ai/api/v1",
		"anthropic":              "https://api.anthropic.com/v1",
		"openai":                 "https://api.openai.com/v1",
		"groq":                   "https://api.groq.com/openai/v1",
		"mistral":                "https://api.mistral.ai/v1",
		"deepseek":               "https://api.deepseek.com/v1",
		"xai":                    "https://api.x.ai/v1",
		"together":               "https://api.together.xyz/v1",
		"fireworks":              "https://api.fireworks.ai/inference/v1",
		"cerebras":               "https://api.cerebras.ai/v1",
		"alibaba":                "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
		"moonshot":               "https://api.moonshot.ai/v1",
		"openrouter":             "https://openrouter.ai/api/v1",
		"ollama-cloud":           "https://ollama.com/v1",
	}

	seen := map[string]bool{}
	for _, e := range embeddedCatalogRows(t) {
		wantBase, classified := want[e.Name]
		if !classified {
			// Not a failure of the row: a failure to look at it. A new provider
			// must be added to this table, with the vendor's documented base.
			t.Errorf("row %q is not classified by this test (add its documented base to the table)", e.Name)
			continue
		}
		seen[e.Name] = true
		if got := e.EndpointBase(); got != wantBase {
			t.Errorf("%s resolves to %s, want %s (base_url=%q version=%q)", e.Name, got, wantBase, e.BaseURL, e.Version)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("expected row %q is absent from providers.json", name)
		}
	}
}

// A row shipped twice under two names, or two rows resolving onto one
// host+path+wire, means one of them is silently unreachable — the picker shows
// two entries and the vendor sees one endpoint. Cheap to check, and it fails
// loudly the day a duplicate is pasted in.
func TestProviderCatalog_NoTwoRowsShareAnEndpoint(t *testing.T) {
	byEndpoint := map[string][]string{}
	for _, e := range embeddedCatalogRows(t) {
		path := "/chat/completions"
		if strings.EqualFold(e.Wire, "anthropic") {
			path = "/messages"
		}
		k := e.EndpointBase() + path
		byEndpoint[k] = append(byEndpoint[k], e.Name)
	}
	for endpoint, names := range byEndpoint {
		if len(names) > 1 {
			t.Errorf("endpoint %s is claimed by %v — one row shadows the other", endpoint, names)
		}
	}
}

// A vendor's version prefix is per-surface, not per-host: perplexity serves
// chat at "/chat/completions" (unversioned) and lists models at "/v1/models".
// One `version` field cannot express both, so the row sets version "none" plus
// models_path — and the picker's sweep and doctor's reachability probe both
// have to go to the LIST path, not to the chat base.
func TestProviderCatalog_ModelsURLUsesTheVendorsListPath(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		// Default: openAIBase() + "/models".
		{"zhipu", "https://open.bigmodel.cn/api/paas/v4/models"},
		{"zai", "https://api.z.ai/api/paas/v4/models"},
		{"openai", "https://api.openai.com/v1/models"},
		{"google", "https://generativelanguage.googleapis.com/v1beta/openai/models"},
		// Overridden: the list is versioned even though chat is not.
		{"perplexity", "https://api.perplexity.ai/v1/models"},
	}
	for _, c := range cases {
		e := embeddedRow(t, c.name)
		r := userRemote{Name: e.Name, BaseURL: e.BaseURL, Version: e.Version, ModelsPath: e.ModelsPath}
		if got := r.modelsURL(); got != c.want {
			t.Errorf("%s lists models at %s, want %s (base_url=%q version=%q models_path=%q)",
				c.name, got, c.want, e.BaseURL, e.Version, e.ModelsPath)
		}
	}
}

// Renaming an environment variable a user has already put in a shell profile
// must not look, from their seat, like the provider disappearing. A row may
// name more than one variable, and BOTH have to open the gate and supply the
// key.
func TestProviderCatalog_EveryNamedEnvVarIsAccepted(t *testing.T) {
	e := embeddedRow(t, "opencode-go")
	r := userRemote{Name: e.Name, APIKeyEnv: e.APIKeyEnv}
	names := r.keyEnvNames()
	for _, want := range []string{"OPENCODE_API_KEY", "OPENCODE_GO_API_KEY"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("api_key_env %q does not name %s — a host that exported it gets an unconfigured row", e.APIKeyEnv, want)
		}
	}
	for _, env := range names {
		t.Run(env, func(t *testing.T) {
			for _, other := range names {
				t.Setenv(other, "")
			}
			t.Setenv(env, "sk-"+env)
			if !remoteKeyEnvSet(r) {
				t.Errorf("%s is set but the row still reads as unconfigured", env)
			}
			if got := r.key(); got != "sk-"+env {
				t.Errorf("key() = %q, want the value of %s", got, env)
			}
		})
	}
}

// The field a correction is most often used for is a model's context window (a
// vendor revises it down as often as up). The merge this replaced called
// fillEmptyProviderFields(dst=synced, src=embedded), which for the declared
// windows meant the EMBEDDED row's numbers won wherever it stated one — so a
// correction was silently overruled and every host kept over-reporting the old
// window (CLAUDE_CODE_MAX_CONTEXT_TOKENS and the proxy's context-fit clamp
// included). Now the stated number wins, and a window the document does not
// restate is not blanked.
func TestProviderCatalog_CorrectionUsesTheSyncedModelWindows(t *testing.T) {
	shipped := embeddedRow(t, "zai")
	old, ok := shipped.Models["glm-5.3"]
	if !ok || old.Context == 0 || old.Output == 0 {
		t.Fatalf("the embedded zai row no longer declares glm-5.3 with both windows (%+v) — pick another row", shipped.Models)
	}
	corrected := old.Context + 12345

	writeSyncedProviderCache(t, 99, fmt.Sprintf(
		`{"name":"zai","base_url":"https://api.z.ai/api/paas","version":"v4","api_key_env":"Z_AI_API_KEY",`+
			`"models":{"glm-5.3":{"context":%d}}}`,
		corrected))

	got := mergedCatalogRow(t, "zai")
	if got.Models["glm-5.3"].Context != corrected {
		t.Errorf("glm-5.3 context = %d, want the synced correction %d — the shipped number must not overrule it",
			got.Models["glm-5.3"].Context, corrected)
	}
	if got.Models["glm-5.3"].Output != old.Output {
		t.Errorf("glm-5.3 output = %d, want %d: a field the document does not state is filled from the shipped row, not blanked",
			got.Models["glm-5.3"].Output, old.Output)
	}
	// A declared model the document never names keeps its shipped window, and
	// one it adds lands: the merge is per model id, not wholesale.
	if other, ok := shipped.Models["glm-5"]; ok {
		if got.Models["glm-5"].Context != other.Context {
			t.Errorf("glm-5 context = %d, want the shipped %d: a correction to another model dropped it",
				got.Models["glm-5"].Context, other.Context)
		}
	}
	writeSyncedProviderCache(t, 1, fmt.Sprintf(
		`{"name":"zai","models":{"glm-unreleased":{"context":%d,"output":4096}}}`, corrected))
	if l, ok := mergedCatalogRow(t, "zai").Models["glm-unreleased"]; !ok || l.Context != corrected {
		t.Errorf("a model the synced copy adds did not land: %+v, ok=%v", l, ok)
	}
}

// A row with a whitespace base_url is a hand-edit that lost its content, not an
// endpoint. The merge judges the value TRIMMED and keeps the shipped one when
// nothing is left, because the alternative is a row whose every URL is relative
// ("/v4/chat/completions" — a 404 whose error names no host): the older
// wholesale-replace path only filled a LITERALLY empty BaseURL, so "   "
// survived as the row's endpoint.
func TestProviderCatalog_CorrectionWithAWhitespaceEndpointKeepsTheShippedOne(t *testing.T) {
	shipped := embeddedRow(t, "zai")
	writeSyncedProviderCache(t, 99, `{"name":"zai","base_url":"   ","api_key_env":"Z_AI_API_KEY"}`)

	got := mergedCatalogRow(t, "zai")
	if got.EndpointBase() != shipped.EndpointBase() {
		t.Errorf("a document that states no endpoint moved zai from %q to %q — every request is now relative",
			shipped.EndpointBase(), got.EndpointBase())
	}
	if strings.TrimSpace(got.BaseURL) == "" {
		t.Error("the merged row's base_url is blank")
	}
}
