package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ollama/ollama/agent"
)

// terminalApprovalPrompter asks for tool approval on the terminal, one
// question per pending call. It implements agent.ApprovalPrompter.
//
//   - "y" approves the call and remembers its scope for the rest of the run
//   - "a" approves all future calls
//   - anything else (default, "n") denies
//
// A denied call denies the whole batch: the engine sends one Approval result
// for all pending calls.
type terminalApprovalPrompter struct {
	in  io.Reader
	out io.Writer
}

func newTerminalApprovalPrompter(in io.Reader, out io.Writer) *terminalApprovalPrompter {
	return &terminalApprovalPrompter{in: in, out: out}
}

func (p *terminalApprovalPrompter) PromptApproval(ctx context.Context, req agent.ApprovalRequest) (agent.Approval, error) {
	if len(req.Calls) == 0 {
		return agent.Approval{Allow: true}, nil
	}
	reader := bufio.NewReader(p.in)
	allowed := make([]string, 0, len(req.Calls))
	for _, call := range req.Calls {
		fmt.Fprintf(p.out, "\n  Run %s with %s?\n", call.ToolName, compactMap(call.Args))
		fmt.Fprint(p.out, "  [y]es / [a]lways / [n]o (default no): ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil {
				return agent.Approval{Reason: "Tool approval canceled."}, nil
			}
			return agent.Approval{}, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			allowed = append(allowed, call.ApprovalScope)
		case "a", "always":
			return agent.Approval{Allow: true, AllowAll: true}, nil
		default:
			return agent.Approval{Reason: "Denied by user."}, nil
		}
	}
	return agent.Approval{Allow: true, AllowScopes: allowed}, nil
}

func compactMap(m map[string]any) string {
	if len(m) == 0 {
		return "(no args)"
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, " ")
}

// autoApprovePrompter approves every tool call without asking (--yes).
type autoApprovePrompter struct{}

func (autoApprovePrompter) PromptApproval(context.Context, agent.ApprovalRequest) (agent.Approval, error) {
	return agent.Approval{Allow: true, AllowAll: true}, nil
}
