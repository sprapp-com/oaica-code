package launch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModelAlias_SetGetListRemove(t *testing.T) {
	withTempOaicaHome(t)

	if err := ModelAliasSet("glm", "ollama/glm-5.3-flash:cloud"); err != nil {
		t.Fatalf("ModelAliasSet: %v", err)
	}
	target, err := ModelAliasGet("glm")
	if err != nil {
		t.Fatalf("ModelAliasGet: %v", err)
	}
	if target != "ollama/glm-5.3-flash:cloud" {
		t.Errorf("target = %q, want ollama/glm-5.3-flash:cloud", target)
	}

	names, err := ModelAliasSortedNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "glm" {
		t.Fatalf("ModelAliasSortedNames = %v", names)
	}

	existed, err := ModelAliasRemove("glm")
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Fatal("ModelAliasRemove reported not-existed for an alias that was just set")
	}
	if _, err := ModelAliasGet("glm"); err == nil {
		t.Fatal("ModelAliasGet succeeded after removal")
	}
}

func TestModelAlias_ValidatesInput(t *testing.T) {
	withTempOaicaHome(t)
	if err := ModelAliasSet("", "x"); err == nil {
		t.Fatal("expected error for empty alias name")
	}
	if err := ModelAliasSet("x", ""); err == nil {
		t.Fatal("expected error for empty target")
	}
	if err := ModelAliasSet("has/slash", "x"); err == nil {
		t.Fatal("expected error for alias name containing '/' (ambiguous with <remote>/<id>)")
	}
}

func TestResolveModelAlias_NoHitForUndefined(t *testing.T) {
	withTempOaicaHome(t)
	if _, ok := resolveModelAlias("nope"); ok {
		t.Fatal("expected no hit for an undefined alias")
	}
}

func TestResolveLaunchEndpoint_AliasAppliedFirst(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	remotesPath := filepath.Join(dir, "remotes.json")
	content := `{
  "remotes": [
    { "name": "opencode-go", "base_url": "http://127.0.0.1:1/v1", "api_key": "k" }
  ]
}`
	if err := os.WriteFile(remotesPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OAICA_REMOTES_FILE", remotesPath)

	if err := ModelAliasSet("myshort", "opencode-go/glm-5.3-flash"); err != nil {
		t.Fatal(err)
	}

	// Proves the alias substitution happens BEFORE remote resolution: the
	// bare id "myshort" only resolves to a real endpoint because it was
	// rewritten to "opencode-go/glm-5.3-flash" first.
	ep, err := resolveLaunchEndpoint("myshort")
	if err != nil {
		t.Fatalf("resolveLaunchEndpoint(myshort): %v", err)
	}
	if ep.UpstreamModel != "glm-5.3-flash" {
		t.Errorf("UpstreamModel = %q, want glm-5.3-flash (alias should have resolved to opencode-go/glm-5.3-flash before remote resolution)", ep.UpstreamModel)
	}
	if ep.Source != sourceUserRemote {
		t.Errorf("Source = %q, want %q", ep.Source, sourceUserRemote)
	}
}
