package cmd

// interactive_oaica_history_integrity_test.go — the interactive session's two
// views of the conversation drifted apart (2026-09-26 audit).
//
// The OAICA thin-client path keeps its own transcript (oaicaHistory, the
// OpenAI-shaped messages it posts to the router) alongside the native path's
// opts.Messages. Two things were only ever done to one of them:
//
//  1. /clear (and /model, which starts a new conversation too) emptied
//     opts.Messages and left oaicaHistory untouched, so "Cleared session
//     context" was true on one path and false on the one the user was actually
//     talking through — the next prompt carried the whole cleared
//     conversation to the router;
//  2. a turn whose request FAILED stayed in oaicaHistory, so every later
//     request re-sent a prompt the model never answered, once more for each
//     retry, growing the context with turns the user never got a reply to.

import (
	"errors"
	"testing"

	"github.com/ollama/ollama/api"
)

func stubOaicaChat(t *testing.T, fn func(string, []oaicaChatMessage) (string, error)) {
	t.Helper()
	orig := oaicaChat
	oaicaChat = fn
	t.Cleanup(func() { oaicaChat = orig })
}

// 2: a failed turn must not stay in the conversation.
func TestAFailedOaicaTurnIsNotKeptInTheHistory(t *testing.T) {
	stubOaicaChat(t, func(string, []oaicaChatMessage) (string, error) {
		return "", errors.New("router unreachable")
	})

	before := []oaicaChatMessage{{Role: "user", Content: "first"}, {Role: "assistant", Content: "answer"}}
	after, reply, err := oaicaTurn("kat", before, "a prompt that fails")
	if err == nil {
		t.Fatal("premise: the stub failed the turn")
	}
	if reply != "" {
		t.Fatalf("premise: a failed turn returned a reply %q", reply)
	}
	if len(after) != len(before) {
		t.Errorf("a turn whose request failed left %d message(s) in the conversation (had %d) — the model never answered it, and every later request re-sends it, once more per retry, so the router's context fills with prompts the user never got a reply to: %+v",
			len(after), len(before), after)
	}
}

// The control: a successful turn is still added, both halves of it.
func TestASuccessfulOaicaTurnIsAddedToTheHistory(t *testing.T) {
	stubOaicaChat(t, func(string, []oaicaChatMessage) (string, error) {
		return "the answer", nil
	})

	after, reply, err := oaicaTurn("kat", nil, "a prompt")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "the answer" {
		t.Errorf("reply = %q", reply)
	}
	if len(after) != 2 || after[0].Role != "user" || after[0].Content != "a prompt" || after[1].Role != "assistant" || after[1].Content != "the answer" {
		t.Errorf("a successful turn produced %+v, want the user turn and the reply", after)
	}
}

// 1: /clear resets every view of the session's conversation.
func TestClearingTheSessionClearsTheOaicaHistoryToo(t *testing.T) {
	opts := runOptions{
		System:   "you are terse",
		Messages: []api.Message{{Role: "user", Content: "hello"}},
	}
	history := []oaicaChatMessage{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}}

	history = resetConversation(&opts, history)

	if len(history) != 0 {
		t.Errorf("clearing the session left %d OAICA history message(s) — the user was told 'Cleared session context' while the path they are talking through still carried it to the router on the next prompt: %+v", len(history), history)
	}
	if len(opts.Messages) != 1 || opts.Messages[0].Role != "system" || opts.Messages[0].Content != "you are terse" {
		t.Errorf("the native path's messages are %+v — clearing must leave exactly the system message", opts.Messages)
	}
}
