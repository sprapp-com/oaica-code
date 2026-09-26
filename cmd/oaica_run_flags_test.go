package cmd

// oaica_run_flags_test.go — the `oaica run` flag surface, pinned to what the
// flags DO rather than to what their help says.
//
// Three flags inherited from upstream Ollama fed an embedding path this fork
// does not have: RunHandler never builds an api.EmbedRequest, so --truncate,
// --dimensions and --insecure all parsed and did nothing — --insecure's name
// promising a security posture nothing changed, --truncate's help promising a
// "(default: true)" behaviour its own registration contradicted. They were
// removed rather than silently accepted, so an ollama-migrating script fails
// loudly instead of believing they took effect (2026-09-26 audit). `push`,
// `pull` and `serve` keep their own --insecure, which those flows read.
//
// Second round (2026-09-26 audit): four more flags had readers that `oaica run`
// cannot reach. --format, --think, --keepalive and --hidethinking are all read
// in chat() or generate() — the native Ollama path — and RunHandler returns
// from its OAICA short-circuit (cmd/cmd.go) on every input shape: an
// interactive terminal goes to generateInteractive, anything else to the
// line-by-line one-shot loop, and both of those talk to the router through
// oaicaChat. So `oaica run --format json <model> ... | jq` printed prose and
// exited 0, and `--think true` asserted the opposite of what the router path
// does (oaicaChatLive pins chat_template_kwargs.enable_thinking=false and
// strips any  thinking block). Removed on the same rule as the three above: a
// flag whose help promises something no reachable code can deliver is worse
// than a missing one, because the script that passes it keeps running.
//
// --verbose had the SAME unreachable reader set (chat() calls
// latest.Summary()) — and that one is worth keeping, so it was given a reader
// on the router path instead (oaicaChatTimed, cmd/oaica_client.go).
//
// The remaining finding was the opposite mistake: a flag whose help
// under-described it. --tool-format accepts four values (freeform, xml and
// none are enforced by validRemoteToolFormats in launch/remote_cli.go and
// reachable from `oaica remote add`) and the help named two.

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// cmdGoSource returns cmd/cmd.go's text. These assertions are on the
// REGISTRATION lines (the flag set is a local inside the command builder, so
// there is no package-level value to inspect).
func cmdGoSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// registrationLine returns the first line containing marker, trimmed.
func registrationLine(t *testing.T, src, marker string) string {
	t.Helper()
	for _, l := range strings.Split(src, "\n") {
		if strings.Contains(l, marker) {
			return strings.TrimSpace(l)
		}
	}
	t.Fatalf("cmd/cmd.go no longer contains %q — re-read it before trusting this test", marker)
	return ""
}

func TestRunRegistersNoFlagNothingReads(t *testing.T) {
	src := cmdGoSource(t)

	// --truncate and --dimensions: `run` never sends an embed request, so
	// nothing could read them. --insecure: RunHandler contains no reference
	// to it; the only GetBool("insecure") sites in cmd/ belong to push, pull
	// and serve. The second group is the 2026-09-26 second round: every reader
	// of these four lives in chat()/generate(), which the OAICA short-circuit
	// makes unreachable from `run`.
	for _, gone := range []string{
		`runCmd.Flags().Bool("insecure"`,
		`runCmd.Flags().Bool("truncate"`,
		`runCmd.Flags().Int("dimensions"`,
		`runCmd.Flags().String("format"`,
		`runCmd.Flags().String("think"`,
		`runCmd.Flags().String("keepalive"`,
		`runCmd.Flags().Bool("hidethinking"`,
	} {
		if strings.Contains(src, gone) {
			t.Errorf("cmd/cmd.go registers %s again — `oaica run` has no code path that reads it, so the flag would parse and do nothing (see this file's header for the 2026-09-26 audit findings)", gone)
		}
	}

	// --verbose is the one flag of that group kept: it now has a reader on the
	// router path, pinned behaviorally by
	// TestRunVerboseReportsTimingsOnTheRouterPath.
	if !strings.Contains(src, `runCmd.Flags().Bool("verbose"`) {
		t.Errorf("cmd/cmd.go no longer registers runCmd's --verbose, but the router path reads it (oaicaChatTimed) — removing the reader is not the fix")
	}

	// The flows that DO read --insecure must keep it.
	for _, kept := range []string{
		`pushCmd.Flags().Bool("insecure"`,
		`serveCmd.Flags().Bool("insecure"`,
	} {
		if !strings.Contains(src, kept) {
			t.Errorf("cmd/cmd.go no longer registers %s, but that flow reads the flag — removing the reader is not the fix", kept)
		}
	}
}

// The removal has to be visible to the user, not just absent from the source:
// a script (or a person) passing one of the removed flags must get an error
// and exit non-zero instead of a reply. Cobra rejects an unregistered flag
// while parsing, before RunE runs, so this never touches the network.
func TestRunRefusesTheFlagsWhoseOnlyReadersAreUnreachable(t *testing.T) {
	for _, flag := range []string{"--format", "--think", "--keepalive", "--hidethinking"} {
		root := NewCLI()
		var stderr bytes.Buffer
		root.SetOut(io.Discard)
		root.SetErr(&stderr)
		root.SetArgs([]string{"run", flag, "some-model", "hello"})

		err := root.Execute()
		if err == nil {
			t.Errorf("`oaica run %s some-model hello` exited 0 — the flag was accepted by a code path that cannot honour it, which is how `--format json | jq` ended up emitting prose with a success status", flag)
			continue
		}
		if !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("`oaica run %s ...` failed with %q, want an unknown-flag error naming the flag", flag, err)
		}
	}
}

// The router path's one-shot reply goes to stdout, so anything --verbose
// prints must NOT: `oaica run <model> "prompt" > answer.txt` and
// `oaica run <model> "prompt" | jq` are the documented one-shot shapes, and a
// timing line on stdout would be captured with the answer.
func TestRunVerboseReportsTimingsOnTheRouterPath(t *testing.T) {
	stubOaicaRouterModel(t)

	// Both are stubbed: the metered call is what a verbose run must use, and
	// stubbing the plain one too keeps a regression that calls IT from
	// reaching the real router — the failure then names the missing timing
	// instead of an unrelated HTTP error from api.oaica.com.
	oaicaChat = func(model string, msgs []oaicaChatMessage) (string, error) { return "4", nil }
	oaicaChatMetered = func(model string, msgs []oaicaChatMessage) (string, oaicaChatUsage, error) {
		return "4", oaicaChatUsage{PromptTokens: 11, CompletionTokens: 1, TotalTokens: 12}, nil
	}
	t.Cleanup(func() {
		oaicaChat = oaicaChatLive
		oaicaChatMetered = oaicaChatMeteredLive
	})

	var out, errOut string
	out = captureStdout(t, func() {
		errOut = captureStderr(t, func() {
			cmd := oaicaRunCmd(t)
			if err := cmd.Flags().Set("verbose", "true"); err != nil {
				t.Fatal(err)
			}
			if err := RunHandler(cmd, []string{"kat-awq", "what is 2+2"}); err != nil {
				t.Fatalf("RunHandler: %v", err)
			}
		})
	})

	if !strings.Contains(errOut, "total duration") {
		t.Errorf("--verbose printed no timing on the router path; stderr had %q — the flag's help has always promised \"Show timings for response\" and its only reader was chat()'s Summary(), unreachable from `run` (2026-09-26 audit)", errOut)
	}
	if !strings.Contains(errOut, "11") || !strings.Contains(errOut, "1") {
		t.Errorf("--verbose did not report the backend's token counts; stderr had %q", errOut)
	}
	if strings.Contains(out, "total duration") {
		t.Errorf("the timing line went to STDOUT, where it is captured with the reply:\n%s", out)
	}
	if !strings.Contains(out, "4") {
		t.Errorf("the reply did not reach stdout: %q", out)
	}
}

// Verbose off must stay quiet, and a backend that reports no usage must not be
// described as if it did.
func TestRunIsQuietWithoutVerboseAndHonestAboutMissingUsage(t *testing.T) {
	stubOaicaRouterModel(t)

	oaicaChat = func(model string, msgs []oaicaChatMessage) (string, error) { return "4", nil }
	oaicaChatMetered = func(model string, msgs []oaicaChatMessage) (string, oaicaChatUsage, error) {
		return "4", oaicaChatUsage{}, nil
	}
	t.Cleanup(func() {
		oaicaChat = oaicaChatLive
		oaicaChatMetered = oaicaChatMeteredLive
	})

	var plainOut, plainErr string
	plainOut = captureStdout(t, func() {
		plainErr = captureStderr(t, func() {
			if err := RunHandler(oaicaRunCmd(t), []string{"kat-awq", "hi"}); err != nil {
				t.Fatal(err)
			}
		})
	})
	if strings.Contains(plainOut, "total duration") || strings.Contains(plainErr, "total duration") {
		t.Errorf("timings printed with --verbose off: stdout=%q stderr=%q", plainOut, plainErr)
	}

	var silentErr string
	captureStdout(t, func() {
		silentErr = captureStderr(t, func() {
			cmd := oaicaRunCmd(t)
			if err := cmd.Flags().Set("verbose", "true"); err != nil {
				t.Fatal(err)
			}
			if err := RunHandler(cmd, []string{"kat-awq", "hi"}); err != nil {
				t.Fatal(err)
			}
		})
	})
	if !strings.Contains(silentErr, "total duration") {
		t.Errorf("--verbose printed no duration at all: %q", silentErr)
	}
	if !strings.Contains(silentErr, "not reported") {
		t.Errorf("a backend that sent no usage was reported as if it had, or said nothing: %q — a verbose mode that silently omits half its line is the same inert promise this file is about", silentErr)
	}
}

// The REPL path is the other reader. Its turn helper takes verbose explicitly,
// and generateInteractive — an interactive terminal loop, which a test cannot
// drive without a tty — is pinned at its call site instead.
func TestTheInteractiveTurnHonoursVerbose(t *testing.T) {
	stubOaicaRouterModel(t)
	oaicaChatMetered = func(model string, msgs []oaicaChatMessage) (string, oaicaChatUsage, error) {
		return "hello", oaicaChatUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}, nil
	}
	t.Cleanup(func() { oaicaChatMetered = oaicaChatMeteredLive })

	var errOut string
	var after []oaicaChatMessage
	captureStdout(t, func() {
		errOut = captureStderr(t, func() {
			var err error
			after, _, err = oaicaTurn("kat-awq", nil, "hi", "", true)
			if err != nil {
				t.Fatalf("oaicaTurn: %v", err)
			}
		})
	})
	if !strings.Contains(errOut, "total duration") {
		t.Errorf("/set verbose (the REPL's only way to ask for timings) produced none: %q", errOut)
	}
	if len(after) != 2 {
		t.Errorf("conversation after one turn = %d messages, want the user turn and the reply", len(after))
	}

	src, err := os.ReadFile("interactive.go")
	if err != nil {
		t.Fatal(err)
	}
	// The same line carries the `/set system` message: both are per-turn state
	// the REPL re-reads, and both were fields oaicaTurn never received.
	if !strings.Contains(string(src), "oaicaTurn(oaicaActiveModel, oaicaHistory, sb.String(), opts.System, oaicaVerboseRequested(cmd))") {
		t.Errorf("generateInteractive no longer passes the session's verbose state and system message into oaicaTurn — `oaica run --verbose` would report timings in one-shot mode and stay silent in the REPL, and a `/set system` message would be dropped without a word (see interactive_oaica_set_integrity_test.go)")
	}
}

func TestToolFormatHelpNamesEveryAcceptedValue(t *testing.T) {
	src := cmdGoSource(t)
	line := registrationLine(t, src, `Flags().String("tool-format"`)

	// validRemoteToolFormats (launch/remote_cli.go) is the enforced set; the
	// help lives here and cannot see it, so the values are repeated — a value
	// added there without the help would make this test fail, which is the
	// point.
	for _, v := range []string{"tool_calls", "freeform", "xml", "none"} {
		if !strings.Contains(line, v) {
			t.Errorf("--tool-format accepts %q but its help does not name it:\n  %s", v, line)
		}
	}
	// The default is inferred from the wire (launch/user_remotes.go's
	// Descriptor), so a help that names only one default is half the story.
	for _, v := range []string{"openai", "anthropic"} {
		if !strings.Contains(line, v) {
			t.Errorf("--tool-format's help does not say what the default is for the %s wire:\n  %s", v, line)
		}
	}
}

// captureStderr is captureStdout's counterpart (cmd/cmd_test.go), for the
// assertions above about where --verbose's lines must NOT go. A pipe is used
// rather than a bytes.Buffer because the code under test holds os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	b, _ := io.ReadAll(r)
	return string(b)
}

// stubOaicaRouterModel makes RunHandler's OAICA short-circuit see a routable
// model without a network.
func stubOaicaRouterModel(t *testing.T) {
	t.Helper()
	oaicaListModelsDetailed = func() ([]oaicaModelListEntry, error) {
		return []oaicaModelListEntry{{ID: "kat-awq"}}, nil
	}
	t.Cleanup(func() {
		oaicaListModelsDetailed = func() ([]oaicaModelListEntry, error) { return nil, nil }
	})
}
