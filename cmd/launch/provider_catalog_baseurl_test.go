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
func TestOpenAIBase_VersionNoneAppendsNothing(t *testing.T) {
	cases := []struct{ version, want string }{
		{"", "https://api.example.com/v1"},
		{"v1", "https://api.example.com/v1"},
		{"/v1/", "https://api.example.com/v1"},
		{"v4", "https://api.example.com/v4"},
		{"none", "https://api.example.com"},
		{"NONE", "https://api.example.com"},
	}
	for _, c := range cases {
		r := userRemote{Name: "x", BaseURL: "https://api.example.com", Version: c.version}
		if got := r.openAIBase(); got != c.want {
			t.Errorf("version %q -> %s, want %s", c.version, got, c.want)
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
