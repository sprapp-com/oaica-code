package cmd

// oaica_session_switch_integrity_test.go — /load and /save in a session served
// by the OAICA API (2026-09-26 audit).
//
// generateInteractive's gate — `sb.Len() > 0 && multiline == MultilineNone &&
// oaicaActiveModel != ""` — takes every turn, and oaicaActiveModel is seeded
// from opts.Model, which cobra requires a non-empty value for (cmd.go's
// oaicaModelExists(args[0]) short-circuit). So the Ollama-native chat() branch
// below it is unreachable in this fork, and every command that existed only to
// configure that branch was promising something the session could not deliver:
//
//   - /load ran client.Show + applyShowResponseToRunOptions +
//     loadOrUnloadModel against a daemon this fork does not run. It changed
//     opts.Model, which nothing on the OAICA path reads, so the turns kept
//     going to the model that was already active while the command printed
//     success — and any failure that was not "not found" (no local server, for
//     one) RETURNED, ending the session;
//   - /save built a local Ollama model out of the session's run options
//     (NewCreateRequest → client.Create) that had nothing to do with the turns
//     it claimed to save, and ended the session on the same class of failure.
//
// The fix routes /load through the same act as /model and refuses /save with
// the reason. Both now stay in the session. The REPL itself needs a tty and is
// pinned at its call sites, the way this package's other REPL contracts are
// (see oaica_run_flags_test.go's TestTheInteractiveTurnHonoursVerbose).

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// interactiveSource returns cmd/interactive.go's text.
func interactiveSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("interactive.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// slashCaseBlock returns the body of one `case isSlashCommand(line, "<cmd>")`
// arm of generateInteractive's switch, from the case label to the next case
// label, with comment lines removed. Comments are stripped because the code
// these tests are about explains what it no longer calls, by name — scanning
// the prose would fail the arm for describing its own fix. A test that cannot
// find the arm fails rather than passing vacuously.
func slashCaseBlock(t *testing.T, src, cmd string) string {
	t.Helper()
	marker := `case isSlashCommand(line, "` + cmd + `"):`
	start := strings.Index(src, marker)
	if start < 0 {
		t.Fatalf("generateInteractive no longer has a %s arm — re-read interactive.go before trusting this test", marker)
	}
	rest := src[start+len(marker):]
	if end := strings.Index(rest, "\n\t\tcase "); end >= 0 {
		rest = rest[:end]
	}
	var code []string
	for _, l := range strings.Split(rest, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		code = append(code, l)
	}
	return strings.Join(code, "\n")
}

// The switch is what /model and /load both do now, and it must be able to fail
// without ending a session for a typo.
func TestSwitchingToAKnownModelRepointsTheSession(t *testing.T) {
	stubOaicaRouterModel(t)

	active := "some-other-model"
	history := []oaicaChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}}

	var out string
	var switched bool
	var err error
	out = captureStdout(t, func() {
		switched, err = oaicaSwitchActiveModel("kat-awq", &active, &history)
	})
	if err != nil {
		t.Fatalf("switching to a model the router serves failed: %v", err)
	}
	if !switched {
		t.Fatalf("switching to a model the router serves reported no switch: %q", out)
	}
	if active != "kat-awq" {
		t.Errorf("active model after switching = %q, want kat-awq — the turns keep going to the model that was already active, which is what made /load print a success about nothing", active)
	}
	if history != nil {
		t.Errorf("conversation survived a model switch: %d messages — the new model would be handed turns the previous one answered", len(history))
	}
}

// A name the router does not serve is not an error and not a switch: the user
// is shown what exists and the session stays exactly where it was.
func TestSwitchingToAnUnknownModelKeepsTheSession(t *testing.T) {
	stubOaicaRouterModel(t)

	active := "kat-awq"
	history := []oaicaChatMessage{{Role: "user", Content: "hi"}}

	var out string
	var switched bool
	var err error
	out = captureStdout(t, func() {
		switched, err = oaicaSwitchActiveModel("no-such-model", &active, &history)
	})
	if err != nil {
		t.Errorf("an unknown model name returned an error (%v) — this is the value /load used to `return err` on, which ends the session for a typo", err)
	}
	if switched {
		t.Error("an unknown model name reported a switch")
	}
	if active != "kat-awq" {
		t.Errorf("active model changed to %q on an unknown name, want kat-awq", active)
	}
	if len(history) != 1 {
		t.Errorf("conversation was cleared by a switch that did not happen: %d messages, want 1", len(history))
	}
	if !strings.Contains(out, "no-such-model") {
		t.Errorf("an unknown model name said nothing about it: %q", out)
	}
	if !strings.Contains(out, "kat-awq") {
		t.Errorf("an unknown model name did not list what the router does serve: %q", out)
	}
}

// The one failure that IS an error: a router that cannot be reached. The
// session must not be repointed on a guess.
func TestAnUnreachableRouterIsTheOnlySwitchError(t *testing.T) {
	old := oaicaListModelsDetailed
	oaicaListModelsDetailed = func() ([]oaicaModelListEntry, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	t.Cleanup(func() { oaicaListModelsDetailed = old })

	active := "kat-awq"
	history := []oaicaChatMessage{{Role: "user", Content: "hi"}}

	var switched bool
	var err error
	captureStdout(t, func() {
		switched, err = oaicaSwitchActiveModel("kat-awq", &active, &history)
	})
	if err == nil {
		t.Error("an unreachable router returned no error")
	}
	if switched {
		t.Error("an unreachable router reported a switch — the session was repointed without ever confirming the model exists")
	}
	if active != "kat-awq" {
		t.Errorf("active model = %q after a failed lookup", active)
	}
}

// /load must be the same act as /model: no daemon call, no `return`, and the
// switch goes through the shared function.
func TestLoadInThisForkDoesNotCallADaemon(t *testing.T) {
	block := slashCaseBlock(t, interactiveSource(t), "/load")

	for _, forbidden := range []string{"client.Show", "loadOrUnloadModel", "applyShowResponseToRunOptions", "ClientFromEnvironment"} {
		if strings.Contains(block, forbidden) {
			t.Errorf("/load still calls %s — this fork serves every turn from the OAICA API, so that call configures a process the session never talks to while printing a success about it:\n%s", forbidden, block)
		}
	}
	if !strings.Contains(block, "oaicaSwitchActiveModel(") {
		t.Errorf("/load does not switch the active model through oaicaSwitchActiveModel, the function /model uses — the two commands have to be the same act:\n%s", block)
	}
	if strings.Contains(block, "return err") {
		t.Errorf("/load still ends the session on error — a failed switch must report and let the session continue:\n%s", block)
	}
}

// /save must not build a local model, and must not end the session.
func TestSaveInThisForkDoesNotBuildALocalModel(t *testing.T) {
	block := slashCaseBlock(t, interactiveSource(t), "/save")

	for _, forbidden := range []string{"NewCreateRequest", "client.Create", "ClientFromEnvironment"} {
		if strings.Contains(block, forbidden) {
			t.Errorf("/save still calls %s — the model it builds is a local Ollama one, unrelated to the turns this session is serving:\n%s", forbidden, block)
		}
	}
	if strings.Contains(block, "return err") {
		t.Errorf("/save still ends the session on error:\n%s", block)
	}
	if !strings.Contains(block, "unavailable") {
		t.Errorf("/save does not tell the user it is unavailable — refusing silently is not better than the success it used to print:\n%s", block)
	}
}

// And the help must not advertise /save as a working command.
func TestTheHelpDoesNotOfferSave(t *testing.T) {
	src := interactiveSource(t)
	usage := src[strings.Index(src, "usage := func()"):]
	if end := strings.Index(usage, "usageSet := func()"); end >= 0 {
		usage = usage[:end]
	}
	if strings.Contains(usage, "/save") {
		t.Errorf("the command list still offers /save, which this session refuses:\n%s", usage)
	}
	if !strings.Contains(usage, "/load") {
		t.Error("the command list no longer mentions /load")
	}
}
