package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

// defaultAgentMaxTokens is the fallback max_tokens when the launch inventory
// has no MaxOutputTokens for the resolved model (spec floor).
const defaultAgentMaxTokens = 4096

// thinkingBudget is the Anthropic thinking budget for enabled thinking. The
// engine passes no budget level through api.ChatRequest, so one fixed budget
// is used for every enabled level.
const thinkingBudget = 20000

// buildMessagesRequest converts the engine's api.ChatRequest into an
// Anthropic MessagesRequest. It is the reverse of anthropic.convertMessage:
// tool_use blocks become api.ToolCalls and consecutive role:"tool" messages
// are grouped into a single user message of tool_result blocks (Anthropic
// requires tool results to live in a user-role turn following the assistant's
// tool_use blocks).
//
// Assistant thinking is dropped on the way out: api.Message carries no
// thinking signature, and Anthropic rejects assistant thinking blocks without
// one. The model does not need the echo; the final text content is sent.
func buildMessagesRequest(req *api.ChatRequest, meta launch.AgentModelMeta) (*anthropic.MessagesRequest, error) {
	out := &anthropic.MessagesRequest{
		Model:     req.Model,
		MaxTokens: meta.MaxOutputTokens,
		Stream:    true,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = defaultAgentMaxTokens
	}

	var system []string
	var messages []anthropic.MessageParam
	var pendingToolResults []anthropic.ContentBlock
	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		messages = append(messages, anthropic.MessageParam{Role: "user", Content: pendingToolResults})
		pendingToolResults = nil
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			system = append(system, msg.Content)
		case "tool":
			pendingToolResults = append(pendingToolResults, anthropic.ContentBlock{
				Type:      "tool_result",
				ToolUseID: msg.ToolCallID,
				Content:   msg.Content,
			})
		default:
			flushToolResults()
			param := anthropic.MessageParam{Role: msg.Role}
			if msg.Content != "" {
				param.Content = append(param.Content, anthropic.ContentBlock{Type: "text", Text: &msg.Content})
			}
			for _, img := range msg.Images {
				data := []byte(img)
				param.Content = append(param.Content, anthropic.ContentBlock{
					Type: "image",
					Source: &anthropic.ImageSource{
						Type:      "base64",
						MediaType: imageMediaType(data),
						Data:      base64.StdEncoding.EncodeToString(data),
					},
				})
			}
			for _, tc := range msg.ToolCalls {
				param.Content = append(param.Content, anthropic.ContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: tc.Function.Arguments,
				})
			}
			if len(param.Content) > 0 {
				messages = append(messages, param)
			}
		}
	}
	flushToolResults()

	if len(system) > 0 {
		out.System = strings.Join(system, "\n\n")
	}
	out.Messages = messages

	if len(req.Tools) > 0 {
		out.Tools = make([]anthropic.Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			params, err := json.Marshal(t.Function.Parameters)
			if err != nil {
				return nil, fmt.Errorf("marshal tool %q parameters: %w", t.Function.Name, err)
			}
			out.Tools = append(out.Tools, anthropic.Tool{
				Type:        "custom",
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: params,
			})
		}
	}

	if thinkEnabled(req.Think) {
		out.Thinking = &anthropic.ThinkingConfig{Type: "enabled", BudgetTokens: thinkingBudget}
	}

	return out, nil
}

// thinkEnabled reports whether a ThinkValue requests thinking (bool true or a
// known level string).
func thinkEnabled(t *api.ThinkValue) bool {
	if t == nil || t.Value == nil {
		return false
	}
	switch v := t.Value.(type) {
	case bool:
		return v
	case string:
		return v == "high" || v == "medium" || v == "low" || v == "max"
	default:
		return false
	}
}

// imageMediaType sniffs image bytes to fill Anthropic's required media_type.
func imageMediaType(b []byte) string {
	switch {
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	case len(b) >= 4 && bytes.Equal(b[:4], []byte{0x89, 'P', 'N', 'G'}):
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 3 && bytes.Equal(b[:3], []byte("GIF")):
		return "image/gif"
	default:
		return "image/png"
	}
}

// shimTimeout matches the translation proxy's upstream timeout so a hung
// upstream fails the run instead of stalling it forever.
const shimTimeout = 5 * time.Minute

// maxSSEFrameSize bounds a single SSE data line (input_json_delta fragments
// are the only long lines; tool schemas are small).
const maxSSEFrameSize = 16 * 1024 * 1024

// shimClient implements agent.ChatClient by speaking the Anthropic Messages
// API to a resolved endpoint: for user remotes the loopback translation proxy,
// for cloud/local the OAICA router or local server (behind the logging proxy).
// The endpoint always presents Anthropic-native /v1/messages, so this shim is
// the one protocol the agent engine sees regardless of upstream.
type shimClient struct {
	baseURL    string // loopback proxy or router; shim appends "/v1/messages"
	token      string // bearer token
	model      string // bare upstream model id
	meta       launch.AgentModelMeta
	httpClient *http.Client
}

func newShimClient(baseURL, token, model string, meta launch.AgentModelMeta) *shimClient {
	return &shimClient{
		baseURL:    baseURL,
		token:      token,
		model:      model,
		meta:       meta,
		httpClient: &http.Client{Timeout: shimTimeout},
	}
}

func (s *shimClient) Chat(ctx context.Context, req *api.ChatRequest, fn api.ChatResponseFunc) error {
	mreq, err := buildMessagesRequest(req, s.meta)
	if err != nil {
		return err
	}
	body, err := json.Marshal(mreq)
	if err != nil {
		return fmt.Errorf("marshal messages request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return parseAPIError(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSEFrameSize)
	acc := newAnthropicSSEAccumulator()

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "event:") {
			continue // the event type is also in the JSON payload
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &envelope); err != nil {
			return fmt.Errorf("parse SSE frame: %w", err)
		}
		deltas, done, err := acc.Feed(envelope.Type, []byte(data))
		if err != nil {
			return err
		}
		for _, d := range deltas {
			if err := fn(d); err != nil {
				return err
			}
		}
		if done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read SSE stream: %w", err)
	}
	return nil
}

// parseAPIError extracts an Anthropic-shaped error body from a non-2xx
// response, falling back to the raw body.
func parseAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e anthropic.Error
	if err := json.Unmarshal(body, &e); err == nil && e.Message != "" {
		return fmt.Errorf("upstream %s: %s", resp.Status, e.Message)
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("upstream error: %s", msg)
}
