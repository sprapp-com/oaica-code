// openai package provides core transformation logic for partial compatibility with the OpenAI REST API
package openai

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/types/model"
)

var finishReasonToolCalls = "tool_calls"
var finishReasonStop = "stop"

// finishReason maps this server's internal done reason onto OpenAI's
// finish_reason enum. OpenAI's values are "stop", "length", "tool_calls",
// "function_call" and "content_filter"; anything else is a value a strict
// client cannot parse.
//
// The reason used to be returned VERBATIM, so the server's own "load" and
// "unload" answers (server/routes.go replies with those for an empty prompt)
// went out as `"finish_reason":"load"` on all three wires. An empty reason
// still means "not finished yet" and stays null — that is what a streaming
// client keys on (2026-09-26 audit, fourth round).
func finishReason(doneReason string, hasToolCalls bool) *string {
	if doneReason == "" {
		return nil
	}
	if hasToolCalls {
		return &finishReasonToolCalls
	}
	switch doneReason {
	case "stop", "length", "content_filter", "tool_calls", "function_call":
		r := doneReason
		return &r
	}
	// An internal reason with no OpenAI spelling ("load", "unload", a future
	// one): the turn ended, so say that rather than leak the literal.
	r := finishReasonStop
	return &r
}

type Error struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   any     `json:"param"`
	Code    *string `json:"code"`
}

type ErrorResponse struct {
	Error Error `json:"error"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	Reasoning  string     `json:"reasoning,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ChoiceLogprobs struct {
	Content []api.Logprob `json:"content"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      Message         `json:"message"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     *ChoiceLogprobs `json:"logprobs,omitempty"`
}

type ChunkChoice struct {
	Index        int             `json:"index"`
	Delta        Message         `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     *ChoiceLogprobs `json:"logprobs,omitempty"`
}

type CompleteChunkChoice struct {
	Text         string          `json:"text"`
	Index        int             `json:"index"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     *ChoiceLogprobs `json:"logprobs,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ResponseFormat struct {
	Type       string      `json:"type"`
	JsonSchema *JsonSchema `json:"json_schema,omitempty"`
}

type JsonSchema struct {
	Schema json.RawMessage `json:"schema"`
}

type EmbedRequest struct {
	Input          any    `json:"input"`
	Model          string `json:"model"`
	Dimensions     int    `json:"dimensions,omitempty"`
	EncodingFormat string `json:"encoding_format,omitempty"` // "float" or "base64"
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type Reasoning struct {
	Effort string `json:"effort,omitempty"`
}

type ChatCompletionRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options"`
	MaxTokens     *int           `json:"max_tokens"`
	// MaxCompletionTokens is the modern spelling of the output cap. A client
	// that sends only this one used to have its cap silently ignored — no
	// num_predict reached the runner at all, and the model generated to the
	// server default (2026-09-26 audit, fourth round).
	MaxCompletionTokens *int `json:"max_completion_tokens"`
	// ToolChoice is accepted because a client that sends "none" must not be
	// handed tool calls. It was absent from the struct entirely, so the field
	// was dropped on decode and the request behaved as if tools were still in
	// play.
	ToolChoice       any             `json:"tool_choice"`
	Seed             *int            `json:"seed"`
	Stop             any             `json:"stop"`
	Temperature      *float64        `json:"temperature"`
	FrequencyPenalty *float64        `json:"frequency_penalty"`
	PresencePenalty  *float64        `json:"presence_penalty"`
	TopP             *float64        `json:"top_p"`
	ResponseFormat   *ResponseFormat `json:"response_format"`
	Tools            []api.Tool      `json:"tools"`
	Reasoning        *Reasoning      `json:"reasoning,omitempty"`
	ReasoningEffort  *string         `json:"reasoning_effort,omitempty"`
	Logprobs         *bool           `json:"logprobs"`
	TopLogprobs      int             `json:"top_logprobs"`
	DebugRenderOnly  bool            `json:"_debug_render_only"`
}

type ChatCompletion struct {
	Id                string         `json:"id"`
	Object            string         `json:"object"`
	Created           int64          `json:"created"`
	Model             string         `json:"model"`
	SystemFingerprint string         `json:"system_fingerprint"`
	Choices           []Choice       `json:"choices"`
	Usage             Usage          `json:"usage,omitempty"`
	DebugInfo         *api.DebugInfo `json:"_debug_info,omitempty"`
}

type ChatCompletionChunk struct {
	Id                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	SystemFingerprint string        `json:"system_fingerprint"`
	Choices           []ChunkChoice `json:"choices"`
	Usage             *Usage        `json:"usage,omitempty"`
}

// TODO (https://github.com/ollama/ollama/issues/5259): support []string, []int and [][]int
type CompletionRequest struct {
	Model            string         `json:"model"`
	Prompt           string         `json:"prompt"`
	FrequencyPenalty float32        `json:"frequency_penalty"`
	MaxTokens        *int           `json:"max_tokens"`
	PresencePenalty  float32        `json:"presence_penalty"`
	Seed             *int           `json:"seed"`
	Stop             any            `json:"stop"`
	Stream           bool           `json:"stream"`
	StreamOptions    *StreamOptions `json:"stream_options"`
	Temperature      *float32       `json:"temperature"`
	TopP             float32        `json:"top_p"`
	Suffix           string         `json:"suffix"`
	Logprobs         *int           `json:"logprobs"`
	DebugRenderOnly  bool           `json:"_debug_render_only"`
}

type Completion struct {
	Id                string                `json:"id"`
	Object            string                `json:"object"`
	Created           int64                 `json:"created"`
	Model             string                `json:"model"`
	SystemFingerprint string                `json:"system_fingerprint"`
	Choices           []CompleteChunkChoice `json:"choices"`
	Usage             Usage                 `json:"usage,omitempty"`
}

type CompletionChunk struct {
	Id                string                `json:"id"`
	Object            string                `json:"object"`
	Created           int64                 `json:"created"`
	Choices           []CompleteChunkChoice `json:"choices"`
	Model             string                `json:"model"`
	SystemFingerprint string                `json:"system_fingerprint"`
	Usage             *Usage                `json:"usage,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Index    int    `json:"index"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Model struct {
	Id      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type Embedding struct {
	Object    string `json:"object"`
	Embedding any    `json:"embedding"` // Can be []float32 (float format) or string (base64 format)
	Index     int    `json:"index"`
}

type ListCompletion struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

type EmbeddingList struct {
	Object string         `json:"object"`
	Data   []Embedding    `json:"data"`
	Model  string         `json:"model"`
	Usage  EmbeddingUsage `json:"usage,omitempty"`
}

type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func NewError(code int, message string) ErrorResponse {
	var etype string
	switch code {
	case http.StatusBadRequest:
		etype = "invalid_request_error"
	case http.StatusNotFound:
		etype = "not_found_error"
	default:
		etype = "api_error"
	}

	return ErrorResponse{Error{Type: etype, Message: message}}
}

// ToUsage converts an api.ChatResponse to Usage
func ToUsage(r api.ChatResponse) Usage {
	return Usage{
		PromptTokens:     r.Metrics.PromptEvalCount,
		CompletionTokens: r.Metrics.EvalCount,
		TotalTokens:      r.Metrics.PromptEvalCount + r.Metrics.EvalCount,
	}
}

// ToToolCalls converts api.ToolCall to OpenAI ToolCall format
func ToToolCalls(tc []api.ToolCall) []ToolCall {
	toolCalls := make([]ToolCall, len(tc))
	for i, tc := range tc {
		toolCalls[i].ID = tc.ID
		toolCalls[i].Type = "function"
		toolCalls[i].Function.Name = tc.Function.Name
		toolCalls[i].Index = tc.Function.Index

		args, err := json.Marshal(tc.Function.Arguments)
		if err != nil {
			slog.Error("could not marshall function arguments to json", "error", err)
			continue
		}

		toolCalls[i].Function.Arguments = string(args)
	}
	return toolCalls
}

// ToChatCompletion converts an api.ChatResponse to ChatCompletion
func ToChatCompletion(id string, r api.ChatResponse) ChatCompletion {
	toolCalls := ToToolCalls(r.Message.ToolCalls)

	var logprobs *ChoiceLogprobs
	if len(r.Logprobs) > 0 {
		logprobs = &ChoiceLogprobs{Content: r.Logprobs}
	}

	return ChatCompletion{
		Id:                id,
		Object:            "chat.completion",
		Created:           r.CreatedAt.Unix(),
		Model:             r.Model,
		SystemFingerprint: "fp_ollama",
		Choices: []Choice{{
			Index:   0,
			Message: Message{Role: r.Message.Role, Content: r.Message.Content, ToolCalls: toolCalls, Reasoning: r.Message.Thinking},
			// The same helper the streaming chunks use, so one api.ChatResponse
			// cannot answer "load" here and "stop" there. This call site kept the
			// verbatim closure finishReason replaced (2026-09-27 audit, round 19).
			FinishReason: finishReason(r.DoneReason, len(toolCalls) > 0),
			Logprobs:     logprobs,
		}}, Usage: ToUsage(r),
		DebugInfo: r.DebugInfo,
	}
}

func toChunk(id string, r api.ChatResponse, toolCallSent bool) ChatCompletionChunk {
	toolCalls := ToToolCalls(r.Message.ToolCalls)

	var logprobs *ChoiceLogprobs
	if len(r.Logprobs) > 0 {
		logprobs = &ChoiceLogprobs{Content: r.Logprobs}
	}

	return ChatCompletionChunk{
		Id:                id,
		Object:            "chat.completion.chunk",
		Created:           time.Now().Unix(),
		Model:             r.Model,
		SystemFingerprint: "fp_ollama",
		Choices: []ChunkChoice{{
			Index:        0,
			Delta:        Message{Role: "assistant", Content: r.Message.Content, ToolCalls: toolCalls, Reasoning: r.Message.Thinking},
			FinishReason: finishReason(r.DoneReason, toolCallSent || len(toolCalls) > 0),
			Logprobs:     logprobs,
		}},
	}
}

// ToChunks converts an api.ChatResponse to one or more ChatCompletionChunk values.
func ToChunks(id string, r api.ChatResponse, toolCallSent bool) []ChatCompletionChunk {
	hasMixedResponse := r.Message.Thinking != "" && (r.Message.Content != "" || len(r.Message.ToolCalls) > 0)
	if !hasMixedResponse {
		return []ChatCompletionChunk{toChunk(id, r, toolCallSent)}
	}

	reasoningChunk := toChunk(id, r, toolCallSent)
	// The logprobs here might include tokens not in this chunk because we now split between thinking and content/tool calls.
	reasoningChunk.Choices[0].Delta.Content = ""
	reasoningChunk.Choices[0].Delta.ToolCalls = nil
	reasoningChunk.Choices[0].FinishReason = nil

	contentOrToolCallsChunk := toChunk(id, r, toolCallSent)
	// Keep both split chunks on the same timestamp since they represent one logical emission.
	contentOrToolCallsChunk.Created = reasoningChunk.Created
	contentOrToolCallsChunk.Choices[0].Delta.Reasoning = ""
	contentOrToolCallsChunk.Choices[0].Logprobs = nil

	return []ChatCompletionChunk{
		reasoningChunk,
		contentOrToolCallsChunk,
	}
}

// Deprecated: use ToChunks for streaming conversion.
func ToChunk(id string, r api.ChatResponse, toolCallSent bool) ChatCompletionChunk {
	return toChunk(id, r, toolCallSent)
}

// ToUsageGenerate converts an api.GenerateResponse to Usage
func ToUsageGenerate(r api.GenerateResponse) Usage {
	return Usage{
		PromptTokens:     r.Metrics.PromptEvalCount,
		CompletionTokens: r.Metrics.EvalCount,
		TotalTokens:      r.Metrics.PromptEvalCount + r.Metrics.EvalCount,
	}
}

// ToCompletion converts an api.GenerateResponse to Completion
func ToCompletion(id string, r api.GenerateResponse) Completion {
	return Completion{
		Id:                id,
		Object:            "text_completion",
		Created:           r.CreatedAt.Unix(),
		Model:             r.Model,
		SystemFingerprint: "fp_ollama",
		Choices: []CompleteChunkChoice{{
			Text:         r.Response,
			Index:        0,
			FinishReason: finishReason(r.DoneReason, false),
		}},
		Usage: ToUsageGenerate(r),
	}
}

// ToCompleteChunk converts an api.GenerateResponse to CompletionChunk
func ToCompleteChunk(id string, r api.GenerateResponse) CompletionChunk {
	return CompletionChunk{
		Id:                id,
		Object:            "text_completion",
		Created:           time.Now().Unix(),
		Model:             r.Model,
		SystemFingerprint: "fp_ollama",
		Choices: []CompleteChunkChoice{{
			Text:         r.Response,
			Index:        0,
			FinishReason: finishReason(r.DoneReason, false),
		}},
	}
}

// ToListCompletion converts an api.ListResponse to ListCompletion
func ToListCompletion(r api.ListResponse) ListCompletion {
	var data []Model
	for _, m := range r.Models {
		id := m.Model
		if id == "" {
			id = m.Name
		}

		data = append(data, Model{
			Id:      id,
			Object:  "model",
			Created: m.ModifiedAt.Unix(),
			OwnedBy: model.ParseName(id).Namespace,
		})
	}

	return ListCompletion{
		Object: "list",
		Data:   data,
	}
}

// ToEmbeddingList converts an api.EmbedResponse to EmbeddingList
// encodingFormat can be "float", "base64", or empty (defaults to "float")
func ToEmbeddingList(model string, r api.EmbedResponse, encodingFormat string) EmbeddingList {
	if r.Embeddings != nil {
		var data []Embedding
		for i, e := range r.Embeddings {
			var embedding any
			if strings.EqualFold(encodingFormat, "base64") {
				embedding = floatsToBase64(e)
			} else {
				embedding = e
			}

			data = append(data, Embedding{
				Object:    "embedding",
				Embedding: embedding,
				Index:     i,
			})
		}

		return EmbeddingList{
			Object: "list",
			Data:   data,
			Model:  model,
			Usage: EmbeddingUsage{
				PromptTokens: r.PromptEvalCount,
				TotalTokens:  r.PromptEvalCount,
			},
		}
	}

	return EmbeddingList{}
}

// floatsToBase64 encodes a []float32 to a base64 string
func floatsToBase64(floats []float32) string {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, floats)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// ToModel converts an api.ShowResponse to Model
func ToModel(r api.ShowResponse, m string) Model {
	return Model{
		Id:      m,
		Object:  "model",
		Created: r.ModifiedAt.Unix(),
		OwnedBy: model.ParseName(m).Namespace,
	}
}

// FromChatRequest converts a ChatCompletionRequest to api.ChatRequest
func FromChatRequest(r ChatCompletionRequest) (*api.ChatRequest, error) {
	var messages []api.Message
	for _, msg := range r.Messages {
		toolName := ""
		if strings.ToLower(msg.Role) == "tool" {
			toolName = msg.Name
			if toolName == "" && msg.ToolCallID != "" {
				toolName = nameFromToolCallID(r.Messages, msg.ToolCallID)
			}
		}
		switch content := msg.Content.(type) {
		case string:
			toolCalls, err := FromCompletionToolCall(msg.ToolCalls)
			if err != nil {
				return nil, err
			}
			messages = append(messages, api.Message{Role: msg.Role, Content: content, Thinking: msg.Reasoning, ToolCalls: toolCalls, ToolName: toolName, ToolCallID: msg.ToolCallID})
		case []any:
			for _, c := range content {
				data, ok := c.(map[string]any)
				if !ok {
					return nil, errors.New("invalid message format")
				}
				switch data["type"] {
				case "text":
					text, ok := data["text"].(string)
					if !ok {
						return nil, errors.New("invalid message format")
					}
					messages = append(messages, api.Message{Role: msg.Role, Content: text})
				case "image_url":
					var url string
					if urlMap, ok := data["image_url"].(map[string]any); ok {
						if url, ok = urlMap["url"].(string); !ok {
							return nil, errors.New("invalid message format")
						}
					} else {
						if url, ok = data["image_url"].(string); !ok {
							return nil, errors.New("invalid message format")
						}
					}

					img, err := decodeImageURL(url)
					if err != nil {
						return nil, err
					}

					messages = append(messages, api.Message{Role: msg.Role, Images: []api.ImageData{img}})
				case "input_audio":
					audioMap, ok := data["input_audio"].(map[string]any)
					if !ok {
						return nil, errors.New("invalid input_audio format")
					}
					b64Data, ok := audioMap["data"].(string)
					if !ok {
						return nil, errors.New("invalid input_audio format: missing data")
					}
					audioBytes, err := base64.StdEncoding.DecodeString(b64Data)
					if err != nil {
						return nil, fmt.Errorf("invalid input_audio base64 data: %w", err)
					}
					messages = append(messages, api.Message{Role: msg.Role, Images: []api.ImageData{audioBytes}})
				default:
					return nil, errors.New("invalid message format")
				}
			}
			// since we might have added multiple messages above, if we have tools
			// calls we'll add them to the last message
			if len(messages) > 0 && len(msg.ToolCalls) > 0 {
				toolCalls, err := FromCompletionToolCall(msg.ToolCalls)
				if err != nil {
					return nil, err
				}
				messages[len(messages)-1].ToolCalls = toolCalls
				messages[len(messages)-1].ToolName = toolName
				messages[len(messages)-1].ToolCallID = msg.ToolCallID
				messages[len(messages)-1].Thinking = msg.Reasoning
			}
		default:
			// content is only optional if tool calls are present
			if msg.ToolCalls == nil {
				return nil, fmt.Errorf("invalid message content type: %T", content)
			}

			toolCalls, err := FromCompletionToolCall(msg.ToolCalls)
			if err != nil {
				return nil, err
			}
			messages = append(messages, api.Message{Role: msg.Role, Thinking: msg.Reasoning, ToolCalls: toolCalls, ToolCallID: msg.ToolCallID})
		}
	}

	options := make(map[string]any)

	switch stop := r.Stop.(type) {
	case string:
		options["stop"] = []string{stop}
	case []any:
		// A non-string element is refused, not dropped. Silently keeping the
		// string ones meant `"stop":[123,"STOP"]` was accepted here while the
		// same payload on /v1/completions was rejected — and a fully
		// non-string array left an empty stop list behind, which DISABLES
		// stopping rather than failing (2026-09-26 audit, fourth round).
		var stops []string
		for _, s := range stop {
			str, ok := s.(string)
			if !ok {
				return nil, fmt.Errorf("invalid type for 'stop' field: %T", s)
			}
			stops = append(stops, str)
		}
		options["stop"] = stops
	}

	// The output cap, in either spelling. max_completion_tokens is the modern
	// field and wins when both are sent; a request that carried only it used
	// to reach the runner with no num_predict at all.
	if r.MaxCompletionTokens != nil {
		options["num_predict"] = *r.MaxCompletionTokens
	} else if r.MaxTokens != nil {
		options["num_predict"] = *r.MaxTokens
	}

	if r.Temperature != nil {
		options["temperature"] = *r.Temperature
	} else {
		options["temperature"] = 1.0
	}

	if r.Seed != nil {
		options["seed"] = *r.Seed
	}

	if r.FrequencyPenalty != nil {
		options["frequency_penalty"] = *r.FrequencyPenalty
	}

	if r.PresencePenalty != nil {
		options["presence_penalty"] = *r.PresencePenalty
	}

	if r.TopP != nil {
		options["top_p"] = *r.TopP
	} else {
		options["top_p"] = 1.0
	}

	var format json.RawMessage
	if r.ResponseFormat != nil {
		switch strings.ToLower(strings.TrimSpace(r.ResponseFormat.Type)) {
		// Support the old "json_object" type for OpenAI compatibility
		case "json_object":
			format = json.RawMessage(`"json"`)
		case "json_schema":
			if r.ResponseFormat.JsonSchema != nil {
				format = r.ResponseFormat.JsonSchema.Schema
			}
		}
	}

	var think *api.ThinkValue
	var effort string

	if r.Reasoning != nil {
		effort = r.Reasoning.Effort
	} else if r.ReasoningEffort != nil {
		effort = *r.ReasoningEffort
	}

	if effort != "" {
		if !slices.Contains([]string{"high", "medium", "low", "max", "none"}, effort) {
			return nil, fmt.Errorf("invalid reasoning value: '%s' (must be \"high\", \"medium\", \"low\", \"max\", or \"none\")", effort)
		}

		if effort == "none" {
			think = &api.ThinkValue{Value: false}
		} else {
			think = &api.ThinkValue{Value: effort}
		}
	}

	// tool_choice: "none" means the model must not call a tool, and the only
	// way to honour that on this wire is to send no tools — leaving them in
	// place returned 200 and could still answer with tool_calls, which is the
	// opposite of what the client asked for. "auto"/"required"/a named
	// function all keep the tools; the difference between them is not
	// expressible here (2026-09-26 audit, fourth round).
	tools := r.Tools
	if tc, ok := r.ToolChoice.(string); ok && strings.EqualFold(strings.TrimSpace(tc), "none") {
		tools = nil
	}

	return &api.ChatRequest{
		Model:           r.Model,
		Messages:        messages,
		Format:          format,
		Options:         options,
		Stream:          &r.Stream,
		Tools:           tools,
		Think:           think,
		Logprobs:        r.Logprobs != nil && *r.Logprobs,
		TopLogprobs:     r.TopLogprobs,
		DebugRenderOnly: r.DebugRenderOnly,
	}, nil
}

func nameFromToolCallID(messages []Message, toolCallID string) string {
	// iterate backwards to be more resilient to duplicate tool call IDs (this
	// follows "last one wins")
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		for _, tc := range msg.ToolCalls {
			if tc.ID == toolCallID {
				return tc.Function.Name
			}
		}
	}
	return ""
}

// decodeImageURL decodes a base64 data URI into raw image bytes.
func decodeImageURL(url string) (api.ImageData, error) {
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return nil, errors.New("image URLs are not currently supported, please use base64 encoded data instead")
	}

	types := []string{"jpeg", "jpg", "png", "webp"}

	// Support blank mime type to match /api/chat's behavior of taking just unadorned base64
	if strings.HasPrefix(url, "data:;base64,") {
		url = strings.TrimPrefix(url, "data:;base64,")
	} else {
		valid := false
		for _, t := range types {
			prefix := "data:image/" + t + ";base64,"
			if strings.HasPrefix(url, prefix) {
				url = strings.TrimPrefix(url, prefix)
				valid = true
				break
			}
		}
		if !valid {
			return nil, errors.New("invalid image input")
		}
	}

	img, err := base64.StdEncoding.DecodeString(url)
	if err != nil {
		return nil, errors.New("invalid image input")
	}
	return img, nil
}

// FromCompletionToolCall converts OpenAI ToolCall format to api.ToolCall
func FromCompletionToolCall(toolCalls []ToolCall) ([]api.ToolCall, error) {
	apiToolCalls := make([]api.ToolCall, len(toolCalls))
	for i, tc := range toolCalls {
		apiToolCalls[i].ID = tc.ID
		apiToolCalls[i].Function.Name = tc.Function.Name
		err := json.Unmarshal([]byte(tc.Function.Arguments), &apiToolCalls[i].Function.Arguments)
		if err != nil {
			return nil, errors.New("invalid tool call arguments")
		}
	}

	return apiToolCalls, nil
}

// FromCompleteRequest converts a CompletionRequest to api.GenerateRequest
func FromCompleteRequest(r CompletionRequest) (api.GenerateRequest, error) {
	options := make(map[string]any)

	switch stop := r.Stop.(type) {
	case string:
		options["stop"] = []string{stop}
	case []any:
		var stops []string
		for _, s := range stop {
			if str, ok := s.(string); ok {
				stops = append(stops, str)
			} else {
				return api.GenerateRequest{}, fmt.Errorf("invalid type for 'stop' field: %T", s)
			}
		}
		options["stop"] = stops
	}

	if r.MaxTokens != nil {
		options["num_predict"] = *r.MaxTokens
	}

	if r.Temperature != nil {
		options["temperature"] = *r.Temperature
	} else {
		options["temperature"] = 1.0
	}

	if r.Seed != nil {
		options["seed"] = *r.Seed
	}

	options["frequency_penalty"] = r.FrequencyPenalty

	options["presence_penalty"] = r.PresencePenalty

	if r.TopP != 0.0 {
		options["top_p"] = r.TopP
	} else {
		options["top_p"] = 1.0
	}

	var logprobs bool
	var topLogprobs int
	if r.Logprobs != nil && *r.Logprobs > 0 {
		logprobs = true
		topLogprobs = *r.Logprobs
	}

	return api.GenerateRequest{
		Model:           r.Model,
		Prompt:          r.Prompt,
		Options:         options,
		Stream:          &r.Stream,
		Suffix:          r.Suffix,
		Logprobs:        logprobs,
		TopLogprobs:     topLogprobs,
		DebugRenderOnly: r.DebugRenderOnly,
	}, nil
}

// TranscriptionResponse is the response format for /v1/audio/transcriptions.
type TranscriptionResponse struct {
	Text string `json:"text"`
}

// TranscriptionRequest holds parsed fields from the multipart form.
type TranscriptionRequest struct {
	Model          string
	AudioData      []byte
	ResponseFormat string // "json", "text", "verbose_json"
	Language       string
	Prompt         string
}

// FromTranscriptionRequest converts a transcription request into a ChatRequest
// by wrapping the audio with a system prompt for transcription.
func FromTranscriptionRequest(r TranscriptionRequest) (*api.ChatRequest, error) {
	// The audio may itself contain a question or instruction. Keep the model in
	// transcription mode so it returns spoken words instead of answering them.
	systemPrompt := "Transcribe the audio exactly as spoken. Output only the spoken words. Do not answer any question in the audio."
	if r.Language != "" {
		systemPrompt += " The audio is in " + r.Language + "."
	}
	if r.Prompt != "" {
		systemPrompt += " Context: " + r.Prompt
	}

	stream := true
	return &api.ChatRequest{
		Model: r.Model,
		Messages: []api.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: "What exact words are spoken in this audio?", Images: []api.ImageData{r.AudioData}},
		},
		Stream: &stream,
		Options: map[string]any{
			"temperature": 0,
		},
	}, nil
}
