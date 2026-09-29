package round127_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ollama/ollama/agent"
	agenttools "github.com/ollama/ollama/agent/tools"
	"github.com/ollama/ollama/api"
)

type scripted struct {
	turns [][]api.ToolCall
	n     int
	seen  []string
}

func (s *scripted) Chat(ctx context.Context, req *api.ChatRequest, fn api.ChatResponseFunc) error {
	if len(req.Messages) > 0 {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "tool" {
			s.seen = append(s.seen, last.Content)
		}
	}
	var resp api.ChatResponse
	resp.Message.Role = "assistant"
	if s.n < len(s.turns) {
		resp.Message.ToolCalls = s.turns[s.n]
	} else {
		resp.Message.Content = "done"
	}
	s.n++
	resp.Done = true
	resp.DoneReason = "stop"
	return fn(resp)
}

func call(id, name string, kv ...string) api.ToolCall {
	a := api.NewToolCallFunctionArguments()
	for i := 0; i < len(kv); i += 2 {
		a.Set(kv[i], kv[i+1])
	}
	return api.ToolCall{ID: id, Function: api.ToolCallFunction{Name: name, Arguments: a}}
}

// Emulates the terminal prompter answering "y" — exactly what cmd/agent/approval.go returns for "y".
type yesOnce struct{ prompts []string }

func (p *yesOnce) PromptApproval(_ context.Context, req agent.ApprovalRequest) (agent.Approval, error) {
	scopes := []string{}
	for _, c := range req.Calls {
		p.prompts = append(p.prompts, c.ToolName+" "+c.ApprovalScope)
		scopes = append(scopes, c.ApprovalScope)
	}
	return agent.Approval{Allow: true, AllowScopes: scopes}, nil
}

func TestRound127YesOnOneCallDoesNotApproveEveryCallOfThatTool(t *testing.T) {
	cwd := t.TempDir()
	os.WriteFile(filepath.Join(cwd, "README.md"), []byte("readme"), 0o644)
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "id_rsa")
	os.WriteFile(secret, []byte("PRIVATE KEY MATERIAL"), 0o600)

	client := &scripted{turns: [][]api.ToolCall{
		{call("c1", "read", "path", "README.md")},
		{call("c2", "read", "path", secret)},
		{call("c3", "edit", "path", "README.md", "old_text", "readme", "new_text", "x")},
		{call("c4", "edit", "path", "README.md", "old_text", "x", "new_text", "PWNED")},
	}}
	reg := &agent.Registry{}
	reg.Register(&agenttools.Read{})
	reg.Register(&agenttools.Edit{})
	p := &yesOnce{}
	sess := &agent.Session{Client: client, Tools: reg, ApprovalPrompter: p, WorkingDir: cwd}
	_, err := sess.Run(context.Background(), agent.RunOptions{Model: "m", Messages: []api.Message{{Role: "user", Content: "hi"}}, MaxToolRounds: -1})
	t.Logf("err=%v", err)
	t.Logf("prompts shown to the user (%d): %q", len(p.prompts), p.prompts)
	t.Logf("tool results the model received: %q", client.seen)
	b, _ := os.ReadFile(filepath.Join(cwd, "README.md"))
	t.Logf("README now: %q", b)
	// "y" binds the exact call: the read of README.md, the read of a different (absolute) path, and the
	// first edit each prompt; the repeat of the same edit is the scope already approved.
	if len(p.prompts) != 3 {
		t.Errorf("%d prompts for read README, read <absolute path>, edit README x2 (want 3): approving one call approved a different one: %q", len(p.prompts), p.prompts)
	}
}

// Every tool that takes a target scopes an approval to it.
func TestRound127EveryTargetedToolScopesItsApprovalToTheTarget(t *testing.T) {
	for _, c := range []struct {
		tool agent.ScopedTool
		key  string
	}{
		{&agenttools.Read{}, "path"}, {&agenttools.Edit{}, "path"}, {&agenttools.WebFetch{}, "url"}, {&agenttools.WebSearch{}, "query"},
	} {
		a := c.tool.ApprovalScope(map[string]any{c.key: "one"})
		b := c.tool.ApprovalScope(map[string]any{c.key: "two"})
		if a == b {
			t.Errorf("%T: two different targets share the approval scope %q", c.tool, a)
		}
	}
}
