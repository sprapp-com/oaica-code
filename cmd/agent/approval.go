package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/ollama/ollama/agent"
	"github.com/ollama/ollama/cmd/internal/termsafe"
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

	// reader is built once and kept for the run. A bufio.Reader reads ahead,
	// so one built per prompt swallows every answer already sitting in the
	// pipe and the next prompt reads an exhausted source instead
	// (2026-09-26 audit).
	reader *bufio.Reader
}

func newTerminalApprovalPrompter(in io.Reader, out io.Writer) *terminalApprovalPrompter {
	return &terminalApprovalPrompter{in: in, out: out, reader: bufio.NewReader(in)}
}

func (p *terminalApprovalPrompter) PromptApproval(ctx context.Context, req agent.ApprovalRequest) (agent.Approval, error) {
	if len(req.Calls) == 0 {
		return agent.Approval{Allow: true}, nil
	}
	reader := p.reader
	if reader == nil {
		reader = bufio.NewReader(p.in)
		p.reader = reader
	}
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

func quoteArg(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return strconv.Quote(fmt.Sprintf("%v", v))
}

func compactMap(m map[string]any) string {
	if len(m) == 0 {
		return "(no args)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(m))
	for _, k := range keys {
		// %q: the values are written by the MODEL, and a raw CR or escape sequence in one redraws the
		// question line, so the user approves a command other than the one shown (2026-09-29 audit,
		// round 127, F127-L1-5).
		// The KEY is the model's too: the registry does not reject unknown keys (round 130, F130-L1-4).
		name := k
		if termsafe.Text(k) != k {
			name = strconv.Quote(k)
		}
		parts = append(parts, fmt.Sprintf("%s=%s", name, quoteArg(m[k])))
	}
	return strings.Join(parts, " ")
}

// autoApprovePrompter approves every tool call without asking (--yes).
type autoApprovePrompter struct{}

func (autoApprovePrompter) PromptApproval(context.Context, agent.ApprovalRequest) (agent.Approval, error) {
	return agent.Approval{Allow: true, AllowAll: true}, nil
}
