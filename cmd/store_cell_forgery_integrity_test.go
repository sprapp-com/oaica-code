package cmd

// store_cell_forgery_integrity_test.go — the CLI's own listings printed the
// values they hold RAW (2026-09-26 audit, eleventh round).
//
// cmd/launch already has the rule and applies it to every store-backed value
// it prints: printableName (remote_cli.go) and manifestCell
// (model_manifest_cli.go) render a value carrying a control character in
// Go-quoted form, so a stored name can never forge a row, or a second field
// line, in a one-line report. Package cmd has no such call at all — the
// writers accept control characters (`model alias set` rejects only an empty
// name and '/', PlanSet bounds the plan NAME to 64 bytes and leaves the
// description unbounded, the config setters trim only), so
//
//	oaica model alias set $'prod\n  evil' --target ollama/glm-5.3-flash:cloud
//	oaica model alias list
//
// printed a fabricated second row naming a model the file never held — in the
// listing a user reads to find out what their aliases actually resolve to.
//
// These tests drive the REAL writer (launch.ModelAliasSet / launch.PlanSet /
// launch.UserConfigSetSonnetModel) and then the REAL list command, and assert
// on the terminal output. Each one carries a control: an ordinary value must
// still print unquoted, so "quote everything" is not a pass.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ollama/ollama/cmd/launch"
)

// outputLines splits command output into its physical lines, dropping the
// empty tail a trailing newline leaves behind. A forged row is one extra
// element here: that is the whole symptom.
func outputLines(out string) []string {
	out = strings.TrimSuffix(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// runOAICA executes one command through the real CLI and returns what it
// printed on stdout (every listing here uses fmt.Printf, not cmd.OutOrStdout).
func runOAICA(t *testing.T, args ...string) string {
	t.Helper()
	root := NewCLI()
	return captureStdout(t, func() {
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Errorf("`oaica %s` failed: %v", strings.Join(args, " "), err)
		}
	})
}

// A stored alias name carrying a newline must not add a row to `model alias
// list`, and must still be visible in the row that IS its own.
func TestModelAliasListDoesNotPrintAForgedRow(t *testing.T) {
	t.Setenv("OAICA_ALIASES_FILE", filepath.Join(t.TempDir(), "aliases.json"))

	const target = "ollama/glm-5.3-flash:cloud"
	nasty := "prod\n  evil"
	if err := launch.ModelAliasSet(nasty, target); err != nil {
		t.Fatalf("ModelAliasSet(%q): %v", nasty, err)
	}
	// Control: an ordinary alias. Its row must be printed as-is, unquoted.
	if err := launch.ModelAliasSet("glm", target); err != nil {
		t.Fatalf("ModelAliasSet(glm): %v", err)
	}

	out := runOAICA(t, "model", "alias", "list")
	lines := outputLines(out)
	if len(lines) != 2 {
		t.Errorf("`model alias list` printed %d line(s) for 2 aliases — a stored name forged a row:\n%s", len(lines), out)
	}
	// The value is still visible, and now unambiguously a quoted one.
	if !strings.Contains(out, `"prod\n  evil"`) {
		t.Errorf("the stored alias name is not shown in its quoted form, so the user cannot read back what the file holds:\n%s", out)
	}
	wantControl := fmt.Sprintf("%-20s -> %s", "glm", target)
	if !strings.Contains(out, wantControl) {
		t.Errorf("the ordinary alias row changed shape — want %q in:\n%s", wantControl, out)
	}
}

// Same for a plan: the NAME is what the user reads, the DESCRIPTION is free
// text (unbounded by PlanSet), and both are printed in one row.
func TestPlanListDoesNotPrintAForgedRow(t *testing.T) {
	t.Setenv("OAICA_PLANS_FILE", filepath.Join(t.TempDir(), "plans.json"))

	nastyName := "team\n  prod"
	nastyDesc := "first line\n  FABRICATED ROW: opus -> a-model-the-file-never-held"
	if err := launch.PlanSet(nastyName, launch.TierPlanProfile{
		Model:       "ollama/glm-5.3-flash:cloud",
		Description: nastyDesc,
	}); err != nil {
		t.Fatalf("PlanSet(%q): %v", nastyName, err)
	}
	// Control: an ordinary plan.
	if err := launch.PlanSet("shipit", launch.TierPlanProfile{Model: "ollama/kimi-k2"}); err != nil {
		t.Fatalf("PlanSet(shipit): %v", err)
	}

	out := runOAICA(t, "plan", "list")
	lines := outputLines(out)
	if len(lines) != 3 { // header + two plans
		t.Errorf("`plan list` printed %d line(s) for a header + 2 plans — a stored name or description forged a row:\n%s", len(lines), out)
	}
	for _, want := range []string{`"team\n  prod"`, `"first line\n  FABRICATED ROW: opus -> a-model-the-file-never-held"`} {
		if !strings.Contains(out, want) {
			t.Errorf("`plan list` does not show the stored value in its quoted form, want %s in:\n%s", want, out)
		}
	}
	wantControl := fmt.Sprintf("%-20s %-20s %-20s %-20s %-20s %-16s %s",
		"shipit", "ollama/kimi-k2", "(same)", "(same)", "-", "-", "")
	if !strings.Contains(out, wantControl) {
		t.Errorf("the ordinary plan row changed shape — want %q in:\n%s", wantControl, out)
	}
}

// `plan show` prints the name back from argv; it only reaches that line when
// the value IS a stored plan name, so the same rule applies to it.
func TestPlanShowDoesNotPrintAForgedRow(t *testing.T) {
	t.Setenv("OAICA_PLANS_FILE", filepath.Join(t.TempDir(), "plans.json"))

	nastyName := "team\n  prod"
	nastyDesc := "line one\n  FABRICATED ROW: description"
	if err := launch.PlanSet(nastyName, launch.TierPlanProfile{
		Model:       "ollama/glm-5.3-flash:cloud",
		Description: nastyDesc,
	}); err != nil {
		t.Fatalf("PlanSet(%q): %v", nastyName, err)
	}

	out := runOAICA(t, "plan", "show", nastyName)
	lines := outputLines(out)
	if len(lines) != 7 { // name, model, sonnet, haiku, oversize, policy, description
		t.Errorf("`plan show` printed %d line(s) for its 7 fields — a stored value forged a line:\n%s", len(lines), out)
	}
	for _, want := range []string{`"team\n  prod"`, `"line one\n  FABRICATED ROW: description"`} {
		if !strings.Contains(out, want) {
			t.Errorf("`plan show` does not show the stored value in its quoted form, want %s in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "model:        ollama/glm-5.3-flash:cloud\n") {
		t.Errorf("the ordinary field line changed shape:\n%s", out)
	}
}

// A standing preference is a model id the user typed once and then reads back
// here; the setter trims and nothing else.
func TestConfigShowDoesNotPrintAForgedRow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	nasty := "ollama/glm\n  fabricated_model: a-model-the-file-never-held"
	if err := launch.UserConfigSetSonnetModel(nasty); err != nil {
		t.Fatalf("UserConfigSetSonnetModel: %v", err)
	}

	out := runOAICA(t, "config", "show")
	lines := outputLines(out)
	if len(lines) != 3 { // path, sonnet_model, haiku_model
		t.Errorf("`config show` printed %d line(s) for its 3 fields — a stored value forged a line:\n%s", len(lines), out)
	}
	if !strings.Contains(out, `"ollama/glm\n  fabricated_model: a-model-the-file-never-held"`) {
		t.Errorf("`config show` does not show the stored value in its quoted form:\n%s", out)
	}
	// Control: the unset haiku tier prints its ordinary (constant) fallback.
	if !strings.Contains(out, "haiku_model:  (unset — a split launch bills background work on the primary; a plain native launch keeps Claude Code's own Haiku)\n") {
		t.Errorf("the unset-tier line changed shape:\n%s", out)
	}
}

// `router list` prints whatever the router's live registry answers with.
// Nothing on this machine validated it, so it is store-shaped data by the same
// argument (and cmd/launch's own rows for it are quoted).
func TestRouterAuthListDoesNotPrintAForgedRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"name":"prov\n  evil","origin":"https://api.example.com/v1","hasAuth":true,"upstreamModel":"openai/gpt-4o\n  fake-model"},
			{"name":"openai","origin":"https://api.example.com/v1","hasAuth":true}
		]}`))
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)
	t.Setenv("OAICA_ADMIN_KEY", "test-admin-key")

	out := runOAICA(t, "router", "list")
	lines := outputLines(out)
	if len(lines) != 2 {
		t.Errorf("`router list` printed %d line(s) for 2 registered providers — a registry value forged a row:\n%s", len(lines), out)
	}
	for _, want := range []string{`"prov\n  evil"`, `(upstream model: "openai/gpt-4o\n  fake-model")`} {
		if !strings.Contains(out, want) {
			t.Errorf("`router list` does not show the registry value in its quoted form, want %s in:\n%s", want, out)
		}
	}
	wantControl := fmt.Sprintf("  %-28s %-45s %s", "openai", "https://api.example.com/v1", "auth-configured")
	if !strings.Contains(out, wantControl) {
		t.Errorf("the ordinary provider row changed shape — want %q in:\n%s", wantControl, out)
	}
}
