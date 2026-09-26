package agent

// approval_piped_input_integrity_test.go — terminalApprovalPrompter built a
// fresh bufio.Reader over its input for every prompt (2026-09-26 audit). A
// bufio.Reader reads AHEAD: the first prompt pulls up to a full buffer from the
// reader it was handed, consumes the first line, and then the reader is
// discarded with the rest of the buffer inside it — so the next prompt starts
// from a drained source and gets io.EOF instead of the answer the user had
// already typed.
//
// With a terminal this is invisible (canonical mode returns one line per read,
// so nothing is ever buffered ahead). With piped input — `printf 'y\ny\n' |
// oaica agent ...`, or any scripted approval — the second tool round failed
// with an EOF error and the run stopped, which is the case the prompter exists
// to serve.
//
// The existing tests call PromptApproval once with a two-call batch, so both
// answers come from one reader and the bug never shows.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ollama/ollama/agent"
)

func approvalRequestWithOneCall() agent.ApprovalRequest {
	req := agent.ApprovalRequest{}
	req.AddToolCall("t1", "bash", "bash", map[string]any{"command": "ls"})
	return req
}

func TestAPipedAnswerSurvivesTheNextPrompt(t *testing.T) {
	var out bytes.Buffer
	p := newTerminalApprovalPrompter(strings.NewReader("y\ny\n"), &out)

	first, err := p.PromptApproval(context.Background(), approvalRequestWithOneCall())
	if err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	if !first.Allow {
		t.Fatalf("first prompt denied: %+v", first)
	}

	// One prompt per tool round: this is a *second* round, whose answer the
	// user already supplied on the same pipe.
	second, err := p.PromptApproval(context.Background(), approvalRequestWithOneCall())
	if err != nil {
		t.Fatalf("second prompt failed although the user's answer was already in the input: %v", err)
	}
	if !second.Allow {
		t.Errorf("second prompt = %+v, want allowed (its `y` was consumed by the first prompt's read-ahead)", second)
	}

	if n := strings.Count(out.String(), "[y]es / [a]lways"); n != 2 {
		t.Errorf("the user was prompted %d times, want 2", n)
	}
}

// The control: a second round whose input has genuinely run out still reports
// the error, rather than being mistaken for a denial.
func TestAnExhaustedInputStillReportsTheError(t *testing.T) {
	p := newTerminalApprovalPrompter(strings.NewReader("y\n"), &bytes.Buffer{})

	if _, err := p.PromptApproval(context.Background(), approvalRequestWithOneCall()); err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	if _, err := p.PromptApproval(context.Background(), approvalRequestWithOneCall()); err == nil {
		t.Errorf("PromptApproval returned nil for a second prompt with no input left")
	}
}
