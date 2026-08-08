package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ollama/ollama/agent"
	"github.com/ollama/ollama/api"
)

func newTestSink() (*stdoutSink, *bytes.Buffer, *bytes.Buffer) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	return &stdoutSink{out: out, errOut: errOut}, out, errOut
}

func TestSinkMessageDelta(t *testing.T) {
	s, out, _ := newTestSink()
	_ = s.Emit(agent.Event{Type: agent.EventMessageDelta, Content: "Hello"})
	_ = s.Emit(agent.Event{Type: agent.EventMessageDelta, Content: ", world"})
	if out.String() != "Hello, world" {
		t.Errorf("stdout = %q, want continuous stream", out.String())
	}
}

func TestSinkToolCallAndFinished(t *testing.T) {
	s, out, _ := newTestSink()
	_ = s.Emit(agent.Event{
		Type:      agent.EventToolCallDetected,
		ToolCalls: []api.ToolCall{{Function: api.ToolCallFunction{Name: "read_file"}}},
	})
	_ = s.Emit(agent.Event{Type: agent.EventToolFinished, ToolName: "read_file", Content: "line1\nline2"})
	got := out.String()
	for _, want := range []string{"read_file", "line1", "line2"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout missing %q:\n%s", want, got)
		}
	}
}

func TestSinkToolFinishedTruncatesLongOutput(t *testing.T) {
	s, out, _ := newTestSink()
	long := strings.Repeat("x", 5000)
	_ = s.Emit(agent.Event{Type: agent.EventToolFinished, ToolName: "bash", Content: long})
	if !strings.Contains(out.String(), "… (truncated)") {
		t.Error("long tool output should be truncated with an ellipsis marker")
	}
}

func TestSinkErrorReturnsError(t *testing.T) {
	s, _, errOut := newTestSink()
	err := s.Emit(agent.Event{Type: agent.EventError, Error: "boom"})
	if err == nil || err.Error() != "boom" {
		t.Errorf("err = %v, want the event error", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("EventError must not print itself (cobra prints it): %q", errOut.String())
	}
}
