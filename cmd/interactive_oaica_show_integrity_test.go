package cmd

// interactive_oaica_show_integrity_test.go — /show ended the session it was
// asked to describe (2026-09-26 audit).
//
// The /show arm called api.ClientFromEnvironment() then client.Show — a local
// Ollama daemon, which this fork does not run — and `return err`ed on any
// failure. So in a session served by the OAICA API, /show info printed "error:
// couldn't get model" and ENDED the session: the user asked what model they
// were talking to and lost the conversation.
//
// The three subcommands that ask a daemon for a Modelfile (license, modelfile,
// template) have no equivalent — the router publishes a model's id, description
// and rating, and nothing else — so they say that. /show info answers from the
// router's record, /show system answers from the session, /show parameters
// answers what a session sends.

import (
	"errors"
	"strings"
	"testing"
)

func stubOaicaCatalog(t *testing.T, entries []oaicaModelListEntry, err error) {
	t.Helper()
	old := oaicaListModelsDetailed
	oaicaListModelsDetailed = func() ([]oaicaModelListEntry, error) { return entries, err }
	t.Cleanup(func() { oaicaListModelsDetailed = old })
}

// The router's record is what /show info has to show.
func TestShowInfoAnswersFromTheRoutersRecord(t *testing.T) {
	stubOaicaCatalog(t, []oaicaModelListEntry{
		{ID: "kat-awq", Description: "fast general chat", Stars: 4},
	}, nil)

	var out string
	var err error
	out = captureStdout(t, func() { err = oaicaShowInfo("kat-awq") })
	if err != nil {
		t.Fatalf("showing a model the router lists failed: %v", err)
	}
	for _, want := range []string{"kat-awq", "fast general chat", starString(4)} {
		if !strings.Contains(out, want) {
			t.Errorf("/show info did not print %q:\n%s", want, out)
		}
	}
}

// A composite name (the router's stacked-LoRA syntax) shows the base model's
// record: the router does not enumerate the combination.
func TestShowInfoHandlesAStackedLoraName(t *testing.T) {
	stubOaicaCatalog(t, []oaicaModelListEntry{
		{ID: "kat-awq", Description: "fast general chat", Stars: 4},
	}, nil)

	var out string
	var err error
	out = captureStdout(t, func() { err = oaicaShowInfo("kat-awq+legal+malay") })
	if err != nil {
		t.Fatalf("showing a stacked-LoRA name failed: %v", err)
	}
	if !strings.Contains(out, "fast general chat") {
		t.Errorf("a stacked-LoRA name did not resolve to its base model's record:\n%s", out)
	}
}

// The one that ended sessions: a model the router no longer lists must be
// reported, not returned as an error.
func TestShowInfoSurvivesAModelTheRouterNoLongerLists(t *testing.T) {
	stubOaicaCatalog(t, []oaicaModelListEntry{{ID: "other"}}, nil)

	var out string
	var err error
	out = captureStdout(t, func() { err = oaicaShowInfo("kat-awq") })
	if err != nil {
		t.Errorf("/show info returned an error for a model the router does not list (%v) — the REPL arm `return err`s on that, which ends the session: the user asked what model they were talking to and lost the conversation", err)
	}
	if !strings.Contains(out, "kat-awq") || !strings.Contains(out, "does not list") {
		t.Errorf("/show info said nothing useful about an unlisted model:\n%s", out)
	}
}

// A router that cannot be reached IS an error — and the arm must report it
// rather than end the session.
func TestShowInfoReportsAnUnreachableRouter(t *testing.T) {
	stubOaicaCatalog(t, nil, errors.New("dial tcp: connection refused"))

	var err error
	captureStdout(t, func() { err = oaicaShowInfo("kat-awq") })
	if err == nil {
		t.Error("an unreachable router returned no error from /show info")
	}
}

// The arm itself, pinned at the source: no daemon, no session-ending return.
func TestShowInThisForkDoesNotAskADaemon(t *testing.T) {
	block := slashCaseBlock(t, interactiveSource(t), "/show")

	for _, forbidden := range []string{"client.Show", "ClientFromEnvironment", "ShowRequest"} {
		if strings.Contains(block, forbidden) {
			t.Errorf("/show still calls %s — the daemon it reaches does not exist in this fork, and the call's failure path ended the session:\n%s", forbidden, block)
		}
	}
	if strings.Contains(block, "return err") {
		t.Errorf("/show still ends the session on error — the user asked what model they are talking to:\n%s", block)
	}
	if !strings.Contains(block, "oaicaShowInfo(") {
		t.Errorf("/show info does not answer from the router's record:\n%s", block)
	}
}
