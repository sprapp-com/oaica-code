package cmd

// interactive_oaica_set_integrity_test.go — /set's options in a session served
// by the OAICA API (2026-09-26 audit).
//
// generateInteractive's OAICA gate takes every turn, so the native chat()
// branch that reads opts.Think, opts.Format, opts.Options and opts.System is
// unreachable in this fork. Four of /set's options wrote those fields and
// printed a success no turn could act on:
//
//   - /set system <text> (and the `"""` multi-line form) — the message the user
//     typed was never sent anywhere. This is the one that matters: a system
//     prompt is not decoration, and dropping it in silence is the worst version
//     of the defect;
//   - /set think / /set nothink — thinking is off for every request this fork
//     sends (oaicaChatComplete pins enable_thinking=false, and oaicaChatTimed
//     strips  thinking blocks the backend returns anyway), so "Set 'think'
//     mode." promised the opposite of what happens;
//   - /set format json / /set noformat / /set parameter … — Ollama
//     generation options that the router is never handed.
//
// The system message is now sent ahead of the conversation on every turn; the
// rest say they are not sent, and write nothing.

import (
	"strings"
	"testing"
)

// oaicaChatMessages records what a turn actually sent.
func stubOaicaChatCapturing(t *testing.T, out *[]oaicaChatMessage) {
	t.Helper()
	old := oaicaChat
	oaicaChat = func(_ string, msgs []oaicaChatMessage) (string, error) {
		*out = append([]oaicaChatMessage(nil), msgs...)
		return "ok", nil
	}
	t.Cleanup(func() { oaicaChat = old })
}

// The system message reaches the router, ahead of the conversation — the whole
// point of /set system.
func TestTheSystemMessageIsSentAheadOfTheConversation(t *testing.T) {
	var sent []oaicaChatMessage
	stubOaicaChatCapturing(t, &sent)

	history := []oaicaChatMessage{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}}
	after, _, err := oaicaTurn("kat", history, "and now", "You answer in Malay.", false)
	if err != nil {
		t.Fatal(err)
	}

	if len(sent) != 4 {
		t.Fatalf("a turn with a system message sent %d message(s), want 4 (system, user, assistant, user): %+v", len(sent), sent)
	}
	if sent[0].Role != "system" || sent[0].Content != "You answer in Malay." {
		t.Errorf("the first message sent was %+v, want the system message — /set system used to write opts.System, which only the unreachable native branch reads, so the message was dropped without a word", sent[0])
	}
	if sent[3].Content != "and now" {
		t.Errorf("the user's new turn is at %+v, want it last", sent[3])
	}

	// It belongs to the session, not to the conversation: /clear resets the
	// history and must leave the system message standing, and a copy carried in
	// the history would be re-sent once per turn's copy of itself.
	if len(after) != 4 {
		t.Errorf("the conversation after the turn is %+v — the system message must not be stored in the history it precedes", after)
	}
	for _, m := range after {
		if m.Role == "system" {
			t.Errorf("the system message was stored in the conversation: %+v", after)
			break
		}
	}
}

// No system message means no change to the request: an empty role message is a
// turn the user did not ask for.
func TestNoSystemMessageSendsNoSystemTurn(t *testing.T) {
	for _, system := range []string{"", "   ", "\n"} {
		var sent []oaicaChatMessage
		stubOaicaChatCapturing(t, &sent)

		if _, _, err := oaicaTurn("kat", nil, "hi", system, false); err != nil {
			t.Fatal(err)
		}
		if len(sent) != 1 {
			t.Errorf("system %q: a turn sent %d message(s), want just the user's: %+v", system, len(sent), sent)
		}
		if len(sent) > 0 && sent[0].Role != "user" {
			t.Errorf("system %q: first message sent is %+v, want the user's turn", system, sent[0])
		}
	}
}

// And the settings that cannot be honoured say so instead of printing a
// success. This is the shape that survives: naming the option, and saying it is
// not sent.
func TestAnOptionTheRouterIsNeverSentSaysSo(t *testing.T) {
	out := captureStdout(t, func() {
		oaicaSettingNotSent("think", "Thinking is off for every request this fork sends.")
	})
	if !strings.Contains(out, "think") {
		t.Errorf("the refusal does not name the option it refused: %q", out)
	}
	if !strings.Contains(out, "not sent") {
		t.Errorf("the refusal does not say the setting is not sent: %q", out)
	}
	if strings.Contains(out, "Set '") {
		t.Errorf("the refusal still reads like a success: %q", out)
	}
}

// The /set arms themselves: none of the unhonourable ones may write the field
// the unreachable native branch reads, or reach for a daemon this fork does not
// run. Pinned at the source, like this package's other REPL contracts — the
// loop needs a tty.
func TestTheUnhonouredSetOptionsWriteNothing(t *testing.T) {
	src := interactiveSource(t)
	block := setCaseBlock(t, src)

	for _, arm := range []struct{ name, forbidden string }{
		{"think", "opts.Think"},
		{"nothink", "opts.Think"},
		{"format", "opts.Format"},
		{"noformat", "opts.Format"},
		{"parameter", "opts.Options"},
	} {
		if strings.Contains(subcase(t, block, arm.name), arm.forbidden) {
			t.Errorf("/set %s still writes %s — every turn is served by the OAICA API and the router is never sent it, so the write changes nothing a turn can observe:\n%s", arm.name, arm.forbidden, subcase(t, block, arm.name))
		}
	}
	if strings.Contains(block, "ensureThinkingSupport") {
		t.Errorf("/set still probes a local daemon for thinking support (ensureThinkingSupport) — this fork does not run one, and thinking is pinned off for every request it sends:\n%s", block)
	}
	if !strings.Contains(block, "oaicaSettingNotSent(") {
		t.Errorf("no /set arm tells the user a setting is not sent:\n%s", block)
	}
	// And the one that CAN be honoured must still be wired to the field
	// oaicaTurn reads.
	if !strings.Contains(block, "opts.System =") {
		t.Errorf("/set system no longer records the system message — it is the option oaicaTurn now sends:\n%s", block)
	}
}

// setCaseBlock returns the `/set` arm of generateInteractive's command switch,
// comments stripped, up to the arm after it.
func setCaseBlock(t *testing.T, src string) string {
	t.Helper()
	marker := `case isSlashCommand(line, "/set"):`
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

// subcase returns one `case "<name>":` arm of the /set option switch.
func subcase(t *testing.T, block, name string) string {
	t.Helper()
	marker := `case "` + name + `":`
	start := strings.Index(block, marker)
	if start < 0 {
		t.Fatalf("/set no longer accepts %q — re-read interactive.go before trusting this test", name)
	}
	rest := block[start+len(marker):]
	if end := strings.Index(rest, "\n\t\t\t\tcase "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}
