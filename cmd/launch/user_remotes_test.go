package launch

import (
	"testing"
)

func TestOpenAIBase(t *testing.T) {
	tests := []struct {
		name string
		rem  userRemote
		want string
	}{
		{
			name: "default v1 version, bare base",
			rem:  userRemote{Name: "deepseek", BaseURL: "https://api.deepseek.com"},
			want: "https://api.deepseek.com/v1",
		},
		{
			name: "base already carries /v1 is de-duplicated",
			rem:  userRemote{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1"},
			want: "https://api.deepseek.com/v1",
		},
		{
			name: "zai v4 version",
			rem:  userRemote{Name: "zai", BaseURL: "https://api.z.ai/api/paas", Version: "v4"},
			want: "https://api.z.ai/api/paas/v4",
		},
		{
			name: "version slash normalization",
			rem:  userRemote{Name: "zai", BaseURL: "https://api.z.ai/api/paas/", Version: "/v4/"},
			want: "https://api.z.ai/api/paas/v4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rem.openAIBase(); got != tt.want {
				t.Fatalf("openAIBase() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuiltinRemotes_ZAIKeyGate(t *testing.T) {
	t.Setenv(zaiEnvKey, "")
	// Every builtin is env-gated; clear the others too or a key in the
	// developer's shell makes builtinRemotes() non-nil here.
	t.Setenv(openrouterEnvKey, "")

	if got := builtinRemotes(); got != nil {
		t.Fatalf("builtinRemotes() = %v, want nil when %s unset", got, zaiEnvKey)
	}

	t.Setenv(zaiEnvKey, "zai-secret")
	got := builtinRemotes()
	if len(got) != 1 {
		t.Fatalf("builtinRemotes() len = %d, want 1", len(got))
	}
	z := got[0]
	if z.Name != zaiName {
		t.Fatalf("builtin name = %q, want %q", z.Name, zaiName)
	}
	if z.BaseURL != zaiBaseURL {
		t.Fatalf("builtin base_url = %q, want %q", z.BaseURL, zaiBaseURL)
	}
	if z.APIKeyEnv != zaiEnvKey {
		t.Fatalf("builtin api_key_env = %q, want %q", z.APIKeyEnv, zaiEnvKey)
	}
	if z.Version != "v4" {
		t.Fatalf("builtin version = %q, want v4", z.Version)
	}
	if got := z.key(); got != "zai-secret" {
		t.Fatalf("builtin key() = %q, want zai-secret", got)
	}
	if got, want := z.openAIBase(), "https://api.z.ai/api/paas/v4"; got != want {
		t.Fatalf("builtin openAIBase() = %q, want %q", got, want)
	}
}

func TestBuiltinRemotes_MergedIntoLoad(t *testing.T) {
	t.Setenv(zaiEnvKey, "zai-secret")
	t.Setenv("OAICA_REMOTES_FILE", t.TempDir()+"/does-not-exist.json")

	remotes, err := loadUserRemotes()
	if err != nil {
		t.Fatalf("loadUserRemotes() error: %v", err)
	}
	found := false
	for _, r := range remotes {
		if r.Name == zaiName {
			found = true
		}
	}
	if !found {
		t.Fatalf("builtin %s not merged into loadUserRemotes(): %+v", zaiName, remotes)
	}
}
