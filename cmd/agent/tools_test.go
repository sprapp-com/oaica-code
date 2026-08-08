package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ollama/ollama/agent"
	"github.com/ollama/ollama/cmd/launch"
)

// TestAgentRegistryEnvGating: OLLAMA_AGENT_DISABLE_SHELL / _WEBSEARCH prune
// the corresponding tools.
func TestAgentRegistryEnvGating(t *testing.T) {
	reg := agentRegistry(nil, launch.AgentModelMeta{ToolCapable: true})
	for _, want := range []string{"bash", "read", "edit", "web_search", "web_fetch"} {
		if !regHas(reg, want) {
			t.Errorf("registry missing %q", want)
		}
	}

	t.Setenv("OLLAMA_AGENT_DISABLE_SHELL", "1")
	t.Setenv("OLLAMA_AGENT_DISABLE_WEBSEARCH", "1")
	reg = agentRegistry(nil, launch.AgentModelMeta{ToolCapable: true})
	if regHas(reg, "bash") {
		t.Error("bash should be disabled by OLLAMA_AGENT_DISABLE_SHELL")
	}
	if regHas(reg, "web_search") || regHas(reg, "web_fetch") {
		t.Error("web tools should be disabled by OLLAMA_AGENT_DISABLE_WEBSEARCH")
	}
	for _, keep := range []string{"read", "edit"} {
		if !regHas(reg, keep) {
			t.Errorf("registry should still have %q", keep)
		}
	}
}

// TestAgentRegistryToolIncapableModel: a model that cannot call tools yields a
// nil registry (the engine then finalizes text-only).
func TestAgentRegistryToolIncapableModel(t *testing.T) {
	if reg := agentRegistry(nil, launch.AgentModelMeta{ToolCapable: false}); reg != nil {
		t.Error("registry should be nil for a non-tool-capable model")
	}
}

func regHas(reg *agent.Registry, name string) bool {
	if reg == nil {
		return false
	}
	_, ok := reg.Get(name)
	return ok
}

// TestTerminalApprovalPrompter: y approves (and remembers the scope), a
// grants all, n denies, and the batch is denied if any call is refused.
func TestTerminalApprovalPrompter(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantAllow bool
	}{
		{"yes", "y\n", true},
		{"always", "a\n", true},
		{"no", "n\n", false},
		{"default no", "\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTerminalApprovalPrompter(strings.NewReader(tt.input), &bytes.Buffer{})
			req := agent.ApprovalRequest{}
			req.AddToolCall("t1", "bash", "bash", map[string]any{"command": "ls"})
			got, err := p.PromptApproval(context.Background(), req)
			if err != nil {
				t.Fatalf("PromptApproval: %v", err)
			}
			if got.Allow != tt.wantAllow {
				t.Errorf("Allow = %v, want %v (reason=%q)", got.Allow, tt.wantAllow, got.Reason)
			}
		})
	}
}

// TestTerminalApprovalPrompterDeniesPartialBatch: a "no" on the second of two
// calls denies the whole batch.
func TestTerminalApprovalPrompterDeniesPartialBatch(t *testing.T) {
	p := newTerminalApprovalPrompter(strings.NewReader("y\nn\n"), &bytes.Buffer{})
	req := agent.ApprovalRequest{}
	req.AddToolCall("t1", "bash", "bash", map[string]any{})
	req.AddToolCall("t2", "bash", "bash", map[string]any{})
	got, err := p.PromptApproval(context.Background(), req)
	if err != nil {
		t.Fatalf("PromptApproval: %v", err)
	}
	if got.Allow {
		t.Error("batch with a denied call must be denied")
	}
}

// TestAutoApprovePrompter: always grants everything.
func TestAutoApprovePrompter(t *testing.T) {
	got, err := (autoApprovePrompter{}).PromptApproval(context.Background(), agent.ApprovalRequest{})
	if err != nil {
		t.Fatalf("PromptApproval: %v", err)
	}
	if !got.Allow || !got.AllowAll {
		t.Errorf("got = %#v, want blanket approval", got)
	}
}
