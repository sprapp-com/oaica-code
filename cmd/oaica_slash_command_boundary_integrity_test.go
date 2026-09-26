package cmd

// oaica_slash_command_boundary_integrity_test.go — a piped line whose text
// merely BEGAN with a command name was executed as that command (2026-09-26
// audit).
//
// The one-shot path walks piped stdin line by line and sends each line that is
// not a command to the model, so the dispatcher's match decides whether a
// prompt reaches the model at all. It matched with strings.HasPrefix, so
//
//	printf '/models are slow today\n' | oaica run kat-awq
//
// ran `/model` with the argument "are": the line was consumed by the
// dispatcher, the model never saw it, `oaica usage` shows no request, and the
// command exited 0 having printed "Unknown model 'are'". Anything starting
// with /lora or /agent was the same, and /list or /show in the REPL.
//
// A command is a line whose FIRST TOKEN is exactly the command name — the
// boundary has to be checked, because the alternative is guessing from a
// prefix that prose can share.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnlyAnExactCommandWordDispatches(t *testing.T) {
	for _, tc := range []struct {
		line    string
		name    string
		command bool
	}{
		{"/model", "/model", true},
		{"/model kat-awq", "/model", true},
		{"/model\tkat-awq", "/model", true},
		{"/models are slow today", "/model", false},
		{"/modeling a system prompt", "/model", false},
		{"/lora list", "/lora", true},
		{"/lorem ipsum dolor sit amet", "/lora", false},
		{"/agent summarize this", "/agent", true},
		{"/agentic patterns in RAG", "/agent", false},
		{"/list", "/list", true},
		{"/listing every file", "/list", false},
		{"/help", "/help", true},
		{"/help set", "/help", true},
		{"/helper function to parse", "/help", false},
		{"/exit", "/exit", true},
		{"/byebye", "/bye", false},
	} {
		if got := isSlashCommand(tc.line, tc.name); got != tc.command {
			t.Errorf("isSlashCommand(%q, %q) = %t, want %t", tc.line, tc.name, got, tc.command)
		}
	}
}

// The dispatcher's own answer for the three commands it owns. handled=false is
// what makes the one-shot loop send the line to the model.
func TestProseThatStartsWithACommandNameIsNotDispatched(t *testing.T) {
	model := "kat-awq"
	for _, line := range []string{
		"/models are slow today",
		"/modeling a system prompt",
		"/lorem ipsum dolor sit amet",
		"/agentic patterns in RAG",
	} {
		reply, handled, err := oaicaDispatchLine(line, &model)
		if err != nil {
			t.Fatalf("oaicaDispatchLine(%q): %v", line, err)
		}
		if handled {
			t.Errorf("a piped line %q was consumed by the dispatcher (%q) instead of being sent to the model — the prompt is lost, no request is logged, and the command still exits 0", line, reply)
		}
		if model != "kat-awq" {
			t.Errorf("line %q changed the active model to %q", line, model)
		}
	}

	// And the commands still work.
	model = "kat-awq"
	if _, handled, err := oaicaDispatchLine("/model", &model); err != nil || !handled {
		t.Errorf("/model handled=%t err=%v, want a usage line", handled, err)
	}
	if _, handled, err := oaicaDispatchLine("/lora off", &model); err != nil || !handled {
		t.Errorf("/lora off handled=%t err=%v, want it handled", handled, err)
	}
}

// End to end on the shape the finding reports: a piped prompt line, through
// RunHandler, must reach the router.
func TestAPipedProseLineReachesTheRouter(t *testing.T) {
	stubOaicaRouterModel(t)

	const prompt = "/models are slow today"
	sent := make(chan string, 4)
	oaicaChat = func(model string, msgs []oaicaChatMessage) (string, error) {
		sent <- msgs[len(msgs)-1].Content
		return "noted", nil
	}
	t.Cleanup(func() { oaicaChat = oaicaChatLive })

	path := filepath.Join(t.TempDir(), "stdin.txt")
	if err := os.WriteFile(path, []byte(prompt+"\n"), 0o600); err != nil {
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

	var out string
	out = captureStdout(t, func() {
		if err := RunHandler(oaicaRunCmd(t), []string{"kat-awq"}); err != nil {
			t.Fatalf("RunHandler: %v", err)
		}
	})

	select {
	case got := <-sent:
		if got != prompt {
			t.Errorf("the router received %q, want %q", got, prompt)
		}
	default:
		t.Errorf("the piped line never reached the router — it was consumed as a command.\nstdout was:\n%s", out)
	}
	if strings.Contains(out, "Unknown model") {
		t.Errorf("the piped prose line was parsed as `/model <arg>`:\n%s", out)
	}
}

// The REPL's fall-through: a slash line no command claimed. Prose goes to the
// model, a lone unknown token keeps the typo guard.
func TestASlashLineNoCommandClaimed(t *testing.T) {
	for _, tc := range []struct {
		line string
		want slashDisposition
	}{
		{"/modl kat-awq", slashProse},          // typo WITH an argument: the argument makes it prose
		{"/models are slow today", slashProse}, // the piped case, typed
		{"/tmp/notes.txt summarize this", slashProse},
		{"/modl", slashUnknownCommand}, // a lone unknown token is the typo guard
		{"/models", slashUnknownCommand},
		{"/nonsense", slashUnknownCommand},
	} {
		if got := slashLineDisposition(tc.line); got != tc.want {
			t.Errorf("slashLineDisposition(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// The REPL's own cases have to use the same boundary. A source-level pin,
// because the loop needs an interactive terminal a test cannot supply.
func TestTheReplCommandCasesCheckTheBoundary(t *testing.T) {
	b, err := os.ReadFile("interactive.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, name := range []string{"/model", "/lora", "/agent", "/list", "/show", "/save", "/load", "/set", "/clear", "/exit", "/bye"} {
		if strings.Contains(src, "strings.HasPrefix(line, \""+name+"\")") {
			t.Errorf("interactive.go still matches %q with strings.HasPrefix — a sentence starting with those letters becomes a command, so the message is never sent (use isSlashCommand)", name)
		}
	}
}
