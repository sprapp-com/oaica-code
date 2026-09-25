package launch

// A catalog row's base_url may or may not carry its version segment; openAIBase
// normalises by stripping a trailing "/v1" and appending Version (default
// "v1"). That only works for rows versioned v1. A row whose base_url ended in
// a DIFFERENT version — Zhipu's "/api/paas/v4", Tencent's "/coding/v3",
// Volcengine's "/api/coding/v3" — had a second segment appended and resolved to
// "…/v4/v1/chat/completions": a 404 on every request, with the picker
// cheerfully listing the models. Google is the other shape: its OpenAI root is
// "…/v1beta/openai", where the version is a path segment BEFORE the root, so
// nothing may be appended at all (2026-09-26 audit).

import (
	"fmt"
	"regexp"
	"testing"
)

func catalogRemotesByName(t *testing.T) map[string]userRemote {
	t.Helper()
	byName := map[string]userRemote{}
	for _, r := range providerCatalogAsUserRemotes() {
		byName[r.Name] = r
	}
	if len(byName) == 0 {
		t.Fatal("the provider catalog is empty — the embed or the parse is broken")
	}
	return byName
}

func TestProviderCatalog_BaseURLsResolveToTheDocumentedEndpoint(t *testing.T) {
	want := map[string]string{
		// Versioned v3/v4 in the base URL: the version must not be doubled.
		"zhipu":                  "https://open.bigmodel.cn/api/paas/v4/chat/completions",
		"zhipuai-coding-plan":    "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
		"tencent-coding-plan":    "https://api.lkeap.cloud.tencent.com/coding/v3/chat/completions",
		"tencent-token-plan":     "https://api.lkeap.cloud.tencent.com/plan/v3/chat/completions",
		"volcengine-coding-plan": "https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions",
		// Version before the root, not after it: append nothing.
		"google": "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		// Same class, found by probing both paths live (2026-09-26): the
		// versioned path answers 404 and the bare one answers the vendor's own
		// auth error, so the version must not be appended.
		"perplexity":     "https://api.perplexity.ai/chat/completions",
		"github-copilot": "https://api.githubcopilot.com/chat/completions",
		// Controls — these were already right and must stay so.
		"zai":     "https://api.z.ai/api/paas/v4/chat/completions",
		"mistral": "https://api.mistral.ai/v1/chat/completions",
		"openai":  "https://api.openai.com/v1/chat/completions",
		"groq":    "https://api.groq.com/openai/v1/chat/completions",
	}
	byName := catalogRemotesByName(t)
	for name, endpoint := range want {
		r, ok := byName[name]
		if !ok {
			t.Errorf("catalog row %q is gone — did the embed change?", name)
			continue
		}
		if got := r.openAIBase() + "/chat/completions"; got != endpoint {
			t.Errorf("%s resolves to %s, want %s", name, got, endpoint)
		}
	}
}

// The sentinel itself, pinned apart from the catalog so the mechanism is
// tested even if the catalog stops using it.
//
// "Append nothing" means append nothing — and REMOVE nothing either. The
// sentinel's own documented use is a base_url that already IS the OpenAI root,
// and the most common such root on earth ends in "/v1"; routing the sentinel
// through remoteBaseURL (which strips a trailing /v1 for the append case)
// would delete the version the user configured and send every request to
// "<host>/chat/completions", which is a 404 on any standard endpoint.
func TestOpenAIBase_VersionNoneAppendsNothing(t *testing.T) {
	cases := []struct{ base, version, want string }{
		{"https://api.example.com", "", "https://api.example.com/v1"},
		{"https://api.example.com", "v1", "https://api.example.com/v1"},
		{"https://api.example.com", "/v1/", "https://api.example.com/v1"},
		{"https://api.example.com", "v4", "https://api.example.com/v4"},
		{"https://api.example.com", "none", "https://api.example.com"},
		{"https://api.example.com", "NONE", "https://api.example.com"},
		// The sentinel's own case: the base is already the whole root.
		{"https://generativelanguage.googleapis.com/v1beta/openai", "none",
			"https://generativelanguage.googleapis.com/v1beta/openai"},
		// A root that ends in /v1 keeps it — this is the case the sentinel
		// must not "normalise".
		{"https://api.example.com/v1", "none", "https://api.example.com/v1"},
		{"https://gw.corp.example/openai/v1/", "none", "https://gw.corp.example/openai/v1"},
		// Any version, with any trailing slash, without doubling.
		{"https://api.z.ai/api/paas", "v4", "https://api.z.ai/api/paas/v4"},
		{"https://api.deepseek.com/v1", "v1", "https://api.deepseek.com/v1"},
	}
	for _, c := range cases {
		r := userRemote{Name: "x", BaseURL: c.base, Version: c.version}
		if got := r.openAIBase(); got != c.want {
			t.Errorf("BaseURL=%q version=%q -> %s, want %s (the request would go to %s/chat/completions)",
				c.base, c.version, got, c.want, got)
		}
	}
}

// The general invariant, so a future row cannot reintroduce this: no catalog
// row may resolve to a base with two version segments stacked at the end.
func TestProviderCatalog_NoRowDoublesItsVersionSegment(t *testing.T) {
	doubled := regexp.MustCompile(`/v[0-9]+/v[0-9]+$`)
	for _, r := range providerCatalogAsUserRemotes() {
		base := r.openAIBase()
		if doubled.MatchString(base) {
			t.Errorf("%s resolves to %s — the base_url already carries a version and Version was appended to it as well", r.Name, base)
		}
		if got := fmt.Sprintf("%s/models", base); doubled.MatchString(got) {
			t.Errorf("%s: %s", r.Name, got)
		}
	}
}
