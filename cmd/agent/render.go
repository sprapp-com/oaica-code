package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ollama/ollama/agent"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/readline"
)

// maxToolOutput caps the tool output echoed to the terminal so a runaway
// command cannot flood the transcript.
const maxToolOutput = 4000

// stdoutSink renders engine events as plain streaming text on stdout — the
// non-TUI counterpart of the bubbletea chat renderer (cmd/tui/chat). Writers
// are fields so tests can capture output without touching os.Stdout.
type stdoutSink struct {
	out    io.Writer // streaming assistant text + tool status
	errOut io.Writer // errors (unused by Emit — see EventError case)
}

func newStdoutSink() *stdoutSink {
	return &stdoutSink{out: os.Stdout, errOut: os.Stderr}
}

func (s *stdoutSink) Emit(ev agent.Event) error {
	switch ev.Type {
	case agent.EventMessageDelta:
		fmt.Fprint(s.out, ev.Content)

	case agent.EventThinkingDelta:
		fmt.Fprintf(s.out, "%s┄ %s%s\n", readline.ColorGrey, strings.TrimRight(ev.Thinking, "\n"), readline.ColorDefault)

	case agent.EventToolCallDetected:
		for _, tc := range ev.ToolCalls {
			fmt.Fprintf(s.out, "\n  ◆ %s%s%s %s\n", readline.ColorBold, tc.Function.Name, readline.ColorDefault, compactToolArgs(tc))
		}

	case agent.EventToolStarted:
		// The call was already announced on EventToolCallDetected.

	case agent.EventToolFinished:
		if ev.Error != "" {
			fmt.Fprintf(s.out, "  ✗ %s %s\n", ev.ToolName, ev.Error)
			return nil
		}
		fmt.Fprintf(s.out, "  ✓ %s\n", ev.ToolName)
		if content := strings.TrimSpace(ev.Content); content != "" {
			fmt.Fprintln(s.out, indentBlock(truncateToolOutput(content), "    "))
		}

	case agent.EventRunFinished:
		fmt.Fprintln(s.out)

	case agent.EventError:
		// Do not print here: Session.Run returns the error and cobra prints
		// the single "Error: …" line. Returning the error keeps the exit
		// code non-zero.
		return fmt.Errorf("%s", ev.Error)
	}
	return nil
}

// compactToolArgs renders a tool call's arguments as a single JSON line,
// capped for the status line.
func compactToolArgs(tc api.ToolCall) string {
	b, err := json.Marshal(tc.Function.Arguments.ToMap())
	if err != nil {
		return ""
	}
	s := string(b)
	const max = 80
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func truncateToolOutput(s string) string {
	if len(s) > maxToolOutput {
		return s[:maxToolOutput] + "\n… (truncated)"
	}
	return s
}

func indentBlock(s, indent string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}
