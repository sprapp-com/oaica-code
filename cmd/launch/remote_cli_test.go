package launch

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// withTempRemotesFile points remotes.json at a throwaway path and clears the
// built-in providers' env vars, so these tests never see a developer's real
// remotes or a Z_AI_API_KEY exported in their shell.
func withTempRemotesFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "remotes.json")
	t.Setenv("OAICA_REMOTES_FILE", path)
	t.Setenv(zaiEnvKey, "")
	t.Setenv(openrouterEnvKey, "")
	t.Setenv(ollamaCloudEnvKey, "")
	return path
}

func TestRemoteAddListShowRemoveRoundTrip(t *testing.T) {
	withTempRemotesFile(t)

	if _, err := RemoteAdd(RemoteAddOptions{
		Name: "mybox", BaseURL: "https://api.example.com/", APIKeyEnv: "MYBOX_KEY",
	}); err != nil {
		t.Fatalf("RemoteAdd: %v", err)
	}

	var list bytes.Buffer
	if err := WriteRemoteList(&list); err != nil {
		t.Fatalf("WriteRemoteList: %v", err)
	}
	for _, want := range []string{"mybox", "https://api.example.com", "openai", "tool_calls", "env:MYBOX_KEY"} {
		if !strings.Contains(list.String(), want) {
			t.Fatalf("list missing %q, got:\n%s", want, list.String())
		}
	}

	var show bytes.Buffer
	if err := WriteRemoteShow(&show, "mybox"); err != nil {
		t.Fatalf("WriteRemoteShow: %v", err)
	}
	if !strings.Contains(show.String(), "env:MYBOX_KEY") {
		t.Fatalf("show missing auth line, got:\n%s", show.String())
	}

	// The remote must be visible to the real resolver, not just to this CLI.
	if _, bare, ok := findUserRemoteForModel("mybox/some-model"); !ok || bare != "some-model" {
		t.Fatalf("findUserRemoteForModel = (%q, %v), want (some-model, true)", bare, ok)
	}

	existed, err := RemoteRemove("mybox")
	if err != nil || !existed {
		t.Fatalf("RemoteRemove = (%v, %v)", existed, err)
	}
	if _, err := RemoteShow("mybox"); err == nil {
		t.Fatal("RemoteShow must fail after rm")
	}
}

// A literal api key must never be echoed back — only that one is set.
func TestRemoteShowNeverPrintsAPIKey(t *testing.T) {
	withTempRemotesFile(t)
	if _, err := RemoteAdd(RemoteAddOptions{
		Name: "secretbox", BaseURL: "https://api.example.com", APIKey: "sk-super-secret",
	}); err != nil {
		t.Fatal(err)
	}

	var show, list bytes.Buffer
	if err := WriteRemoteShow(&show, "secretbox"); err != nil {
		t.Fatal(err)
	}
	if err := WriteRemoteList(&list); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{show.String(), list.String()} {
		if strings.Contains(out, "sk-super-secret") {
			t.Fatalf("api key leaked into output:\n%s", out)
		}
	}
	if !strings.Contains(show.String(), "<set>") {
		t.Fatalf("show should report the key as <set>, got:\n%s", show.String())
	}
}

// Empty state must name the add command, the same way `oaica model list` does.
func TestWriteRemoteListEmptyStateNamesAddCommand(t *testing.T) {
	withTempRemotesFile(t)
	var out bytes.Buffer
	if err := WriteRemoteList(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "oaica remote add") {
		t.Fatalf("empty state must tell the user how to add a remote, got:\n%s", out.String())
	}
}

func TestRemoteAddValidation(t *testing.T) {
	withTempRemotesFile(t)
	cases := []struct {
		name string
		opts RemoteAddOptions
		want string
	}{
		{"no base url", RemoteAddOptions{Name: "x"}, "--base-url is required"},
		{"both keys", RemoteAddOptions{Name: "x", BaseURL: "https://a", APIKey: "k", APIKeyEnv: "V"}, "mutually exclusive"},
		{"bad wire", RemoteAddOptions{Name: "x", BaseURL: "https://a", Wire: "grpc"}, "--wire must be one of"},
		{"bad tool format", RemoteAddOptions{Name: "x", BaseURL: "https://a", ToolFormat: "magic"}, "--tool-format must be one of"},
		{"slash in name", RemoteAddOptions{Name: "a/b", BaseURL: "https://a"}, "must not contain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RemoteAdd(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// Re-adding an existing name is an edit: fields this CLI doesn't expose
// (force_tools, prices) must survive.
func TestRemoteAddPreservesUnexposedFields(t *testing.T) {
	path := withTempRemotesFile(t)
	if _, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: "https://a"}); err != nil {
		t.Fatal(err)
	}
	f, _, err := loadUserRemotesFileRaw()
	if err != nil {
		t.Fatal(err)
	}
	f.Remotes[0].ForceTools = true
	f.Remotes[0].PriceInputPerM = 1.5
	if err := saveUserRemotesFile(f, path); err != nil {
		t.Fatal(err)
	}

	if _, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: "https://b"}); err != nil {
		t.Fatal(err)
	}
	f, _, err = loadUserRemotesFileRaw()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Remotes) != 1 {
		t.Fatalf("re-add should replace, not append: %+v", f.Remotes)
	}
	if f.Remotes[0].BaseURL != "https://b" {
		t.Fatalf("base_url not updated: %+v", f.Remotes[0])
	}
	if !f.Remotes[0].ForceTools || f.Remotes[0].PriceInputPerM != 1.5 {
		t.Fatalf("hand-tuned fields were reset: %+v", f.Remotes[0])
	}
}
