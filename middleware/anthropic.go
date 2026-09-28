package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/envconfig"
	internalcloud "github.com/ollama/ollama/internal/cloud"
	"github.com/ollama/ollama/internal/modelref"
	"github.com/ollama/ollama/logutil"
)

// AnthropicWriter wraps the response writer to transform Ollama responses to Anthropic format
type AnthropicWriter struct {
	BaseWriter
	stream    bool
	id        string
	converter *anthropic.StreamConverter
	// estimatedInputTokens is the prompt size this handler worked out for
	// itself, used where the upstream states none. The streaming shape already
	// carries it (NewStreamConverter seeds message_start with it); the
	// non-stream shape applied nothing, so the same turn answered with
	// stream:false reported input_tokens:0 — a context meter that never grew
	// and auto-compaction that never fired, for a request the client can send
	// just as easily (2026-09-27 audit, round 40, C40-6).
	estimatedInputTokens int
}

// withInputEstimate fills in the prompt count a turn's usage does not state. It
// is the converter's own rule (both counts silent ⇒ the estimate), applied
// where the terminal event is written instead — the message_delta of a
// web-search turn is emitted here, and writing input_tokens:0 after
// message_start stated the estimate RESETS the client's context accounting for
// a turn whose size it had already been told (2026-09-27 audit, round 40,
// C40-6). A stated prompt, or a stated cache read, is the upstream's word and
// is left alone — stated, not merely non-zero: the cache field is tested for
// PRESENCE, because an upstream that served the whole prompt from cache states
// input 0 WITH a cache read and one that cached nothing states a real 0, and
// both are readings the estimate must not overwrite. Asking it by value made
// one turn answer two prompt sizes: this arm kept the estimate while the
// streaming arm's message_start stated the upstream's 0 (2026-09-28 audit,
// round 68, F68-L1-2).
func withInputEstimate(u anthropic.Usage, estimate int) anthropic.Usage {
	if estimate > 0 && u.InputTokens == 0 && u.CacheReadInputTokens == nil {
		u.InputTokens = estimate
	}
	return u
}

func (w *AnthropicWriter) writeError(data []byte) (int, error) {
	var errData struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &errData); err != nil {
		// If the error response isn't valid JSON, use the raw bytes as the
		// error message rather than surfacing a confusing JSON parse error.
		errData.Error = string(data)
	}

	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w.ResponseWriter).Encode(anthropic.NewError(w.Status(), errData.Error)); err != nil {
		return 0, err
	}

	return len(data), nil
}

func (w *AnthropicWriter) writeEvent(eventType string, data any) error {
	return writeSSE(w.ResponseWriter, eventType, data)
}

func (w *AnthropicWriter) writeResponse(data []byte) (int, error) {
	var chatResponse api.ChatResponse
	err := json.Unmarshal(data, &chatResponse)
	if err != nil {
		return 0, err
	}

	if w.stream {
		w.ResponseWriter.Header().Set("Content-Type", "text/event-stream")

		events := w.converter.Process(chatResponse)
		logutil.Trace("anthropic middleware: stream chunk", "resp", anthropic.TraceChatResponse(chatResponse), "events", len(events))
		for _, event := range events {
			if err := w.writeEvent(event.Event, event.Data); err != nil {
				return 0, err
			}
		}
		return len(data), nil
	}

	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	response := anthropic.ToMessagesResponse(w.id, chatResponse)
	response.Usage = withInputEstimate(response.Usage, w.estimatedInputTokens)
	logutil.Trace("anthropic middleware: converted response", "resp", anthropic.TraceMessagesResponse(response))
	return len(data), json.NewEncoder(w.ResponseWriter).Encode(response)
}

func (w *AnthropicWriter) Write(data []byte) (int, error) {
	code := w.ResponseWriter.Status()
	if code != http.StatusOK {
		return w.writeError(data)
	}

	return w.writeResponse(data)
}

// WebSearchAnthropicWriter intercepts responses containing web_search tool calls,
// executes the search, re-invokes the model with results, and assembles the
// Anthropic-format response (server_tool_use + web_search_tool_result + text).
type WebSearchAnthropicWriter struct {
	BaseWriter
	newLoopContext func() (context.Context, context.CancelFunc)
	inner          *AnthropicWriter
	req            anthropic.MessagesRequest // original Anthropic request
	chatReq        *api.ChatRequest          // converted Ollama request (for followup calls)
	stream         bool

	estimatedInputTokens int

	terminalSent bool

	observedPromptEvalCount       int
	observedPromptEvalCachedCount *int
	observedEvalCount             int

	loopInFlight         bool
	loopBaseInputTok     int
	loopBaseCacheReadTok *int
	loopBaseOutputTok    int
	loopResultCh         chan webSearchLoopResult

	streamMessageStarted bool
	streamHasOpenBlock   bool
	streamOpenBlockIndex int
	streamNextIndex      int

	// pendingCallChunks holds streaming chunks that carry a tool call but no
	// web_search call, because this arm cannot yet know what the TURN is. A
	// client tool call that arrived before the model asked to search is a call
	// the whole-document arm drops — the loop takes the turn over — but this
	// arm writes chunks as they come, and a tool_use already handed to the
	// client cannot be taken back. So such a chunk waits for the next one: if
	// that carries the search call the held calls are dropped (their text and
	// thinking still go out, which is what the document arm keeps), and
	// otherwise the held chunk goes out unchanged. The wait is one chunk — the
	// runner's tool parser emits each completed call in its own chunk
	// (server/routes.go) — so a turn that never searches streams its calls at
	// the same boundary it always did (2026-09-28 audit, round 70, F70-L1-1).
	pendingCallChunks []api.ChatResponse
	// lateThinking and lateText are the narration of every chunk this arm
	// discarded while the loop was in flight. The loop's terminal response is
	// built by the worker from the ONE chunk that carried the search call, so
	// the model's later prose would otherwise be lost here while the
	// whole-document arm — which sees the merged message — carries it. Merged
	// into the terminal's leading narration at write time, which is after
	// every such chunk has arrived (the terminal is written on the done
	// chunk) (2026-09-28 audit, round 70, F70-L1-2).
	lateThinking string
	lateText     string
}

const maxWebSearchLoops = 3

type webSearchLoopResult struct {
	response anthropic.MessagesResponse
	loopErr  *webSearchLoopError
}

type webSearchLoopError struct {
	code  string
	query string
	usage anthropic.Usage
	err   error
}

func (e *webSearchLoopError) Error() string {
	if e.err == nil {
		return e.code
	}
	return fmt.Sprintf("%s: %v", e.code, e.err)
}

func (w *WebSearchAnthropicWriter) Write(data []byte) (int, error) {
	if w.terminalSent {
		return len(data), nil
	}

	code := w.Status()
	if code != http.StatusOK {
		return w.inner.writeError(data)
	}

	var chatResponse api.ChatResponse
	if err := json.Unmarshal(data, &chatResponse); err != nil {
		return 0, err
	}
	w.recordObservedUsage(chatResponse.Metrics)

	if w.stream && w.loopInFlight {
		w.absorbLateNarration(chatResponse)
		if !chatResponse.Done {
			return len(data), nil
		}
		if err := w.writeLoopResult(); err != nil {
			return len(data), err
		}
		return len(data), nil
	}

	webSearchCall, hasWebSearch, hasOtherTools := findWebSearchToolCall(chatResponse.Message.ToolCalls)
	logutil.Trace("anthropic middleware: upstream chunk",
		"resp", anthropic.TraceChatResponse(chatResponse),
		"web_search", hasWebSearch,
		"other_tools", hasOtherTools,
	)
	if hasWebSearch && hasOtherTools {
		// Prefer web_search if both server and client tools are present in one chunk.
		slog.Debug("preferring web_search tool call over client tool calls in mixed tool response")
	}

	if !hasWebSearch {
		if w.stream {
			if !chatResponse.Done && len(chatResponse.Message.ToolCalls) > 0 {
				// A call the model asked for, in a turn that may still turn out
				// to be a search turn. Held, not written: see the field.
				w.pendingCallChunks = append(w.pendingCallChunks, chatResponse)
				return len(data), nil
			}
			if err := w.releasePendingCallChunks(true); err != nil {
				return 0, err
			}
			if err := w.writePassthroughStreamChunk(chatResponse); err != nil {
				return 0, err
			}
			return len(data), nil
		}
		return w.inner.writeResponse(data)
	}

	if w.stream {
		// The turn is a search turn, so the calls it asked for before it asked
		// to search are ones the whole-document arm drops: release the held
		// chunks without them.
		if err := w.releasePendingCallChunks(false); err != nil {
			return 0, err
		}
		// Let the original generation continue to completion while web search runs in parallel.
		logutil.Trace("anthropic middleware: starting async web_search loop",
			"tool_call", anthropic.TraceToolCall(webSearchCall),
			"resp", anthropic.TraceChatResponse(chatResponse),
		)
		w.startLoopWorker(chatResponse, webSearchCall)
		if chatResponse.Done {
			if err := w.writeLoopResult(); err != nil {
				return len(data), err
			}
		}
		return len(data), nil
	}

	loopCtx, cancel := w.startLoopContext()
	defer cancel()

	initialUsage := w.usageWithObservedMetrics(chatResponse.Metrics)
	logutil.Trace("anthropic middleware: starting sync web_search loop",
		"tool_call", anthropic.TraceToolCall(webSearchCall),
		"resp", anthropic.TraceChatResponse(chatResponse),
		"usage", initialUsage,
	)
	response, loopErr := w.runWebSearchLoop(loopCtx, chatResponse, webSearchCall, initialUsage)
	if loopErr != nil {
		return len(data), w.sendError(loopErr.code, loopErr.query, loopErr.usage)
	}

	if err := w.writeTerminalResponse(response); err != nil {
		return 0, err
	}

	return len(data), nil
}

func (w *WebSearchAnthropicWriter) runWebSearchLoop(ctx context.Context, initialResponse api.ChatResponse, initialToolCall api.ToolCall, initialUsage anthropic.Usage) (anthropic.MessagesResponse, *webSearchLoopError) {
	followUpMessages := make([]api.Message, 0, len(w.chatReq.Messages)+maxWebSearchLoops*2)
	followUpMessages = append(followUpMessages, w.chatReq.Messages...)

	followUpTools := append(api.Tools(nil), w.chatReq.Tools...)
	usage := initialUsage
	logutil.TraceContext(ctx, "anthropic middleware: web_search loop init",
		"model", w.req.Model,
		"tool_call", anthropic.TraceToolCall(initialToolCall),
		"messages", len(followUpMessages),
		"tools", len(followUpTools),
		"max_loops", maxWebSearchLoops,
	)

	currentResponse := initialResponse
	currentToolCall := initialToolCall

	var serverContent []anthropic.ContentBlock

	for loop := 1; loop <= maxWebSearchLoops; loop++ {
		query := extractQueryFromToolCall(&currentToolCall)
		logutil.TraceContext(ctx, "anthropic middleware: web_search loop iteration",
			"loop", loop,
			"query", anthropic.TraceTruncateString(query),
			"messages", len(followUpMessages),
		)
		if query == "" {
			return anthropic.MessagesResponse{}, &webSearchLoopError{
				code:  "invalid_request",
				query: "",
				usage: usage,
			}
		}

		const defaultMaxResults = 5
		searchResp, err := anthropic.WebSearch(ctx, query, defaultMaxResults)
		if err != nil {
			logutil.TraceContext(ctx, "anthropic middleware: web_search request failed",
				"loop", loop,
				"query", query,
				"error", err,
			)
			return anthropic.MessagesResponse{}, &webSearchLoopError{
				code:  "unavailable",
				query: query,
				usage: usage,
				err:   err,
			}
		}
		logutil.TraceContext(ctx, "anthropic middleware: web_search results",
			"loop", loop,
			"results", len(searchResp.Results),
		)

		toolUseID := loopServerToolUseID(w.inner.id, loop)
		searchResults := anthropic.ConvertOllamaToAnthropicResults(searchResp)
		// The turn that asked for this search leads with what it said before
		// asking; it is prepended here rather than at the terminal response so
		// each iteration's narration sits before its own search block.
		serverContent = append(serverContent, carriedNarrationBlocks(currentResponse)...)
		serverContent = append(serverContent,
			anthropic.ContentBlock{
				Type:  "server_tool_use",
				ID:    toolUseID,
				Name:  "web_search",
				Input: queryArgs(query),
			},
			anthropic.ContentBlock{
				Type:      "web_search_tool_result",
				ToolUseID: toolUseID,
				Content:   searchResults,
			},
		)

		assistantMsg := buildWebSearchAssistantMessage(currentResponse, currentToolCall)
		toolResultMsg := api.Message{
			Role:       "tool",
			Content:    formatWebSearchResultsForToolMessage(searchResp.Results),
			ToolCallID: currentToolCall.ID,
		}
		followUpMessages = append(followUpMessages, assistantMsg, toolResultMsg)

		followUpResponse, err := w.callFollowUpChat(ctx, followUpMessages, followUpTools)
		if err != nil {
			logutil.TraceContext(ctx, "anthropic middleware: followup /api/chat failed",
				"loop", loop,
				"query", query,
				"error", err,
			)
			return anthropic.MessagesResponse{}, &webSearchLoopError{
				code:  "api_error",
				query: query,
				usage: usage,
				err:   err,
			}
		}
		logutil.TraceContext(ctx, "anthropic middleware: followup response",
			"loop", loop,
			"resp", anthropic.TraceChatResponse(followUpResponse),
		)

		followUpUsage := anthropic.UsageFromMetrics(followUpResponse.Metrics)
		usage.InputTokens += followUpUsage.InputTokens
		usage.CacheReadInputTokens = addOptionalInts(usage.CacheReadInputTokens, followUpUsage.CacheReadInputTokens)
		usage.OutputTokens += followUpUsage.OutputTokens

		nextToolCall, hasWebSearch, hasOtherTools := findWebSearchToolCall(followUpResponse.Message.ToolCalls)
		if hasWebSearch && hasOtherTools {
			// Prefer web_search if both server and client tools are present in one chunk.
			slog.Debug("preferring web_search tool call over client tool calls in mixed followup response")
		}

		if !hasWebSearch {
			finalResponse := w.combineServerAndFinalContent(serverContent, followUpResponse, usage)
			logutil.TraceContext(ctx, "anthropic middleware: web_search loop complete",
				"loop", loop,
				"resp", anthropic.TraceMessagesResponse(finalResponse),
			)
			return finalResponse, nil
		}

		currentResponse = followUpResponse
		currentToolCall = nextToolCall
	}

	maxLoopQuery := extractQueryFromToolCall(&currentToolCall)
	maxLoopToolUseID := loopServerToolUseID(w.inner.id, maxWebSearchLoops+1)
	// The last turn's narration, dropped by the same rule: it carried the call
	// that ran the loop out.
	serverContent = append(serverContent, carriedNarrationBlocks(currentResponse)...)
	serverContent = append(serverContent,
		anthropic.ContentBlock{
			Type:  "server_tool_use",
			ID:    maxLoopToolUseID,
			Name:  "web_search",
			Input: queryArgs(maxLoopQuery),
		},
		anthropic.ContentBlock{
			Type:      "web_search_tool_result",
			ToolUseID: maxLoopToolUseID,
			Content: anthropic.WebSearchToolResultError{
				Type:      "web_search_tool_result_error",
				ErrorCode: "max_uses_exceeded",
			},
		},
	)

	maxResponse := anthropic.MessagesResponse{
		ID:         w.inner.id,
		Type:       "message",
		Role:       "assistant",
		Model:      w.req.Model,
		Content:    serverContent,
		StopReason: "end_turn",
		Usage:      usage,
	}
	logutil.TraceContext(ctx, "anthropic middleware: web_search loop max reached",
		"resp", anthropic.TraceMessagesResponse(maxResponse),
	)
	return maxResponse, nil
}

func (w *WebSearchAnthropicWriter) startLoopWorker(initialResponse api.ChatResponse, initialToolCall api.ToolCall) {
	if w.loopInFlight {
		return
	}

	initialUsage := w.usageWithObservedMetrics(initialResponse.Metrics)
	w.loopBaseInputTok = initialUsage.InputTokens
	w.loopBaseCacheReadTok = initialUsage.CacheReadInputTokens
	w.loopBaseOutputTok = initialUsage.OutputTokens
	w.loopResultCh = make(chan webSearchLoopResult, 1)
	w.loopInFlight = true
	logutil.Trace("anthropic middleware: loop worker started",
		"usage", initialUsage,
		"tool_call", anthropic.TraceToolCall(initialToolCall),
	)

	go func() {
		ctx, cancel := w.startLoopContext()
		defer cancel()

		response, loopErr := w.runWebSearchLoop(ctx, initialResponse, initialToolCall, initialUsage)
		w.loopResultCh <- webSearchLoopResult{
			response: response,
			loopErr:  loopErr,
		}
	}()
}

func (w *WebSearchAnthropicWriter) writeLoopResult() error {
	if w.loopResultCh == nil {
		return w.sendError("api_error", "", w.currentObservedUsage())
	}

	result := <-w.loopResultCh
	w.loopResultCh = nil
	w.loopInFlight = false
	if result.loopErr != nil {
		logutil.Trace("anthropic middleware: loop worker returned error",
			"code", result.loopErr.code,
			"query", result.loopErr.query,
			"usage", result.loopErr.usage,
			"error", result.loopErr.err,
		)
		usage := result.loopErr.usage
		w.applyObservedUsageDeltaToUsage(&usage)
		return w.sendError(result.loopErr.code, result.loopErr.query, usage)
	}
	logutil.Trace("anthropic middleware: loop worker done", "resp", anthropic.TraceMessagesResponse(result.response))

	w.applyObservedUsageDelta(&result.response)
	w.mergeLateNarration(&result.response)
	return w.writeTerminalResponse(result.response)
}

func (w *WebSearchAnthropicWriter) applyObservedUsageDelta(response *anthropic.MessagesResponse) {
	w.applyObservedUsageDeltaToUsage(&response.Usage)
}

func (w *WebSearchAnthropicWriter) recordObservedUsage(metrics api.Metrics) {
	if metrics.PromptEvalCount > w.observedPromptEvalCount {
		w.observedPromptEvalCount = metrics.PromptEvalCount
	}
	w.observedPromptEvalCachedCount = maxOptionalInts(w.observedPromptEvalCachedCount, metrics.PromptEvalCachedCount)
	if metrics.EvalCount > w.observedEvalCount {
		w.observedEvalCount = metrics.EvalCount
	}
}

func (w *WebSearchAnthropicWriter) applyObservedUsageDeltaToUsage(usage *anthropic.Usage) {
	observed := w.currentObservedUsage()
	if delta := observed.InputTokens - w.loopBaseInputTok; delta > 0 {
		usage.InputTokens += delta
	}
	if observed.CacheReadInputTokens != nil {
		delta := max(0, optionalIntValue(observed.CacheReadInputTokens)-optionalIntValue(w.loopBaseCacheReadTok))
		usage.CacheReadInputTokens = addOptionalInts(usage.CacheReadInputTokens, &delta)
	}
	if deltaOut := w.observedEvalCount - w.loopBaseOutputTok; deltaOut > 0 {
		usage.OutputTokens += deltaOut
	}
}

func (w *WebSearchAnthropicWriter) currentObservedUsage() anthropic.Usage {
	return anthropic.UsageFromMetrics(api.Metrics{
		PromptEvalCount:       w.observedPromptEvalCount,
		PromptEvalCachedCount: w.observedPromptEvalCachedCount,
		EvalCount:             w.observedEvalCount,
	})
}

func (w *WebSearchAnthropicWriter) usageWithObservedMetrics(metrics api.Metrics) anthropic.Usage {
	metrics.PromptEvalCount = max(metrics.PromptEvalCount, w.observedPromptEvalCount)
	metrics.PromptEvalCachedCount = maxOptionalInts(metrics.PromptEvalCachedCount, w.observedPromptEvalCachedCount)
	metrics.EvalCount = max(metrics.EvalCount, w.observedEvalCount)
	return anthropic.UsageFromMetrics(metrics)
}

func (w *WebSearchAnthropicWriter) startLoopContext() (context.Context, context.CancelFunc) {
	if w.newLoopContext != nil {
		return w.newLoopContext()
	}
	return context.WithTimeout(context.Background(), 5*time.Minute)
}

func (w *WebSearchAnthropicWriter) combineServerAndFinalContent(serverContent []anthropic.ContentBlock, finalResponse api.ChatResponse, usage anthropic.Usage) anthropic.MessagesResponse {
	converted := anthropic.ToMessagesResponse(w.inner.id, finalResponse)

	content := make([]anthropic.ContentBlock, 0, len(serverContent)+len(converted.Content))
	content = append(content, serverContent...)
	content = append(content, converted.Content...)

	return anthropic.MessagesResponse{
		ID:           w.inner.id,
		Type:         "message",
		Role:         "assistant",
		Model:        w.req.Model,
		Content:      content,
		StopReason:   converted.StopReason,
		StopSequence: converted.StopSequence,
		Usage:        usage,
	}
}

// carriedNarrationBlocks is the narration of the response that carried a
// web_search call — what the model said and thought before it asked to search —
// as content blocks, in the order the converter writes them.
//
// The loop consumes that response: neither arm serves it as it stands. The
// passthrough arm relays only the chunks that carried no call, so the chunk
// that carries the call is the one chunk of a turn whose text is not streamed
// (and a turn can put its text and its call in one chunk); the non-stream arm
// serves the loop's terminal response alone. Left out, the same completion
// reached the client with the model's text on one wire and without it on the
// other, and the text of every further loop iteration was lost on both
// (2026-09-28 audit, round 56, F56-2).
func carriedNarrationBlocks(response api.ChatResponse) []anthropic.ContentBlock {
	var blocks []anthropic.ContentBlock
	if response.Message.Thinking != "" {
		thinking := response.Message.Thinking
		blocks = append(blocks, anthropic.ContentBlock{Type: "thinking", Thinking: &thinking})
	}
	if response.Message.Content != "" {
		text := response.Message.Content
		blocks = append(blocks, anthropic.ContentBlock{Type: "text", Text: &text})
	}
	return blocks
}

func buildWebSearchAssistantMessage(response api.ChatResponse, webSearchCall api.ToolCall) api.Message {
	assistantMsg := api.Message{
		Role:      "assistant",
		ToolCalls: []api.ToolCall{webSearchCall},
	}
	if response.Message.Content != "" {
		assistantMsg.Content = response.Message.Content
	}
	if response.Message.Thinking != "" {
		assistantMsg.Thinking = response.Message.Thinking
	}
	return assistantMsg
}

func formatWebSearchResultsForToolMessage(results []anthropic.OllamaWebSearchResult) string {
	var resultText strings.Builder
	for _, r := range results {
		fmt.Fprintf(&resultText, "Title: %s\nURL: %s\n", r.Title, r.URL)
		if r.Content != "" {
			fmt.Fprintf(&resultText, "Content: %s\n", r.Content)
		}
		resultText.WriteString("\n")
	}
	return resultText.String()
}

func findWebSearchToolCall(toolCalls []api.ToolCall) (api.ToolCall, bool, bool) {
	var webSearchCall api.ToolCall
	hasWebSearch := false
	hasOtherTools := false

	for _, toolCall := range toolCalls {
		if toolCall.Function.Name == "web_search" {
			if !hasWebSearch {
				webSearchCall = toolCall
				hasWebSearch = true
			}
			continue
		}
		hasOtherTools = true
	}

	return webSearchCall, hasWebSearch, hasOtherTools
}

func loopServerToolUseID(messageID string, loop int) string {
	base := serverToolUseID(messageID)
	if loop <= 1 {
		return base
	}
	return fmt.Sprintf("%s_%d", base, loop)
}

func (w *WebSearchAnthropicWriter) callFollowUpChat(ctx context.Context, messages []api.Message, tools api.Tools) (api.ChatResponse, error) {
	streaming := false
	followUp := api.ChatRequest{
		Model:    w.chatReq.Model,
		Messages: messages,
		Stream:   &streaming,
		Tools:    tools,
		Options:  w.chatReq.Options,
	}

	body, err := json.Marshal(followUp)
	if err != nil {
		return api.ChatResponse{}, err
	}

	chatURL := envconfig.Host().String() + "/api/chat"
	logutil.TraceContext(ctx, "anthropic middleware: followup request",
		"url", chatURL,
		"req", anthropic.TraceChatRequest(&followUp),
	)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", chatURL, bytes.NewReader(body))
	if err != nil {
		return api.ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return api.ChatResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		logutil.TraceContext(ctx, "anthropic middleware: followup non-200 response",
			"status", resp.StatusCode,
			"response", strings.TrimSpace(string(respBody)),
		)
		return api.ChatResponse{}, fmt.Errorf("followup /api/chat returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var chatResp api.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return api.ChatResponse{}, err
	}
	logutil.TraceContext(ctx, "anthropic middleware: followup decoded", "resp", anthropic.TraceChatResponse(chatResp))

	return chatResp, nil
}

// absorbLateNarration records the narration of a chunk this arm is about to
// discard, so the loop's terminal can carry it (F70-L1-2).
func (w *WebSearchAnthropicWriter) absorbLateNarration(chatResponse api.ChatResponse) {
	w.lateThinking += chatResponse.Message.Thinking
	w.lateText += chatResponse.Message.Content
}

// releasePendingCallChunks writes out the streaming chunks this arm held back
// because they carried a tool call. keepCalls is the decision they were held
// for: true once the turn is known not to be a search turn (the calls are the
// client's, and go out as they stand), false when a web_search call arrived
// (the loop takes the turn over, so the calls are dropped and only the text
// and thinking of those chunks — what the document arm keeps — are relayed).
func (w *WebSearchAnthropicWriter) releasePendingCallChunks(keepCalls bool) error {
	pending := w.pendingCallChunks
	w.pendingCallChunks = nil
	for _, chunk := range pending {
		if !keepCalls {
			chunk.Message.ToolCalls = nil
			// Nothing left of it: a chunk that carried only the call says
			// nothing once the call is gone, and writing it would open a block
			// for an empty message.
			if chunk.Message.Content == "" && chunk.Message.Thinking == "" {
				continue
			}
		}
		if err := w.writePassthroughStreamChunk(chunk); err != nil {
			return err
		}
	}
	return nil
}

// mergeLateNarration folds the narration absorbed while the loop was in flight
// into the loop's terminal response, replacing the response's leading narration
// run (what carriedNarrationBlocks wrote for the turn that asked to search).
// Appending to that run rather than adding blocks after it is what keeps the
// streamed turn's block order the whole-document arm's, where the model's whole
// prose is one merged message and its narration is emitted together.
func (w *WebSearchAnthropicWriter) mergeLateNarration(response *anthropic.MessagesResponse) {
	if w.lateThinking == "" && w.lateText == "" {
		return
	}
	lead := 0
	for lead < len(response.Content) && (response.Content[lead].Type == "thinking" || response.Content[lead].Type == "text") {
		lead++
	}
	thinking := ""
	text := ""
	for _, b := range response.Content[:lead] {
		if b.Type == "thinking" && b.Thinking != nil {
			thinking += *b.Thinking
		}
		if b.Type == "text" && b.Text != nil {
			text += *b.Text
		}
	}
	thinking += w.lateThinking
	text += w.lateText
	combined := make([]anthropic.ContentBlock, 0, 2)
	if thinking != "" {
		t := thinking
		combined = append(combined, anthropic.ContentBlock{Type: "thinking", Thinking: &t})
	}
	if text != "" {
		t := text
		combined = append(combined, anthropic.ContentBlock{Type: "text", Text: &t})
	}
	response.Content = append(combined, response.Content[lead:]...)
}

func (w *WebSearchAnthropicWriter) writePassthroughStreamChunk(chatResponse api.ChatResponse) error {
	events := w.inner.converter.Process(chatResponse)
	for _, event := range events {
		switch e := event.Data.(type) {
		case anthropic.MessageStartEvent:
			w.streamMessageStarted = true
		case anthropic.ContentBlockStartEvent:
			w.streamHasOpenBlock = true
			w.streamOpenBlockIndex = e.Index
			if e.Index+1 > w.streamNextIndex {
				w.streamNextIndex = e.Index + 1
			}
		case anthropic.ContentBlockStopEvent:
			if w.streamHasOpenBlock && w.streamOpenBlockIndex == e.Index {
				w.streamHasOpenBlock = false
			}
			if e.Index+1 > w.streamNextIndex {
				w.streamNextIndex = e.Index + 1
			}
		case anthropic.MessageStopEvent:
			w.terminalSent = true
		}

		if err := writeSSE(w.ResponseWriter, event.Event, event.Data); err != nil {
			return err
		}
	}

	return nil
}

func (w *WebSearchAnthropicWriter) ensureStreamMessageStart(usage anthropic.Usage) error {
	if w.streamMessageStarted {
		return nil
	}

	// The estimate is filled in only where the upstream stated NOTHING: the
	// cache read is tested for presence, exactly as withInputEstimate above (and
	// the converter) tests it — a stated zero is a reading. This site is the
	// last one to keep asking by value, and it is the site that matters most on
	// this route: writeTerminalResponse runs withInputEstimate BEFORE this call,
	// so a usage reaching here with input_tokens 0 states a cache read (or has
	// no size to estimate at all), which makes the by-value branch's only live
	// effect the substitution of the estimate for a stated zero. Measured: a
	// turn whose upstream states {"prompt_eval_cached_count": 0} and no prompt
	// count answered message_start input_tokens 82 and message_delta 0, while
	// the whole-document arm of the same handler answered 0 (2026-09-28 audit,
	// round 69, F69-L1-1).
	inputTokens := usage.InputTokens
	if inputTokens == 0 && usage.CacheReadInputTokens == nil {
		inputTokens = w.estimatedInputTokens
	}

	if err := writeSSE(w.ResponseWriter, "message_start", anthropic.MessageStartEvent{
		Type: "message_start",
		Message: anthropic.MessagesResponse{
			ID:      w.inner.id,
			Type:    "message",
			Role:    "assistant",
			Model:   w.req.Model,
			Content: []anthropic.ContentBlock{},
			Usage: anthropic.Usage{
				InputTokens:          inputTokens,
				CacheReadInputTokens: usage.CacheReadInputTokens,
			},
		},
	}); err != nil {
		return err
	}

	w.streamMessageStarted = true
	return nil
}

func (w *WebSearchAnthropicWriter) closeOpenStreamBlock() error {
	if !w.streamHasOpenBlock {
		return nil
	}

	if err := writeSSE(w.ResponseWriter, "content_block_stop", anthropic.ContentBlockStopEvent{
		Type:  "content_block_stop",
		Index: w.streamOpenBlockIndex,
	}); err != nil {
		return err
	}

	if w.streamOpenBlockIndex+1 > w.streamNextIndex {
		w.streamNextIndex = w.streamOpenBlockIndex + 1
	}
	w.streamHasOpenBlock = false
	return nil
}

func (w *WebSearchAnthropicWriter) writeStreamContentBlocks(content []anthropic.ContentBlock) error {
	for _, block := range content {
		index := w.streamNextIndex
		if block.Type == "text" {
			emptyText := ""
			if err := writeSSE(w.ResponseWriter, "content_block_start", anthropic.ContentBlockStartEvent{
				Type:  "content_block_start",
				Index: index,
				ContentBlock: anthropic.ContentBlock{
					Type: "text",
					Text: &emptyText,
				},
			}); err != nil {
				return err
			}

			text := ""
			if block.Text != nil {
				text = *block.Text
			}
			if err := writeSSE(w.ResponseWriter, "content_block_delta", anthropic.ContentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: index,
				Delta: anthropic.Delta{
					Type: "text_delta",
					Text: text,
				},
			}); err != nil {
				return err
			}
		} else if block.Type == "thinking" {
			// A thinking block, like a tool_use one above, is accumulated from
			// its deltas: the agent shim reads thinking text from
			// thinking_delta and never from the start event's block
			// (cmd/agent/sse.go), so a thought written whole inside
			// content_block_start reached the engine as nothing. The converter
			// emits this same shape (anthropic.StreamConverter).
			emptyThinking := ""
			if err := writeSSE(w.ResponseWriter, "content_block_start", anthropic.ContentBlockStartEvent{
				Type:  "content_block_start",
				Index: index,
				ContentBlock: anthropic.ContentBlock{
					Type:     "thinking",
					Thinking: &emptyThinking,
				},
			}); err != nil {
				return err
			}

			thinking := ""
			if block.Thinking != nil {
				thinking = *block.Thinking
			}
			if err := writeSSE(w.ResponseWriter, "content_block_delta", anthropic.ContentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: index,
				Delta: anthropic.Delta{
					Type:     "thinking_delta",
					Thinking: thinking,
				},
			}); err != nil {
				return err
			}
		} else if block.Type == "tool_use" {
			// A tool_use block opens with an EMPTY input and its arguments
			// arrive as an input_json_delta — the framing the converter one
			// branch over in this file emits for the same block
			// (anthropic.StreamConverter). Written whole inside
			// content_block_start, as this arm did, the arguments reached only
			// a client that reads the start event's input as the base object:
			// the agent shim accumulates a call from input_json_delta alone
			// (cmd/agent/sse.go, the rule the SDKs use too), so a web-search
			// turn handed the engine a tool call with the arguments gone, while
			// the same block served through this handler's passthrough path
			// carried them (2026-09-27 audit, round 53).
			argsJSON, err := json.Marshal(block.Input)
			if err != nil {
				return err
			}
			if err := writeSSE(w.ResponseWriter, "content_block_start", anthropic.ContentBlockStartEvent{
				Type:  "content_block_start",
				Index: index,
				ContentBlock: anthropic.ContentBlock{
					Type: "tool_use",
					ID:   block.ID,
					Name: block.Name,
					// The empty input the client accumulates onto: a start with
					// no input key at all is a block shape Claude Code parses
					// differently, and the converter above emits this one.
					Input: api.NewToolCallFunctionArguments(),
				},
			}); err != nil {
				return err
			}
			if err := writeSSE(w.ResponseWriter, "content_block_delta", anthropic.ContentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: index,
				Delta: anthropic.Delta{
					Type:        "input_json_delta",
					PartialJSON: string(argsJSON),
				},
			}); err != nil {
				return err
			}
		} else {
			if err := writeSSE(w.ResponseWriter, "content_block_start", anthropic.ContentBlockStartEvent{
				Type:         "content_block_start",
				Index:        index,
				ContentBlock: block,
			}); err != nil {
				return err
			}
		}

		if err := writeSSE(w.ResponseWriter, "content_block_stop", anthropic.ContentBlockStopEvent{
			Type:  "content_block_stop",
			Index: index,
		}); err != nil {
			return err
		}

		w.streamNextIndex++
	}

	return nil
}

func (w *WebSearchAnthropicWriter) writeTerminalResponse(response anthropic.MessagesResponse) error {
	if w.terminalSent {
		return nil
	}

	response.Usage = withInputEstimate(response.Usage, w.estimatedInputTokens)

	if !w.stream {
		w.ResponseWriter.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w.ResponseWriter).Encode(response); err != nil {
			return err
		}
		w.terminalSent = true
		return nil
	}

	if err := w.ensureStreamMessageStart(response.Usage); err != nil {
		return err
	}
	if err := w.closeOpenStreamBlock(); err != nil {
		return err
	}
	if err := w.writeStreamContentBlocks(response.Content); err != nil {
		return err
	}

	if err := writeSSE(w.ResponseWriter, "message_delta", anthropic.MessageDeltaEvent{
		Type: "message_delta",
		Delta: anthropic.MessageDelta{
			StopReason: response.StopReason,
		},
		Usage: anthropic.DeltaUsage{
			InputTokens:          response.Usage.InputTokens,
			CacheReadInputTokens: response.Usage.CacheReadInputTokens,
			OutputTokens:         response.Usage.OutputTokens,
		},
	}); err != nil {
		return err
	}

	if err := writeSSE(w.ResponseWriter, "message_stop", anthropic.MessageStopEvent{
		Type: "message_stop",
	}); err != nil {
		return err
	}

	w.terminalSent = true
	return nil
}

// streamResponse emits a complete MessagesResponse as SSE events.
func (w *WebSearchAnthropicWriter) streamResponse(response anthropic.MessagesResponse) error {
	return w.writeTerminalResponse(response)
}

func (w *WebSearchAnthropicWriter) webSearchErrorResponse(errorCode, query string, usage anthropic.Usage) anthropic.MessagesResponse {
	toolUseID := serverToolUseID(w.inner.id)

	return anthropic.MessagesResponse{
		ID:    w.inner.id,
		Type:  "message",
		Role:  "assistant",
		Model: w.req.Model,
		Content: []anthropic.ContentBlock{
			{
				Type:  "server_tool_use",
				ID:    toolUseID,
				Name:  "web_search",
				Input: queryArgs(query),
			},
			{
				Type:      "web_search_tool_result",
				ToolUseID: toolUseID,
				Content: anthropic.WebSearchToolResultError{
					Type:      "web_search_tool_result_error",
					ErrorCode: errorCode,
				},
			},
		},
		StopReason: "end_turn",
		Usage:      usage,
	}
}

// sendError sends a web search error response.
func (w *WebSearchAnthropicWriter) sendError(errorCode, query string, usage anthropic.Usage) error {
	response := w.webSearchErrorResponse(errorCode, query, usage)
	logutil.Trace("anthropic middleware: web_search error", "code", errorCode, "query", query, "usage", usage)
	return w.writeTerminalResponse(response)
}

// AnthropicMessagesMiddleware handles Anthropic Messages API requests
func AnthropicMessagesMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestCtx := c.Request.Context()

		var req anthropic.MessagesRequest
		err := c.ShouldBindJSON(&req)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, anthropic.NewError(http.StatusBadRequest, err.Error()))
			return
		}

		if req.Model == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, anthropic.NewError(http.StatusBadRequest, "model is required"))
			return
		}

		if req.MaxTokens <= 0 {
			c.AbortWithStatusJSON(http.StatusBadRequest, anthropic.NewError(http.StatusBadRequest, "max_tokens is required and must be positive"))
			return
		}

		if len(req.Messages) == 0 {
			c.AbortWithStatusJSON(http.StatusBadRequest, anthropic.NewError(http.StatusBadRequest, "messages is required"))
			return
		}

		chatReq, err := anthropic.FromMessagesRequest(req)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, anthropic.NewError(http.StatusBadRequest, err.Error()))
			return
		}

		// A url-sourced image cannot be carried to this leg's backend, which
		// takes image BYTES: the address travelled through api.Message.Images as
		// its own characters, and llm.NewMediaData sniffed those characters as
		// text/plain, forced them to image/jpeg and base64'd them — the model was
		// shown a picture of the URL text while the client had pointed at a
		// screenshot it never sent, and the turn was answered 200. The client
		// leg and the gateway both hand a url source to their backend as a URL;
		// this one has no fetcher, so it refuses in words, the same verdict the
		// converter gives any source the wire cannot express. A client that
		// wants a local model to see a url image has to send its bytes
		// (2026-09-27 audit, round 41, C41-13).
		for _, m := range chatReq.Messages {
			for _, img := range m.Images {
				if anthropic.IsImageURL(img) {
					c.AbortWithStatusJSON(http.StatusBadRequest, anthropic.NewError(http.StatusBadRequest,
						`image source.type "url" cannot be represented on this leg: the model is given image bytes, and this url arrived as the address text. Send the image as a base64 source instead.`))
					return
				}
			}
		}

		// Set think to nil when being used with Anthropic API to connect to tools like claude code
		c.Set("relax_thinking", true)

		var b bytes.Buffer
		if err := json.NewEncoder(&b).Encode(chatReq); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, anthropic.NewError(http.StatusInternalServerError, err.Error()))
			return
		}

		c.Request.Body = io.NopCloser(&b)

		messageID := anthropic.GenerateMessageID()

		// Estimate input tokens for streaming (actual count not available until generation completes)
		estimatedTokens := anthropic.EstimateInputTokens(req)

		innerWriter := &AnthropicWriter{
			BaseWriter:           BaseWriter{ResponseWriter: c.Writer},
			stream:               req.Stream,
			id:                   messageID,
			converter:            anthropic.NewStreamConverter(messageID, req.Model, estimatedTokens),
			estimatedInputTokens: estimatedTokens,
		}

		if req.Stream {
			c.Writer.Header().Set("Content-Type", "text/event-stream")
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.Writer.Header().Set("Connection", "keep-alive")
		}

		if hasWebSearchTool(req.Tools) {
			// Guard against runtime cloud-disable policy (OLLAMA_NO_CLOUD/server.json)
			// for cloud models. Local models may still receive web_search tool definitions;
			// execution is validated when the model actually emits a web_search tool call.
			if isCloudModelName(req.Model) {
				if disabled, _ := internalcloud.Status(); disabled {
					c.AbortWithStatusJSON(http.StatusForbidden, anthropic.NewError(http.StatusForbidden, internalcloud.DisabledError("web search is unavailable")))
					return
				}
			}

			c.Writer = &WebSearchAnthropicWriter{
				BaseWriter: BaseWriter{ResponseWriter: c.Writer},
				newLoopContext: func() (context.Context, context.CancelFunc) {
					return context.WithTimeout(requestCtx, 5*time.Minute)
				},
				inner:                innerWriter,
				req:                  req,
				chatReq:              chatReq,
				stream:               req.Stream,
				estimatedInputTokens: estimatedTokens,
			}
		} else {
			c.Writer = innerWriter
		}

		c.Next()
	}
}

// hasWebSearchTool checks if the request tools include a web_search tool
func hasWebSearchTool(tools []anthropic.Tool) bool {
	for _, tool := range tools {
		if strings.HasPrefix(tool.Type, "web_search") {
			return true
		}
	}
	return false
}

func isCloudModelName(name string) bool {
	return modelref.HasExplicitCloudSource(name)
}

// extractQueryFromToolCall extracts the search query from a web_search tool call
func extractQueryFromToolCall(tc *api.ToolCall) string {
	q, ok := tc.Function.Arguments.Get("query")
	if !ok {
		return ""
	}
	if s, ok := q.(string); ok {
		return s
	}
	return ""
}

// writeSSE writes a Server-Sent Event
func writeSSE(w http.ResponseWriter, eventType string, data any) error {
	d, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, d); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// queryArgs creates a ToolCallFunctionArguments with a single "query" key.
func queryArgs(query string) api.ToolCallFunctionArguments {
	args := api.NewToolCallFunctionArguments()
	args.Set("query", query)
	return args
}

// serverToolUseID derives a server tool use ID from a message ID
func serverToolUseID(messageID string) string {
	return "srvtoolu_" + strings.TrimPrefix(messageID, "msg_")
}
