package openai

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/ollama/ollama/api"
)

// ResponsesContent is a discriminated union for input content types.
// Concrete types: ResponsesTextContent, ResponsesImageContent,
// ResponsesOutputTextContent, ResponsesFileContent.
type ResponsesContent interface {
	responsesContent() // unexported marker method
}

type ResponsesTextContent struct {
	Type string `json:"type"` // always "input_text"
	Text string `json:"text"`
}

func (ResponsesTextContent) responsesContent() {}

type ResponsesImageContent struct {
	Type string `json:"type"` // always "input_image"
	// TODO(drifkin): is this really required? that seems verbose and a default is specified in the docs
	Detail   string `json:"detail"`              // required
	FileID   string `json:"file_id,omitempty"`   // optional
	ImageURL string `json:"image_url,omitempty"` // optional
}

func (ResponsesImageContent) responsesContent() {}

// ResponsesOutputTextContent represents output text from a previous assistant response
// that is being passed back as part of the conversation history.
type ResponsesOutputTextContent struct {
	Type string `json:"type"` // always "output_text"
	Text string `json:"text"`
}

func (ResponsesOutputTextContent) responsesContent() {}

type ResponsesFileContent struct {
	Type     string `json:"type"` // always "input_file"
	FileData string `json:"file_data,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	FileURL  string `json:"file_url,omitempty"`
	Filename string `json:"filename,omitempty"`
}

func (ResponsesFileContent) responsesContent() {}

type ResponsesInputMessage struct {
	Type    string             `json:"type"` // always "message"
	Role    string             `json:"role"` // one of `user`, `system`, `developer`
	Content []ResponsesContent `json:"content,omitempty"`
}

func (m *ResponsesInputMessage) UnmarshalJSON(data []byte) error {
	var aux struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	m.Type = aux.Type
	m.Role = aux.Role

	if len(aux.Content) == 0 {
		return nil
	}

	// Try to parse content as a string first (shorthand format)
	var contentStr string
	if err := json.Unmarshal(aux.Content, &contentStr); err == nil {
		m.Content = []ResponsesContent{
			ResponsesTextContent{Type: "input_text", Text: contentStr},
		}
		return nil
	}

	// Otherwise, parse as an array of content items
	var rawItems []json.RawMessage
	if err := json.Unmarshal(aux.Content, &rawItems); err != nil {
		return fmt.Errorf("content must be a string or array: %w", err)
	}

	m.Content = make([]ResponsesContent, 0, len(rawItems))
	for i, raw := range rawItems {
		content, err := unmarshalResponsesContent(raw)
		if err != nil {
			return fmt.Errorf("content[%d]: %w", i, err)
		}
		m.Content = append(m.Content, content)
	}

	return nil
}

func unmarshalResponsesContent(data []byte) (ResponsesContent, error) {
	// Peek at the type field to determine which concrete type to use
	var typeField struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &typeField); err != nil {
		return nil, err
	}

	switch typeField.Type {
	case "input_text":
		var content ResponsesTextContent
		if err := json.Unmarshal(data, &content); err != nil {
			return nil, err
		}
		return content, nil
	case "input_image":
		var content ResponsesImageContent
		if err := json.Unmarshal(data, &content); err != nil {
			return nil, err
		}
		return content, nil
	case "output_text":
		var content ResponsesOutputTextContent
		if err := json.Unmarshal(data, &content); err != nil {
			return nil, err
		}
		return content, nil
	case "input_file":
		var content ResponsesFileContent
		if err := json.Unmarshal(data, &content); err != nil {
			return nil, err
		}
		return content, nil
	default:
		return nil, fmt.Errorf("unknown content type: %s", typeField.Type)
	}
}

type ResponsesOutputMessage struct{}

// ResponsesInputItem is a discriminated union for input items.
// Concrete types: ResponsesInputMessage (more to come)
type ResponsesInputItem interface {
	responsesInputItem() // unexported marker method
}

func (ResponsesInputMessage) responsesInputItem() {}

// ResponsesFunctionCall represents an assistant's function call in conversation history.
type ResponsesFunctionCall struct {
	ID        string `json:"id,omitempty"` // item ID
	Type      string `json:"type"`         // always "function_call"
	CallID    string `json:"call_id"`      // the tool call ID
	Name      string `json:"name"`         // function name
	Arguments string `json:"arguments"`    // JSON arguments string
}

func (ResponsesFunctionCall) responsesInputItem() {}

// ResponsesFunctionCallOutput represents a function call result from the client.
type ResponsesFunctionCallOutput struct {
	Type   string `json:"type"`    // always "function_call_output"
	CallID string `json:"call_id"` // links to the original function call
	Output string `json:"output"`  // the function result

	// OutputItems is populated when output is provided as Responses content
	// items instead of the string shorthand.
	OutputItems []ResponsesContent `json:"-"`
}

func (o *ResponsesFunctionCallOutput) UnmarshalJSON(data []byte) error {
	var aux struct {
		Type   string          `json:"type"`
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	o.Type = aux.Type
	o.CallID = aux.CallID
	o.Output = ""
	o.OutputItems = nil

	if len(aux.Output) == 0 {
		return nil
	}

	var output string
	if err := json.Unmarshal(aux.Output, &output); err == nil {
		o.Output = output
		return nil
	}

	var rawItems []json.RawMessage
	if err := json.Unmarshal(aux.Output, &rawItems); err != nil {
		return fmt.Errorf("output must be a string or array: %w", err)
	}

	o.OutputItems = make([]ResponsesContent, 0, len(rawItems))
	var outputText strings.Builder
	for i, raw := range rawItems {
		content, err := unmarshalResponsesContent(raw)
		if err != nil {
			return fmt.Errorf("output[%d]: %w", i, err)
		}
		o.OutputItems = append(o.OutputItems, content)

		switch v := content.(type) {
		case ResponsesTextContent:
			outputText.WriteString(v.Text)
		case ResponsesOutputTextContent:
			outputText.WriteString(v.Text)
		}
	}
	o.Output = outputText.String()
	return nil
}

func (ResponsesFunctionCallOutput) responsesInputItem() {}

// ResponsesReasoningInput represents a reasoning item passed back as input.
// This is used when the client sends previous reasoning back for context.
type ResponsesReasoningInput struct {
	ID               string                      `json:"id,omitempty"`
	Type             string                      `json:"type"` // always "reasoning"
	Summary          []ResponsesReasoningSummary `json:"summary,omitempty"`
	EncryptedContent string                      `json:"encrypted_content,omitempty"`
}

func (ResponsesReasoningInput) responsesInputItem() {}

// unmarshalResponsesInputItem unmarshals a single input item from JSON.
func unmarshalResponsesInputItem(data []byte) (ResponsesInputItem, error) {
	var typeField struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(data, &typeField); err != nil {
		return nil, err
	}

	// Handle shorthand message format: {"role": "...", "content": "..."}
	// When type is empty but role is present, treat as a message
	itemType := typeField.Type
	if itemType == "" && typeField.Role != "" {
		itemType = "message"
	}

	switch itemType {
	case "message":
		var msg ResponsesInputMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, err
		}
		return msg, nil
	case "function_call":
		var fc ResponsesFunctionCall
		if err := json.Unmarshal(data, &fc); err != nil {
			return nil, err
		}
		return fc, nil
	case "function_call_output":
		var output ResponsesFunctionCallOutput
		if err := json.Unmarshal(data, &output); err != nil {
			return nil, err
		}
		return output, nil
	case "reasoning":
		var reasoning ResponsesReasoningInput
		if err := json.Unmarshal(data, &reasoning); err != nil {
			return nil, err
		}
		return reasoning, nil
	default:
		if itemType == "" {
			return nil, fmt.Errorf("input item missing required 'type' field")
		}
		return nil, fmt.Errorf("unknown input item type: %q", itemType)
	}
}

// ResponsesInput can be either:
// - a string (equivalent to a text input with the user role)
// - an array of input items (see ResponsesInputItem)
type ResponsesInput struct {
	Text  string               // set if input was a plain string
	Items []ResponsesInputItem // set if input was an array
}

func (r *ResponsesInput) UnmarshalJSON(data []byte) error {
	// Try string first
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		r.Text = s
		return nil
	}

	// Otherwise, try array of input items
	var rawItems []json.RawMessage
	if err := json.Unmarshal(data, &rawItems); err != nil {
		return fmt.Errorf("input must be a string or array: %w", err)
	}

	r.Items = make([]ResponsesInputItem, 0, len(rawItems))
	for i, raw := range rawItems {
		item, err := unmarshalResponsesInputItem(raw)
		if err != nil {
			return fmt.Errorf("input[%d]: %w", i, err)
		}
		r.Items = append(r.Items, item)
	}

	return nil
}

type ResponsesReasoning struct {
	// originally: optional, default is per-model
	Effort string `json:"effort,omitempty"`

	// originally: deprecated, use `summary` instead. One of `auto`, `concise`, `detailed`
	GenerateSummary string `json:"generate_summary,omitempty"`

	// originally: optional, one of `auto`, `concise`, `detailed`
	Summary string `json:"summary,omitempty"`
}

type ResponsesTextFormat struct {
	Type   string          `json:"type"`             // "text", "json_schema"
	Name   string          `json:"name,omitempty"`   // for json_schema
	Schema json.RawMessage `json:"schema,omitempty"` // for json_schema
	Strict *bool           `json:"strict,omitempty"` // for json_schema
}

type ResponsesText struct {
	Format *ResponsesTextFormat `json:"format,omitempty"`
}

// ResponsesTool represents a tool in the Responses API format.
// Note: This differs from api.Tool which nests fields under "function".
type ResponsesTool struct {
	Type        string         `json:"type"` // "function"
	Name        string         `json:"name"`
	Description *string        `json:"description"` // nullable but required
	Strict      *bool          `json:"strict"`      // nullable but required
	Parameters  map[string]any `json:"parameters"`  // nullable but required
}

type ResponsesRequest struct {
	Model string `json:"model"`

	// originally: optional, default is false
	// for us: not supported
	Background bool `json:"background"`

	// originally: optional `string | {id: string}`
	// for us: not supported
	Conversation json.RawMessage `json:"conversation"`

	// originally: string[]
	// for us: ignored
	Include []string `json:"include"`

	Input ResponsesInput `json:"input"`

	// optional, inserts a system message at the start of the conversation
	Instructions string `json:"instructions,omitempty"`

	// optional, maps to num_predict
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`

	Reasoning ResponsesReasoning `json:"reasoning"`

	// optional, default is 1.0
	Temperature *float64 `json:"temperature"`

	// optional, controls output format (e.g. json_schema)
	Text *ResponsesText `json:"text,omitempty"`

	// optional, default is 1.0
	TopP *float64 `json:"top_p"`

	// optional, default is `"disabled"`
	Truncation *string `json:"truncation"`

	Tools []ResponsesTool `json:"tools,omitempty"`

	// TODO(drifkin): tool_choice is not supported. We could support "none" by not
	// passing tools, but the other controls like `"required"` cannot be generally
	// supported.

	// optional, default is false
	Stream *bool `json:"stream,omitempty"`
}

// FromResponsesRequest converts a ResponsesRequest to api.ChatRequest
func FromResponsesRequest(r ResponsesRequest) (*api.ChatRequest, error) {
	var messages []api.Message

	// Add instructions as system message if present
	if r.Instructions != "" {
		messages = append(messages, api.Message{
			Role:    "system",
			Content: r.Instructions,
		})
	}

	// Handle simple string input
	if r.Input.Text != "" {
		messages = append(messages, api.Message{
			Role:    "user",
			Content: r.Input.Text,
		})
	}

	// Handle array of input items
	// Track pending reasoning to merge with the next assistant message
	var pendingThinking string

	for _, item := range r.Input.Items {
		switch v := item.(type) {
		case ResponsesReasoningInput:
			// Store thinking to merge with the next assistant message
			pendingThinking = v.EncryptedContent
		case ResponsesInputMessage:
			msg, err := convertInputMessage(v)
			if err != nil {
				return nil, err
			}
			// If this is an assistant message, attach pending thinking
			if msg.Role == "assistant" && pendingThinking != "" {
				msg.Thinking = pendingThinking
				pendingThinking = ""
			}
			messages = append(messages, msg)
		case ResponsesFunctionCall:
			// Convert function call to assistant message with tool calls
			var args api.ToolCallFunctionArguments
			if v.Arguments != "" {
				if err := json.Unmarshal([]byte(v.Arguments), &args); err != nil {
					return nil, fmt.Errorf("failed to parse function call arguments: %w", err)
				}
			}
			toolCall := api.ToolCall{
				ID: v.CallID,
				Function: api.ToolCallFunction{
					Name:      v.Name,
					Arguments: args,
				},
			}

			// Merge tool call into existing assistant message if it has content or tool calls
			if len(messages) > 0 && messages[len(messages)-1].Role == "assistant" {
				lastMsg := &messages[len(messages)-1]
				lastMsg.ToolCalls = append(lastMsg.ToolCalls, toolCall)
				if pendingThinking != "" {
					lastMsg.Thinking = pendingThinking
					pendingThinking = ""
				}
			} else {
				msg := api.Message{
					Role:      "assistant",
					ToolCalls: []api.ToolCall{toolCall},
				}
				if pendingThinking != "" {
					msg.Thinking = pendingThinking
					pendingThinking = ""
				}
				messages = append(messages, msg)
			}
		case ResponsesFunctionCallOutput:
			content := v.Output
			var images []api.ImageData
			if len(v.OutputItems) > 0 {
				var err error
				content, images, err = convertResponsesContent(v.OutputItems)
				if err != nil {
					return nil, err
				}
			}
			messages = append(messages, api.Message{
				Role:       "tool",
				Content:    content,
				Images:     images,
				ToolCallID: v.CallID,
			})
		}
	}

	// If there's trailing reasoning without a following message, emit it
	if pendingThinking != "" {
		messages = append(messages, api.Message{
			Role:     "assistant",
			Thinking: pendingThinking,
		})
	}

	options := make(map[string]any)

	if r.Temperature != nil {
		options["temperature"] = *r.Temperature
	} else {
		options["temperature"] = 1.0
	}

	if r.TopP != nil {
		options["top_p"] = *r.TopP
	} else { //nolint:staticcheck // SA9003: empty branch
		// TODO(drifkin): OpenAI defaults to 1.0 here, but we don't follow that here
		// in case the model has a different default. It would be best if we
		// understood whether there was a model-specific default and if not, we
		// should also default to 1.0, but that will require some additional
		// plumbing
	}

	if r.MaxOutputTokens != nil {
		options["num_predict"] = *r.MaxOutputTokens
	}

	var think *api.ThinkValue
	if effort := r.Reasoning.Effort; effort != "" {
		switch effort {
		case "none":
			think = &api.ThinkValue{Value: false}
		case "low", "medium", "high", "max":
			think = &api.ThinkValue{Value: effort}
		default:
			return nil, fmt.Errorf("invalid reasoning value: %q (must be \"high\", \"medium\", \"low\", \"max\", or \"none\")", effort)
		}
	}

	// Convert tools from Responses API format to api.Tool format
	var tools []api.Tool
	for _, t := range r.Tools {
		tool, err := convertTool(t)
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}

	// Handle text format (e.g. json_schema)
	var format json.RawMessage
	if r.Text != nil && r.Text.Format != nil {
		switch r.Text.Format.Type {
		case "json_schema":
			if r.Text.Format.Schema != nil {
				format = r.Text.Format.Schema
			}
		}
	}

	return &api.ChatRequest{
		Model:    r.Model,
		Messages: messages,
		Options:  options,
		Tools:    tools,
		Format:   format,
		Think:    think,
	}, nil
}

func convertTool(t ResponsesTool) (api.Tool, error) {
	// Convert parameters from map[string]any to api.ToolFunctionParameters
	var params api.ToolFunctionParameters
	if t.Parameters != nil {
		// Marshal and unmarshal to convert
		b, err := json.Marshal(t.Parameters)
		if err != nil {
			return api.Tool{}, fmt.Errorf("failed to marshal tool parameters: %w", err)
		}
		if err := json.Unmarshal(b, &params); err != nil {
			return api.Tool{}, fmt.Errorf("failed to unmarshal tool parameters: %w", err)
		}
	}

	var description string
	if t.Description != nil {
		description = *t.Description
	}

	return api.Tool{
		Type: t.Type,
		Function: api.ToolFunction{
			Name:        t.Name,
			Description: description,
			Parameters:  params,
		},
	}, nil
}

func convertInputMessage(m ResponsesInputMessage) (api.Message, error) {
	content, images, err := convertResponsesContent(m.Content)
	if err != nil {
		return api.Message{}, err
	}

	return api.Message{
		Role:    m.Role,
		Content: content,
		Images:  images,
	}, nil
}

func convertResponsesContent(contents []ResponsesContent) (string, []api.ImageData, error) {
	var content string
	var images []api.ImageData

	for _, c := range contents {
		switch v := c.(type) {
		case ResponsesTextContent:
			content += v.Text
		case ResponsesOutputTextContent:
			content += v.Text
		case ResponsesImageContent:
			if v.ImageURL == "" {
				continue // Skip if no URL (FileID not supported)
			}
			img, err := decodeImageURL(v.ImageURL)
			if err != nil {
				return "", nil, err
			}
			images = append(images, img)
		case ResponsesFileContent:
			// TODO(drifkin): support inlining text-only file_data when it is safe
			// to decode and of a reasonable size
			return "", nil, fmt.Errorf("file inputs are not currently supported")
		}
	}

	return content, images, nil
}

// Response types for the Responses API

// ResponsesTextField represents the text output configuration in the response.
type ResponsesTextField struct {
	Format ResponsesTextFormat `json:"format"`
}

// ResponsesReasoningOutput represents reasoning configuration in the response.
type ResponsesReasoningOutput struct {
	Effort  *string `json:"effort,omitempty"`
	Summary *string `json:"summary,omitempty"`
}

// ResponsesError represents an error in the response.
type ResponsesError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ResponsesIncompleteDetails represents details about why a response was incomplete.
type ResponsesIncompleteDetails struct {
	Reason string `json:"reason"`
}

type ResponsesResponse struct {
	ID                 string                      `json:"id"`
	Object             string                      `json:"object"`
	CreatedAt          int64                       `json:"created_at"`
	CompletedAt        *int64                      `json:"completed_at"`
	Status             string                      `json:"status"`
	IncompleteDetails  *ResponsesIncompleteDetails `json:"incomplete_details"`
	Model              string                      `json:"model"`
	PreviousResponseID *string                     `json:"previous_response_id"`
	Instructions       *string                     `json:"instructions"`
	Output             []ResponsesOutputItem       `json:"output"`
	Error              *ResponsesError             `json:"error"`
	Tools              []ResponsesTool             `json:"tools"`
	ToolChoice         any                         `json:"tool_choice"`
	Truncation         string                      `json:"truncation"`
	ParallelToolCalls  bool                        `json:"parallel_tool_calls"`
	Text               ResponsesTextField          `json:"text"`
	TopP               float64                     `json:"top_p"`
	PresencePenalty    float64                     `json:"presence_penalty"`
	FrequencyPenalty   float64                     `json:"frequency_penalty"`
	TopLogprobs        int                         `json:"top_logprobs"`
	Temperature        float64                     `json:"temperature"`
	Reasoning          *ResponsesReasoningOutput   `json:"reasoning"`
	Usage              *ResponsesUsage             `json:"usage"`
	MaxOutputTokens    *int                        `json:"max_output_tokens"`
	MaxToolCalls       *int                        `json:"max_tool_calls"`
	Store              bool                        `json:"store"`
	Background         bool                        `json:"background"`
	ServiceTier        string                      `json:"service_tier"`
	Metadata           map[string]any              `json:"metadata"`
	SafetyIdentifier   *string                     `json:"safety_identifier"`
	PromptCacheKey     *string                     `json:"prompt_cache_key"`
}

type ResponsesOutputItem struct {
	ID        string                   `json:"id"`
	Type      string                   `json:"type"` // "message", "function_call", or "reasoning"
	Status    string                   `json:"status,omitempty"`
	Role      string                   `json:"role,omitempty"`      // for message
	Content   []ResponsesOutputContent `json:"content,omitempty"`   // for message
	CallID    string                   `json:"call_id,omitempty"`   // for function_call
	Name      string                   `json:"name,omitempty"`      // for function_call
	Arguments string                   `json:"arguments,omitempty"` // for function_call

	// Reasoning fields
	Summary          []ResponsesReasoningSummary `json:"summary,omitempty"`           // for reasoning
	EncryptedContent string                      `json:"encrypted_content,omitempty"` // for reasoning
}

type ResponsesReasoningSummary struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

type ResponsesOutputContent struct {
	Type        string `json:"type"` // "output_text"
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
	Logprobs    []any  `json:"logprobs"`
}

type ResponsesInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type ResponsesOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type ResponsesUsage struct {
	InputTokens         int                          `json:"input_tokens"`
	OutputTokens        int                          `json:"output_tokens"`
	TotalTokens         int                          `json:"total_tokens"`
	InputTokensDetails  ResponsesInputTokensDetails  `json:"input_tokens_details"`
	OutputTokensDetails ResponsesOutputTokensDetails `json:"output_tokens_details"`
}

func intValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// derefFloat64 returns the value of a float64 pointer, or a default if nil.
func derefFloat64(p *float64, def float64) float64 {
	if p != nil {
		return *p
	}
	return def
}

// ToResponse converts an api.ChatResponse to a Responses API response.
// The request is used to echo back request parameters in the response.
func ToResponse(model, responseID, itemID string, chatResponse api.ChatResponse, request ResponsesRequest) ResponsesResponse {
	// An empty array, not a nil one: `output` is an array on this wire, and the
	// streamed arm's own `response.created`/`response.in_progress` events state
	// it as `[]`, so a terminal `null` would contradict this response's own
	// first two events (2026-09-29 audit, round 94, F94-L1-2).
	output := []ResponsesOutputItem{}

	// reasoningItem is the item that holds this turn's reasoning.
	reasoningItem := func() ResponsesOutputItem {
		return ResponsesOutputItem{
			ID:   fmt.Sprintf("rs_%s", responseID),
			Type: "reasoning",
			Summary: []ResponsesReasoningSummary{
				{
					Type: "summary_text",
					Text: chatResponse.Message.Thinking,
				},
			},
			EncryptedContent: chatResponse.Message.Thinking, // Plain text for now
		}
	}
	// callItems is the turn's calls, in the order it made them.
	callItems := func() []ResponsesOutputItem {
		toolCalls := ToToolCalls(chatResponse.Message.ToolCalls)
		items := make([]ResponsesOutputItem, 0, len(toolCalls))
		for i, tc := range toolCalls {
			items = append(items, ResponsesOutputItem{
				ID:        fmt.Sprintf("fc_%s_%d", responseID, i),
				Type:      "function_call",
				Status:    "completed",
				CallID:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		return items
	}

	// When the turn's own run list is present and accounts for it, the items are
	// written in the order the RUNS state, because that is the order the
	// streaming arm of this leg announces and documents them in — item by item,
	// as the chunks arrive. Both arms must answer one upstream body with one
	// item order: this arm used to write a fixed reasoning/message/calls order
	// whatever the runs said, so a turn that narrated BEFORE it reasoned —
	// `[text A, thinking T]`, the shape a reasoning model that answers and then
	// reconsiders produces — reached a streaming client as
	// `[message, reasoning]` and a non-streaming one as `[reasoning, message]`,
	// and a turn that reasoned, called and then spoke (`[thinking, call, text]`)
	// reached them as `[reasoning, function_call, message]` and
	// `[reasoning, message, function_call]` (2026-09-29 audit, round 98,
	// F98-L1-3).
	//
	// Each item stands where its FIRST run of that kind stands, and the calls
	// stand one per call run: the text of a turn is one message item on this
	// wire, so a turn whose prose sits either side of its reasoning states one
	// message item at its first text run — which is what the streamed arm's own
	// terminal document states for the same turn (measured: `[text, thinking,
	// text]` → `[message, reasoning]` on both arms, the two text runs joined in
	// the one message item exactly as its streamed sibling joins them). The
	// order is all this arm takes from the runs; the text itself is the merged
	// Content, which is what the streamed arm's document carries too.
	runsInOrder := chatResponse.Message.OutputRunsAccountFor()
	if runsInOrder {
		textWritten, reasoningWritten := false, false
		nextCall := 0
		calls := callItems()
		for _, run := range chatResponse.Message.OutputRuns {
			switch run.Kind {
			case "thinking":
				if reasoningWritten || chatResponse.Message.Thinking == "" {
					continue
				}
				reasoningWritten = true
				output = append(output, reasoningItem())
			case "call":
				if nextCall < len(calls) {
					output = append(output, calls[nextCall])
					nextCall++
				}
			case "text":
				if textWritten || chatResponse.Message.Content == "" {
					continue
				}
				textWritten = true
				output = append(output, ResponsesOutputItem{
					ID:     itemID,
					Type:   "message",
					Status: "completed",
					Role:   "assistant",
					Content: []ResponsesOutputContent{
						{
							Type:        "output_text",
							Text:        chatResponse.Message.Content,
							Annotations: []any{},
							Logprobs:    []any{},
						},
					},
				})
			}
		}
		return buildResponsesResponse(model, responseID, itemID, chatResponse, request, output)
	}

	// Add reasoning item if thinking is present
	if chatResponse.Message.Thinking != "" {
		output = append(output, reasoningItem())
	}

	// The text of a turn that also called a tool is part of the answer, not an
	// alternative to it (2026-09-27 audit, round 18). It is written BEFORE the
	// calls because that is the order the streaming arm's own output array is
	// built in — its `buildFinalOutput` states reasoning, then the message, then
	// the calls, whatever order the events arrived in — so writing the calls
	// first here answered a model that narrated before it acted with
	// `[function_call, message]` and the same model streamed with
	// `[message, function_call]`: one upstream body, two orders, the call in
	// front of the prose that introduced it. Measured on every other arm of this
	// leg (native, OpenAI chat, Anthropic) and on this arm's own streamed
	// sibling, which all say text-then-call (2026-09-29 audit, round 92,
	// F92-L1-3).
	//
	// A turn that stated NO text gets no message item, on this arm or the
	// streamed one: the item exists to hold the text, and one holding an empty
	// string is an item no other arm states — the streamed arm's own output is
	// built from the text deltas the client was sent, and the Anthropic surface,
	// whose `content` is the same kind of array this `output` is, answers the
	// same turn `"content": []`. Written unconditionally, this arm answered
	// `[{"type":"message","content":[{"text":""}]}]` where its streamed sibling
	// answered nothing at all (2026-09-29 audit, round 94, F94-L1-2).
	if chatResponse.Message.Content != "" {
		output = append(output, ResponsesOutputItem{
			ID:     itemID,
			Type:   "message",
			Status: "completed",
			Role:   "assistant",
			Content: []ResponsesOutputContent{
				{
					Type:        "output_text",
					Text:        chatResponse.Message.Content,
					Annotations: []any{},
					Logprobs:    []any{},
				},
			},
		})
	}

	// Then the calls, in the order the turn made them.
	output = append(output, callItems()...)

	return buildResponsesResponse(model, responseID, itemID, chatResponse, request, output)
}

// doneReasonLength is the runner's own word for a turn the model stopped at its
// generation cap, and capReason is this wire's word for the same thing. Both
// are stated below and in processCompletion.
const (
	doneReasonLength = "length"
	capReason        = "max_output_tokens"
)

// cappedByGenerationLimit reports whether this turn's stop is the cap, under the
// same priority the sibling translated surfaces use: a turn that delivered calls
// is stated by the CALLS, whatever the runner's done_reason said (openai's
// finishReason, anthropic's mapStopReason). A capped turn with no call is the
// only one that carries a truncation verdict on this leg. `hasCalls` is the
// caller's own count — the whole response on the buffered arm, the calls the
// converter has already written on the streamed one, since the terminal chunk of
// a streamed turn carries none of them.
func cappedByGenerationLimit(doneReason string, hasCalls bool) bool {
	return doneReason == doneReasonLength && !hasCalls
}

// buildResponsesResponse wraps the items an arm assembled into the response
// document this wire states, echoing back the request parameters it carries.
func buildResponsesResponse(model, responseID, itemID string, chatResponse api.ChatResponse, request ResponsesRequest, output []ResponsesOutputItem) ResponsesResponse {
	var instructions *string
	if request.Instructions != "" {
		instructions = &request.Instructions
	}

	// Build truncation with default
	truncation := "disabled"
	if request.Truncation != nil {
		truncation = *request.Truncation
	}

	tools := request.Tools
	if tools == nil {
		tools = []ResponsesTool{}
	}

	text := ResponsesTextField{
		Format: ResponsesTextFormat{Type: "text"},
	}
	if request.Text != nil && request.Text.Format != nil {
		text.Format = *request.Text.Format
	}

	// Build reasoning output from request
	var reasoning *ResponsesReasoningOutput
	if request.Reasoning.Effort != "" || request.Reasoning.Summary != "" {
		reasoning = &ResponsesReasoningOutput{}
		if request.Reasoning.Effort != "" {
			reasoning.Effort = &request.Reasoning.Effort
		}
		if request.Reasoning.Summary != "" {
			reasoning.Summary = &request.Reasoning.Summary
		}
	}

	// The verdict word. Every other surface of this leg states a turn the model
	// stopped at its cap — the native wire `"done_reason":"length"`, chat
	// `"finish_reason":"length"`, Anthropic `"stop_reason":"max_tokens"` — and
	// this one said `"status":"completed"` with `"incomplete_details":null`, on
	// both of its arms, so a client reading the truncation the way this wire
	// defines it (`status == "incomplete"`, `incomplete_details.reason ==
	// "max_output_tokens"`, both typed by openai-python) was told a capped answer
	// was the whole one (2026-09-29 audit, round 101, F101-L1-1). `max_output_tokens`
	// is forwarded as `num_predict` and the runner's cap path sets
	// `done_reason:"length"`, so this is reachable with an ordinary body.
	status := "completed"
	var incomplete *ResponsesIncompleteDetails
	if cappedByGenerationLimit(chatResponse.DoneReason, len(chatResponse.Message.ToolCalls) > 0) {
		status = "incomplete"
		incomplete = &ResponsesIncompleteDetails{Reason: capReason}
	}

	return ResponsesResponse{
		ID:                 responseID,
		Object:             "response",
		CreatedAt:          chatResponse.CreatedAt.Unix(),
		CompletedAt:        nil, // Set by middleware when writing final response
		Status:             status,
		IncompleteDetails:  incomplete,
		Model:              model,
		PreviousResponseID: nil, // Not supported
		Instructions:       instructions,
		Output:             output,
		Error:              nil, // Only populated on failure
		Tools:              tools,
		ToolChoice:         "auto", // Default value
		Truncation:         truncation,
		ParallelToolCalls:  true, // Default value
		Text:               text,
		TopP:               derefFloat64(request.TopP, 1.0),
		PresencePenalty:    0, // Default value
		FrequencyPenalty:   0, // Default value
		TopLogprobs:        0, // Default value
		Temperature:        derefFloat64(request.Temperature, 1.0),
		Reasoning:          reasoning,
		Usage: &ResponsesUsage{
			InputTokens:        chatResponse.PromptEvalCount,
			OutputTokens:       chatResponse.EvalCount,
			TotalTokens:        chatResponse.PromptEvalCount + chatResponse.EvalCount,
			InputTokensDetails: ResponsesInputTokensDetails{CachedTokens: intValue(chatResponse.PromptEvalCachedCount)},
			// TODO(drifkin): wire through the actual values
			OutputTokensDetails: ResponsesOutputTokensDetails{ReasoningTokens: 0},
		},
		MaxOutputTokens:  request.MaxOutputTokens,
		MaxToolCalls:     nil,   // Not supported
		Store:            false, // We don't store responses
		Background:       request.Background,
		ServiceTier:      "default", // Default value
		Metadata:         map[string]any{},
		SafetyIdentifier: nil, // Not supported
		PromptCacheKey:   nil, // Not supported
	}
}

// Streaming events: <https://platform.openai.com/docs/api-reference/responses-streaming>

// ResponsesStreamEvent represents a single Server-Sent Event for the Responses API.
type ResponsesStreamEvent struct {
	Event string // The event type (e.g., "response.created")
	Data  any    // The event payload (will be JSON-marshaled)
}

// ResponsesStreamConverter converts api.ChatResponse objects to Responses API
// streaming events. It maintains state across multiple calls to handle the
// streaming event sequence correctly.
type ResponsesStreamConverter struct {
	// Configuration (immutable after creation)
	responseID string
	itemID     string
	model      string
	request    ResponsesRequest

	// State tracking (mutated across Process calls)
	firstWrite     bool
	outputIndex    int
	contentIndex   int
	contentStarted bool
	// messageItemIndex is the output_index the assistant message item claimed
	// when it started. Every later event about that item (deltas, the part and
	// item "done" events) reports the index the item was announced under, which
	// is no longer outputIndex once the counter has moved past it.
	messageItemIndex int
	accumulatedText  string
	sequenceNumber   int

	// Reasoning/thinking state
	accumulatedThinking string
	reasoningItemID     string
	reasoningStarted    bool
	reasoningDone       bool
	// reasoningItemIndex is the output_index the reasoning item was announced
	// under.
	reasoningItemIndex int

	// Tool calls state (for final output)
	toolCallItems []map[string]any
	// toolCallItemIndexes[i] is the output_index toolCallItems[i] was announced
	// under; the two slices are written together.
	toolCallItemIndexes []int
}

// newEvent creates a ResponsesStreamEvent with the sequence number included in the data.
func (c *ResponsesStreamConverter) newEvent(eventType string, data map[string]any) ResponsesStreamEvent {
	data["type"] = eventType
	data["sequence_number"] = c.sequenceNumber
	c.sequenceNumber++
	return ResponsesStreamEvent{
		Event: eventType,
		Data:  data,
	}
}

// NewResponsesStreamConverter creates a new converter with the given configuration.
func NewResponsesStreamConverter(responseID, itemID, model string, request ResponsesRequest) *ResponsesStreamConverter {
	return &ResponsesStreamConverter{
		responseID: responseID,
		itemID:     itemID,
		model:      model,
		request:    request,
		firstWrite: true,
	}
}

// Process takes a ChatResponse and returns the events that should be emitted.
// Events are returned in order. The caller is responsible for serializing
// and sending these events.
func (c *ResponsesStreamConverter) Process(r api.ChatResponse) []ResponsesStreamEvent {
	var events []ResponsesStreamEvent

	hasToolCalls := len(r.Message.ToolCalls) > 0
	hasThinking := r.Message.Thinking != ""

	// First chunk - emit initial events
	if c.firstWrite {
		c.firstWrite = false
		events = append(events, c.createResponseCreatedEvent())
		events = append(events, c.createResponseInProgressEvent())
	}

	// Handle reasoning/thinking (before other content)
	// A chunk that states its own order is written in it: the message item is
	// announced where the chunk's first text run stands, so a model that states a
	// call and THEN the bytes of an entry it never named announces the call first
	// — the order the buffered arm of this surface gives the same turn (round
	// 100, F100-L1-1). The list accounts for the turn exactly or it would not be
	// read at all (OutputRunsAccountFor), so every item below is written: one
	// reasoning item, one message item, and one call item per call run.
	if r.Message.OutputRunsAccountFor() {
		events = append(events, c.processInRunOrder(r)...)
		if r.Done {
			events = append(events, c.processCompletion(r)...)
		}
		return events
	}

	if hasThinking {
		events = append(events, c.processThinking(r.Message.Thinking)...)
	}

	// Handle text content. It is emitted whatever the tool calls are doing: a
	// model that narrates before it acts ("Let me look at the file." then the
	// call) produces both, they are both items of the response, and refusing
	// the content whenever the chunk carried a call dropped the model's answer
	// with no error (2026-09-27 audit, round 18).
	if r.Message.Content != "" {
		events = append(events, c.processTextContent(r.Message.Content)...)
	}

	// Handle tool calls
	if hasToolCalls {
		events = append(events, c.processToolCalls(r.Message.ToolCalls)...)
	}

	// Done - emit closing events
	if r.Done {
		events = append(events, c.processCompletion(r)...)
	}

	return events
}

// buildResponseObject creates a full response object with all required fields for streaming events.
func (c *ResponsesStreamConverter) buildResponseObject(status string, output []any, usage map[string]any) map[string]any {
	var instructions any = nil
	if c.request.Instructions != "" {
		instructions = c.request.Instructions
	}

	truncation := "disabled"
	if c.request.Truncation != nil {
		truncation = *c.request.Truncation
	}

	var tools []any
	if c.request.Tools != nil {
		for _, t := range c.request.Tools {
			tools = append(tools, map[string]any{
				"type":        t.Type,
				"name":        t.Name,
				"description": t.Description,
				"strict":      t.Strict,
				"parameters":  t.Parameters,
			})
		}
	}
	if tools == nil {
		tools = []any{}
	}

	textFormat := map[string]any{"type": "text"}
	if c.request.Text != nil && c.request.Text.Format != nil {
		textFormat = map[string]any{
			"type": c.request.Text.Format.Type,
		}
		if c.request.Text.Format.Name != "" {
			textFormat["name"] = c.request.Text.Format.Name
		}
		if c.request.Text.Format.Schema != nil {
			textFormat["schema"] = c.request.Text.Format.Schema
		}
		if c.request.Text.Format.Strict != nil {
			textFormat["strict"] = *c.request.Text.Format.Strict
		}
	}

	var reasoning any = nil
	if c.request.Reasoning.Effort != "" || c.request.Reasoning.Summary != "" {
		r := map[string]any{}
		if c.request.Reasoning.Effort != "" {
			r["effort"] = c.request.Reasoning.Effort
		} else {
			r["effort"] = nil
		}
		if c.request.Reasoning.Summary != "" {
			r["summary"] = c.request.Reasoning.Summary
		} else {
			r["summary"] = nil
		}
		reasoning = r
	}

	// Build top_p and temperature with defaults
	topP := 1.0
	if c.request.TopP != nil {
		topP = *c.request.TopP
	}
	temperature := 1.0
	if c.request.Temperature != nil {
		temperature = *c.request.Temperature
	}

	return map[string]any{
		"id":                   c.responseID,
		"object":               "response",
		"created_at":           time.Now().Unix(),
		"completed_at":         nil,
		"status":               status,
		"incomplete_details":   nil,
		"model":                c.model,
		"previous_response_id": nil,
		"instructions":         instructions,
		"output":               output,
		"error":                nil,
		"tools":                tools,
		"tool_choice":          "auto",
		"truncation":           truncation,
		"parallel_tool_calls":  true,
		"text":                 map[string]any{"format": textFormat},
		"top_p":                topP,
		"presence_penalty":     0,
		"frequency_penalty":    0,
		"top_logprobs":         0,
		"temperature":          temperature,
		"reasoning":            reasoning,
		"usage":                usage,
		"max_output_tokens":    c.request.MaxOutputTokens,
		"max_tool_calls":       nil,
		"store":                false,
		"background":           c.request.Background,
		"service_tier":         "default",
		"metadata":             map[string]any{},
		"safety_identifier":    nil,
		"prompt_cache_key":     nil,
	}
}

func (c *ResponsesStreamConverter) createResponseCreatedEvent() ResponsesStreamEvent {
	return c.newEvent("response.created", map[string]any{
		"response": c.buildResponseObject("in_progress", []any{}, nil),
	})
}

func (c *ResponsesStreamConverter) createResponseInProgressEvent() ResponsesStreamEvent {
	return c.newEvent("response.in_progress", map[string]any{
		"response": c.buildResponseObject("in_progress", []any{}, nil),
	})
}

func (c *ResponsesStreamConverter) processThinking(thinking string) []ResponsesStreamEvent {
	var events []ResponsesStreamEvent

	// Start reasoning item if not started
	if !c.reasoningStarted {
		c.reasoningStarted = true
		c.reasoningItemID = fmt.Sprintf("rs_%d", rand.Intn(999999))
		c.reasoningItemIndex = c.outputIndex

		events = append(events, c.newEvent("response.output_item.added", map[string]any{
			"output_index": c.reasoningItemIndex,
			"item": map[string]any{
				"id":      c.reasoningItemID,
				"type":    "reasoning",
				"summary": []any{},
			},
		}))
		// The item claims its index HERE, the way the message item does, so
		// that every later event about it can name the index it was announced
		// at. It used to be claimed when the item was CLOSED, which meant a
		// close that happened after another item had been announced named that
		// item's index instead (2026-09-29 audit, round 96, F96-L1-3).
		c.outputIndex++
	} else if c.reasoningDone {
		// Thinking that resumes after the item was closed — think, answer,
		// think again — is the SAME item: the terminal document states one
		// reasoning item whose summary is the whole of the thinking, and the
		// buffered arm states the same turn. Reopened so the summary it closes
		// with is the whole text rather than the prefix that had arrived when
		// the answer interrupted it (round 96, F96-L1-3).
		c.reasoningDone = false
	}

	// Accumulate thinking
	c.accumulatedThinking += thinking

	// Emit delta
	events = append(events, c.newEvent("response.reasoning_summary_text.delta", map[string]any{
		"item_id":       c.reasoningItemID,
		"output_index":  c.reasoningItemIndex,
		"summary_index": 0,
		"delta":         thinking,
	}))

	// TODO(drifkin): consider adding
	// [`response.reasoning_text.delta`](https://platform.openai.com/docs/api-reference/responses-streaming/response/reasoning_text/delta),
	// but need to do additional research to understand how it's used and how
	// widely supported it is

	return events
}

func (c *ResponsesStreamConverter) finishReasoning() []ResponsesStreamEvent {
	if !c.reasoningStarted || c.reasoningDone {
		return nil
	}
	c.reasoningDone = true

	events := []ResponsesStreamEvent{
		c.newEvent("response.reasoning_summary_text.done", map[string]any{
			"item_id":       c.reasoningItemID,
			"output_index":  c.reasoningItemIndex,
			"summary_index": 0,
			"text":          c.accumulatedThinking,
		}),
		c.newEvent("response.output_item.done", map[string]any{
			"output_index": c.reasoningItemIndex,
			"item": map[string]any{
				"id":                c.reasoningItemID,
				"type":              "reasoning",
				"summary":           []map[string]any{{"type": "summary_text", "text": c.accumulatedThinking}},
				"encrypted_content": c.accumulatedThinking, // Plain text for now
			},
		}),
	}

	return events
}

// processInRunOrder states the chunk's items in the order the chunk gives them:
// each kind written once, at the run that introduces it — the reasoning item at
// the first thinking run, the message item at the first text run, and one call
// item per call run. The list accounts for the turn exactly before this is
// reached, so the walk spends it: the text runs join to Content, the thinking
// runs to Thinking, and there is one call run per entry of ToolCalls. A chunk
// with no list at all keeps the fixed reasoning-prose-calls order Process writes
// (2026-09-29 audit, round 100, F100-L1-1).
func (c *ResponsesStreamConverter) processInRunOrder(r api.ChatResponse) []ResponsesStreamEvent {
	var events []ResponsesStreamEvent
	wroteThinking, wroteText, written := false, false, 0
	for _, run := range r.Message.OutputRuns {
		switch run.Kind {
		case "thinking":
			if wroteThinking || r.Message.Thinking == "" {
				continue
			}
			wroteThinking = true
			events = append(events, c.processThinking(r.Message.Thinking)...)
		case "text":
			if wroteText || r.Message.Content == "" {
				continue
			}
			wroteText = true
			events = append(events, c.processTextContent(r.Message.Content)...)
		case "call":
			if written >= len(r.Message.ToolCalls) {
				continue
			}
			events = append(events, c.processToolCalls(r.Message.ToolCalls[written:written+1])...)
			written++
		}
	}
	return events
}

func (c *ResponsesStreamConverter) processToolCalls(toolCalls []api.ToolCall) []ResponsesStreamEvent {
	var events []ResponsesStreamEvent

	// Finish reasoning first if it was started
	events = append(events, c.finishReasoning()...)

	converted := ToToolCalls(toolCalls)

	for i, tc := range converted {
		fcItemID := fmt.Sprintf("fc_%d_%d", rand.Intn(999999), i)
		// Each call is an item of the RESPONSE, and outputIndex is the index of
		// the next one — the same counter the reasoning and message items take
		// from. Nothing else advances it for a function_call, so a stream that
		// delivers them one chunk at a time must advance it here, by as many
		// items as it just emitted; a second counter added to this one instead
		// (round 16's toolCallCount) left the text and reasoning paths — which
		// index by outputIndex alone — claiming indexes the calls already owned
		// (2026-09-27 audit, round 17).
		idx := c.outputIndex + i

		// Store for final output (with status: completed)
		toolCallItem := map[string]any{
			"id":        fcItemID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   tc.ID,
			"name":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
		}
		c.toolCallItems = append(c.toolCallItems, toolCallItem)
		c.toolCallItemIndexes = append(c.toolCallItemIndexes, idx)

		// response.output_item.added for function call
		events = append(events, c.newEvent("response.output_item.added", map[string]any{
			"output_index": idx,
			"item": map[string]any{
				"id":        fcItemID,
				"type":      "function_call",
				"status":    "in_progress",
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": "",
			},
		}))

		// response.function_call_arguments.delta
		if tc.Function.Arguments != "" {
			events = append(events, c.newEvent("response.function_call_arguments.delta", map[string]any{
				"item_id":      fcItemID,
				"output_index": idx,
				"delta":        tc.Function.Arguments,
			}))
		}

		// response.function_call_arguments.done
		events = append(events, c.newEvent("response.function_call_arguments.done", map[string]any{
			"item_id":      fcItemID,
			"output_index": idx,
			"arguments":    tc.Function.Arguments,
		}))

		// response.output_item.done for function call
		events = append(events, c.newEvent("response.output_item.done", map[string]any{
			"output_index": idx,
			"item": map[string]any{
				"id":        fcItemID,
				"type":      "function_call",
				"status":    "completed",
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			},
		}))
	}
	c.outputIndex += len(converted)

	return events
}

func (c *ResponsesStreamConverter) processTextContent(content string) []ResponsesStreamEvent {
	var events []ResponsesStreamEvent

	// Finish reasoning first if it was started
	events = append(events, c.finishReasoning()...)

	// Emit output item and content part for first text content
	if !c.contentStarted {
		c.contentStarted = true
		// The message is an item of the response and claims the next index; the
		// counter then moves past it so whatever follows does not reuse it
		// (2026-09-27 audit, round 17).
		c.messageItemIndex = c.outputIndex
		c.outputIndex++

		// response.output_item.added
		events = append(events, c.newEvent("response.output_item.added", map[string]any{
			"output_index": c.messageItemIndex,
			"item": map[string]any{
				"id":      c.itemID,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		}))

		// response.content_part.added
		events = append(events, c.newEvent("response.content_part.added", map[string]any{
			"item_id":       c.itemID,
			"output_index":  c.messageItemIndex,
			"content_index": c.contentIndex,
			"part": map[string]any{
				"type":        "output_text",
				"text":        "",
				"annotations": []any{},
				"logprobs":    []any{},
			},
		}))
	}

	// Accumulate text
	c.accumulatedText += content

	// Emit content delta
	events = append(events, c.newEvent("response.output_text.delta", map[string]any{
		"item_id":       c.itemID,
		"output_index":  c.messageItemIndex,
		"content_index": 0,
		"delta":         content,
		"logprobs":      []any{},
	}))

	return events
}

func (c *ResponsesStreamConverter) buildFinalOutput() []any {
	// An empty array, not a nil one — see ToResponse: the terminal object must
	// not state `null` where this same response's own `response.created` events
	// stated `[]` (2026-09-29 audit, round 94, F94-L1-2).
	output := []any{}

	// The items are written in the order their own output_index claimed, because
	// that is the order this same stream announced them to the client. A turn
	// that CALLED and then narrated claimed 0 for the call and 1 for the message,
	// and its text arrives after the call's item was opened — so a document that
	// listed the message first told a client keying its bookkeeping on
	// output_index (Codex, OMP) the opposite of what the events it had just read
	// said (2026-09-29 audit, round 95, F95-L1-2). The order within one response
	// is unchanged: text that arrives before a call still claims the lower index.
	type finalItem struct {
		index int
		value any
	}
	items := make([]finalItem, 0, 1+len(c.toolCallItems))

	// Reasoning item if present
	if c.reasoningStarted {
		items = append(items, finalItem{c.reasoningItemIndex, map[string]any{
			"id":                c.reasoningItemID,
			"type":              "reasoning",
			"summary":           []map[string]any{{"type": "summary_text", "text": c.accumulatedThinking}},
			"encrypted_content": c.accumulatedThinking,
		}})
	}

	// The message item if text was relayed. A turn that stated NO text gets no
	// message item (2026-09-29 audit, round 94, F94-L1-2).
	if c.contentStarted {
		items = append(items, finalItem{c.messageItemIndex, map[string]any{
			"id":     c.itemID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]any{{
				"type":        "output_text",
				"text":        c.accumulatedText,
				"annotations": []any{},
				"logprobs":    []any{},
			}},
		}})
	}

	// The calls, each under the index it was announced with.
	for i, item := range c.toolCallItems {
		items = append(items, finalItem{c.toolCallItemIndexes[i], item})
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].index < items[j].index })
	for _, it := range items {
		output = append(output, it.value)
	}

	return output
}

func (c *ResponsesStreamConverter) processCompletion(r api.ChatResponse) []ResponsesStreamEvent {
	var events []ResponsesStreamEvent

	// Finish reasoning if not done
	events = append(events, c.finishReasoning()...)

	// Emit text completion events if we had text content
	if c.contentStarted {
		// response.output_text.done
		events = append(events, c.newEvent("response.output_text.done", map[string]any{
			"item_id":       c.itemID,
			"output_index":  c.messageItemIndex,
			"content_index": 0,
			"text":          c.accumulatedText,
			"logprobs":      []any{},
		}))

		// response.content_part.done
		events = append(events, c.newEvent("response.content_part.done", map[string]any{
			"item_id":       c.itemID,
			"output_index":  c.messageItemIndex,
			"content_index": 0,
			"part": map[string]any{
				"type":        "output_text",
				"text":        c.accumulatedText,
				"annotations": []any{},
				"logprobs":    []any{},
			},
		}))

		// response.output_item.done
		events = append(events, c.newEvent("response.output_item.done", map[string]any{
			"output_index": c.messageItemIndex,
			"item": map[string]any{
				"id":     c.itemID,
				"type":   "message",
				"status": "completed",
				"role":   "assistant",
				"content": []map[string]any{{
					"type":        "output_text",
					"text":        c.accumulatedText,
					"annotations": []any{},
					"logprobs":    []any{},
				}},
			},
		}))
	}

	// response.completed
	usage := map[string]any{
		"input_tokens":  r.PromptEvalCount,
		"output_tokens": r.EvalCount,
		"total_tokens":  r.PromptEvalCount + r.EvalCount,
		"input_tokens_details": map[string]any{
			"cached_tokens": intValue(r.PromptEvalCachedCount),
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": 0,
		},
	}
	// The terminal event states this wire's verdict for the turn, the same one
	// the buffered arm states in its document: `response.incomplete` with status
	// "incomplete" and the reason the model stopped, where a capped turn used to
	// be announced as `response.completed` — a client of the streaming arm had no
	// truncation signal anywhere on this surface (2026-09-29 audit, round 101,
	// F101-L1-1). Same priority as the buffered builder: the calls win.
	status, event := "completed", "response.completed"
	if cappedByGenerationLimit(r.DoneReason, len(c.toolCallItems) > 0) {
		status, event = "incomplete", "response.incomplete"
	}
	response := c.buildResponseObject(status, c.buildFinalOutput(), usage)
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": capReason}
	}
	response["completed_at"] = time.Now().Unix()
	events = append(events, c.newEvent(event, map[string]any{
		"response": response,
	}))

	return events
}

// Failure is the terminal event of a turn that did NOT finish, in the shape this
// wire defines for it: `response.failed`, carrying the response object with
// status "failed" and the producer's own sentence in `error`, alongside whatever
// output the converter had already assembled. It is the counterpart of the
// `response.completed` above, and it exists because a stream that stops without
// either leaves the client holding a partial answer as if it were the whole one:
// no terminal event, no cause, and nothing to retry (2026-09-29 audit, round 92,
// F92-L1-1). The events already written cannot be un-sent, so the failure is
// stated on the wire rather than the turn being left unfinished in silence.
func (c *ResponsesStreamConverter) Failure(sentence string, status int) []ResponsesStreamEvent {
	response := c.buildResponseObject("failed", c.buildFinalOutput(), nil)
	response["completed_at"] = time.Now().Unix()
	response["error"] = map[string]any{
		"code":    ErrorCode(status),
		"message": sentence,
	}
	return []ResponsesStreamEvent{c.newEvent("response.failed", map[string]any{
		"response": response,
	})}
}
