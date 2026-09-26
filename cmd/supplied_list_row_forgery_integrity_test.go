package cmd

// supplied_list_row_forgery_integrity_test.go — the one-shot dispatcher and the
// typo path printed router-supplied and store-supplied strings raw, so a
// control character in one forged rows in a listing the user trusts
// (2026-09-26 audit, fifteenth round).
//
// Round 13 fixed the interactive twin (oaicaPrintModelList, oaicaPrintLoraList,
// oaicaSwitchActiveModel's hint) through launch.PrintableCell; the same
// rendering lives a second time in the piped/one-shot path (oaicaDispatchLine)
// and in `oaica run <typo>`'s list, and those were left raw. Two
// implementations of one list, one fixed.
//
// The sources are the router's /v1/models answer (`id`, `description`) and its
// /v1/lora answer (name, model).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// forgeModelList answers /v1/models with one ordinary model and one whose id
// and description carry a newline — the shape a hostile or buggy catalog
// contributes.
func forgeModelList(t *testing.T) {
	t.Helper()
	entries := []oaicaModelListEntry{
		{ID: "real-model", Description: "what it is for", Stars: 3},
		{ID: "evil\n  fake-admin-row", Description: "first\n  second-best-for", Stars: 5},
		// A clean id with a hostile DESCRIPTION, so the panel's own field can
		// be exercised: oaicaShowInfo matches on the id, so the entry above is
		// unreachable by lookup.
		{ID: "desc-model", Description: "first\n  second-best-for", Stars: 5},
	}
	old := oaicaListModelsDetailed
	oaicaListModelsDetailed = func() ([]oaicaModelListEntry, error) { return entries, nil }
	t.Cleanup(func() { oaicaListModelsDetailed = old })
}

func TestOneShotDispatcherDoesNotForgeRowsFromRouterModels(t *testing.T) {
	forgeModelList(t)

	m := "real-model"
	out, _, err := oaicaDispatchLine("/model list", &m)
	if err != nil {
		t.Fatalf("oaicaDispatchLine: %v", err)
	}

	if strings.Contains(out, "\n  fake-admin-row") {
		t.Errorf("a model id carrying a newline forged a row of its own in the one-shot model list:\n%s", out)
	}
	if strings.Contains(out, "\n  second-best-for") {
		t.Errorf("a model description carrying a newline forged a line of its own:\n%s", out)
	}
	// Quoting, not dropping: the real entries must still be readable.
	for _, want := range []string{"real-model", "what it is for", "evil"} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing lost %q instead of quoting it:\n%s", want, out)
		}
	}
}

// The unknown-model hint is the correction list shown right after an error —
// the moment a user is most likely to copy a name verbatim.
func TestOneShotDispatcherDoesNotForgeRowsFromTheUnknownModelHint(t *testing.T) {
	forgeModelList(t)

	m := "real-model"
	out, _, err := oaicaDispatchLine("/model nothing-like-this", &m)
	if err != nil {
		t.Fatalf("oaicaDispatchLine: %v", err)
	}

	if strings.Contains(out, "\n  fake-admin-row") {
		t.Errorf("the unknown-model hint listed a router-supplied name raw:\n%s", out)
	}
}

func TestOneShotDispatcherDoesNotForgeRowsFromLoraNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"name":"adapter\n  fake-lora-row","model":"kat-awq","id":1}]}`)
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)

	m := "kat-awq"
	out, _, err := oaicaDispatchLine("/lora list", &m)
	if err != nil {
		t.Fatalf("oaicaDispatchLine: %v", err)
	}

	if strings.Contains(out, "\n  fake-lora-row") {
		t.Errorf("a LoRA name carrying a newline forged a row in the adapter list:\n%s", out)
	}
	if !strings.Contains(out, "adapter") {
		t.Errorf("the adapter vanished instead of being quoted:\n%s", out)
	}
}

// `oaica run <typo>` reaches the same list through the top-level command.
func TestRunHandlerUnknownModelHintDoesNotForgeRows(t *testing.T) {
	forgeModelList(t)
	oaicaChat = func(string, []oaicaChatMessage) (string, error) { return "", nil }
	t.Cleanup(func() { oaicaChat = oaicaChatLive })
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")

	var runErr error
	out := captureStdout(t, func() {
		runErr = RunHandler(oaicaRunCmd(t), []string{"nope", "hi"})
	})
	if runErr == nil {
		t.Fatal("premise: an unknown model must still return an error")
	}
	if strings.Contains(out, "\n  fake-admin-row") {
		t.Errorf("`oaica run <typo>` listed a router-supplied name raw, so the correction list it prints after the error contains a row the router invented:\n%s", out)
	}
}

// /show info renders the router's free-text description inside a panel.
func TestShowInfoDoesNotForgeFromRouterDescription(t *testing.T) {
	forgeModelList(t)

	out := captureStdout(t, func() {
		if err := oaicaShowInfo("real-model"); err != nil {
			t.Fatalf("oaicaShowInfo: %v", err)
		}
	})
	if !strings.Contains(out, "what it is for") {
		t.Fatalf("premise: the info panel did not print the description:\n%s", out)
	}

	forgeModelList(t)
	// Now the model whose description carries the newline.
	out = captureStdout(t, func() {
		if err := oaicaShowInfo("desc-model"); err != nil {
			t.Fatalf("oaicaShowInfo: %v", err)
		}
	})
	if !strings.Contains(out, "best for: ") {
		t.Fatalf("premise: the panel printed no description at all:\n%s", out)
	}
	if strings.Contains(out, "best for: first\n  second-best-for") {
		t.Errorf("/show info printed the router's description raw, so it emitted arbitrary lines inside the panel:\n%s", out)
	}
}

// A failing logout printed the router's whole body into the error, while every
// sibling in the same file truncates through oaicaDiagnosisBody.
func TestLogoutFailureDoesNotDumpTheWholeRouterBody(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, big)
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)
	t.Setenv("OAICA_ADMIN_KEY", "admin-test")

	err := oaicaAuthLogout("groq")
	if err == nil {
		t.Fatal("premise: a 500 from the router should fail the logout")
	}
	if len(err.Error()) > 4096 {
		t.Errorf("the logout error carries %d bytes of the router's body — a broken or hostile router flushes its whole answer (up to 64 MiB) into the terminal and into whatever log or ticket the user pastes it into; the siblings in this file bound it through oaicaDiagnosisBody", len(err.Error()))
	}
}

// Guard: the JSON literals above must stay valid, or the stub silently answers
// nothing and every assertion above passes vacuously.
func TestForgeTestLiteralsAreValidJSON(t *testing.T) {
	for _, s := range []string{`{"data":[{"name":"adapter\n  fake-lora-row","model":"kat-awq","id":1}]}`} {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatalf("stub JSON is invalid: %v", err)
		}
	}
}
