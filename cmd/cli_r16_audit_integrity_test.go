package cmd

// cli_r16_audit_integrity_test.go — six CLI defects (2026-09-26 audit,
// sixteenth round).
//
//	1. /lora add|remove printed the router's `model` field raw, so a control
//	   character in it forged a status line ("Switched to model …").
//	2. oaica site's unknown-model hint joined the router's model ids raw.
//	3. oaica site's router-error branch returned the body message unbounded and
//	   unredacted, while its two sibling branches truncated at 300 bytes.
//	4. the manifest error path did the same, on the one request that carries
//	   the distribution licence as a bearer.
//	5. `oaica run m "question" < file` silently dropped the argument prompt.
//	6. `oaica gpu ps`/`gpu clean` printed a process's own argv with newlines
//	   intact, so a holder could forge a row in the pre-kill list.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/internal/sitebuilder"
)

// (1) The router's model field must not forge a status line.
func TestLoraToggleDoesNotForgeStatusLinesFromTheRouterModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"model":"kat-awq\nSwitched to model 'claude-9-opus' (forged status line)"}`)
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)

	for _, line := range []string{"/lora add my-lora", "/lora remove my-lora"} {
		m := "kat-awq"
		out, _, err := oaicaDispatchLine(line, &m)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		if strings.Contains(out, "\nSwitched to model 'claude-9-opus'") {
			t.Errorf("%s printed the router's model field raw, so it emitted a forged status line of its own:\n%s", line, out)
		}
		if !strings.Contains(out, "kat-awq") {
			t.Errorf("%s lost the model name instead of quoting it:\n%s", line, out)
		}
	}
}

// (1b) The REPL's /lora add|remove is the same print site in a second file.
// The arm is reached only from generateInteractive's readline loop, which needs
// a terminal, so this pins the arm to the helper the same way
// interactive_router_row_forgery_integrity_test.go does — the rule lives in
// launch.PrintableCell, and the arm must not print the router's model raw.
func TestTheInteractiveLoraToggleQuotesTheRouterModel(t *testing.T) {
	block := slashCaseBlock(t, interactiveSource(t), "/lora")

	if n := strings.Count(block, "launch.PrintableCell(model)"); n != 2 {
		t.Errorf("/lora's add/remove arms quote the router's model %d time(s), want 2 (one per arm):\n%s", n, block)
	}
	if strings.Contains(block, "args[2], model)") {
		t.Errorf("/lora prints the router's model raw instead of quoting it:\n%s", block)
	}
}

// (2) The corrective list after a typo is still a list of the router's names.
func TestSiteUnknownModelHintDoesNotForgeRows(t *testing.T) {
	oaicaListModelsDetailed = func() ([]oaicaModelListEntry, error) {
		return []oaicaModelListEntry{
			{ID: "real-model"},
			{ID: "forged\n  fake-admin-row  admin  https://evil.example"},
		}, nil
	}
	t.Cleanup(func() { oaicaListModelsDetailed = oaicaListModelsDetailedLive })

	_, err := siteLLMForModel("nothing-like-this")
	if err == nil {
		t.Fatal("premise: an unknown model must fail before the plan call")
	}
	if strings.Contains(err.Error(), "\n  fake-admin-row") {
		t.Errorf("the unknown-model hint listed a router-supplied id raw:\n%v", err)
	}
	if !strings.Contains(err.Error(), "real-model") {
		t.Errorf("the hint lost the real names instead of quoting them:\n%v", err)
	}
}

// (3) A router error message is bounded and carries no credential.
func TestSiteErrorIsBoundedAndRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"`+strings.Repeat("x", 200<<10)+` invalid api key: r16-dummy-key"}}`)
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)
	t.Setenv("OAICA_API_KEY", "r16-dummy-key")

	llm := routerLLM{model: "m"}
	_, err := llm.Complete(context.Background(), sitebuilder.Request{System: "s", User: "u"})
	if err == nil {
		t.Fatal("premise: a 502 from the router must fail the site call")
	}
	if len(err.Error()) > 4096 {
		t.Errorf("the site error carries %d bytes of the router's body — a broken or hostile router flushes its whole answer (up to %d bytes) into the terminal and into whatever log the user pastes it into:\n%.200s…", len(err.Error()), maxChatResponseBytes, err.Error())
	}
	if strings.Contains(err.Error(), "r16-dummy-key") {
		t.Errorf("the router's error message echoed the API key and it reached the terminal verbatim:\n%s", err.Error())
	}
}

// (4) The manifest error carries the licence key on the wire; the message that
// comes back must be bounded and must not echo it.
func TestManifestErrorIsBoundedAndRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"type":"license_invalid","message":"`+strings.Repeat("y", 200<<10)+` rejected bearer r16-licence-key"}}`)
	}))
	defer srv.Close()
	t.Setenv("OAICA_HOST", srv.URL)
	t.Setenv("OAICA_LICENSE_KEY", "r16-licence-key")

	_, err := oaicaFetchManifest("kat-awq")
	if err == nil {
		t.Fatal("premise: a 402 from the router must fail the manifest fetch")
	}
	if len(err.Error()) > 4096 {
		t.Errorf("the manifest error carries %d bytes of the router's body:\n%.200s…", len(err.Error()), err.Error())
	}
	if strings.Contains(err.Error(), "r16-licence-key") {
		t.Errorf("the router echoed the licence key and it reached the terminal verbatim:\n%s", err.Error())
	}
}

// (5) A prompt given as an argument is not discarded for having piped stdin.
func TestRunHandlerWithPipedStdinKeepsTheArgumentPrompt(t *testing.T) {
	oaicaListModelsDetailed = func() ([]oaicaModelListEntry, error) {
		return []oaicaModelListEntry{{ID: "kat-awq"}}, nil
	}
	t.Cleanup(func() { oaicaListModelsDetailed = func() ([]oaicaModelListEntry, error) { return nil, nil } })

	var sent []string
	oaicaChat = func(model string, msgs []oaicaChatMessage) (string, error) {
		sent = append(sent, msgs[len(msgs)-1].Content)
		return "ok", nil
	}
	t.Cleanup(func() { oaicaChat = oaicaChatLive })
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")

	path := filepath.Join(t.TempDir(), "stdin.txt")
	if err := os.WriteFile(path, []byte("piped line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	oldStdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = oldStdin })

	captureStdout(t, func() {
		if err := RunHandler(oaicaRunCmd(t), []string{"kat-awq", "ARGUMENT QUESTION"}); err != nil {
			t.Fatalf("RunHandler: %v", err)
		}
	})

	var sawArg bool
	for _, s := range sent {
		if strings.Contains(s, "ARGUMENT QUESTION") {
			sawArg = true
		}
	}
	if !sawArg {
		t.Errorf("the prompt given as an argument was dropped because stdin was piped, so the question the user typed was never asked (sent: %q)", sent)
	}
	if len(sent) == 0 || !strings.Contains(sent[0], "piped line") {
		t.Errorf("the piped input no longer reaches the model (sent: %q)", sent)
	}
}

// (6) A process's own argv cannot forge a row in the pre-kill list.
func TestGPUProcInfoFoldsControlRunesInArgv(t *testing.T) {
	// argv[0] is the process's own text: give it a newline and a fake row.
	cmd := exec.Command("/bin/sleep", "30")
	cmd.Args = []string{"VLLM::EngineCore\n  PID 1 (9999 MiB): /usr/bin/innocent", "30"}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a test process: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	// /proc/PID/cmdline is transiently empty right after Start, and procInfo
	// falls back to /proc/PID/stat's comm ("sleep") when it is — so the loop
	// waits for the argv itself, not merely for a non-empty answer.
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, cmdLine, ok := procInfo(cmd.Process.Pid)
		if ok && strings.Contains(cmdLine, "EngineCore") {
			if strings.ContainsAny(cmdLine, "\n\r") {
				t.Errorf("procInfo returned a command line with a newline in it (%q) — the row an operator reads before a kill can be forged by the process being killed", cmdLine)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("premise: the test process's argv never appeared in /proc (last read: %q)", cmdLine)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
