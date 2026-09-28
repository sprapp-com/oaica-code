package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
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
	// failed is set when an error frame ended this turn. Everything after it is
	// nothing: the client has been told why the turn stopped, and a converter
	// fed the frames that follow would write content events for a turn that is
	// over (2026-09-28 audit, round 72, F72-L1-1).
	failed bool
}

// upstreamErrorFrame reads the error frame the chat path puts into a 200 stream
// when the generation dies partway (server/routes.go:2149-2166, filled from the
// parser failure at :2947 and the completion error at :2954). Its shape is the
// buffered path's own: a gin.H with an "error" string, and a "status" when the
// upstream named one. The name is not a field of api.ChatResponse, so without
// this reading the frame unmarshals to a zero chunk and the arm answers nothing
// — the same body refused when buffered and silently truncated when streamed.
func upstreamErrorFrame(data []byte) (sentence string, status int, ok bool) {
	var frame struct {
		Error  *string `json:"error"`
		Status *int    `json:"status"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return "", 0, false
	}
	if frame.Error == nil {
		return "", 0, false
	}
	status = http.StatusInternalServerError
	if frame.Status != nil && *frame.Status != 0 {
		status = *frame.Status
	}
	return *frame.Error, status, true
}

// failTurn states the failure to the client in the shape the Anthropic wire
// defines for it — the same error event the gateway leg emits for a mid-stream
// error frame, and the same envelope the buffered arm answers with. The events
// already sent cannot be un-sent, so a streaming turn delivers the error on the
// wire rather than pretending it completed.
func (w *AnthropicWriter) failTurn(sentence string, status int) (int, error) {
	if w.failed {
		return 0, nil
	}
	w.failed = true
	if w.stream {
		if err := w.writeEvent("error", anthropic.NewError(status, sentence)); err != nil {
			return 0, err
		}
		return 0, nil
	}
	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	w.ResponseWriter.WriteHeader(status)
	return 0, json.NewEncoder(w.ResponseWriter).Encode(anthropic.NewError(status, sentence))
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
	// The buffered arm used to state whatever model the UPSTREAM reported:
	// ToMessagesResponse is handed a ChatResponse and has no way to know what
	// the client asked for, and the server rewrites a suffixed name
	// (`kimi-k2.5:cloud`) to the bare one before the request leaves. So one
	// /v1/messages body answered with two different `model` strings depending on
	// `stream`, and the buffered one named a model the client had never asked
	// for. The converter already carries the client's own string — it is what
	// the streamed arm's message_start states — so the buffered arm states it
	// too (2026-09-28 audit, round 84, R84-L1-1). The nil check is for writers
	// built by hand (the web_search tests construct one); a request with no
	// model never reaches here — the middleware rejects it as invalid before
	// the upstream is called.
	if w.converter != nil {
		response.Model = w.converter.Model
	}
	response.Usage = withInputEstimate(response.Usage, w.estimatedInputTokens)
	logutil.Trace("anthropic middleware: converted response", "resp", anthropic.TraceMessagesResponse(response))
	return len(data), json.NewEncoder(w.ResponseWriter).Encode(response)
}

func (w *AnthropicWriter) Write(data []byte) (int, error) {
	if w.failed {
		return len(data), nil
	}

	code := w.ResponseWriter.Status()
	if code != http.StatusOK {
		return w.writeError(data)
	}

	if sentence, status, ok := upstreamErrorFrame(data); ok {
		return w.failTurn(sentence, status)
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
	// streamOpenBlockKind is the type of the block the passthrough arm left
	// open, so the terminal can CONTINUE it when the narration it carries is of
	// the same kind rather than close it and open a second one. One merged
	// message is one run per kind; the streamed turn was reaching the client as
	// two blocks wherever the run crossed a chunk boundary the takeover
	// swallowed (2026-09-28 audit, round 81, F81-L1-2).
	streamOpenBlockKind string
	streamNextIndex     int

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
	// lateRuns is the narration of every chunk this arm discarded while the
	// loop was in flight, in the order the chunks arrived and, within a chunk,
	// in the order the converter writes them (reasoning, prose, then the bytes
	// of an entry the upstream never named). The loop's terminal response is
	// built by the worker from the ONE chunk that carried the search call, so
	// the model's later prose would otherwise be lost here while the
	// whole-document arm — which sees the merged message — carries it. Merged
	// into the terminal's leading narration at write time, which is after
	// every such chunk has arrived (the terminal is written on the done
	// chunk) (2026-09-28 audit, round 70, F70-L1-2).
	//
	// An ordered LIST rather than one bucket per kind: the whole-document arm
	// of the same body keeps the order the runs arrived in, and folding this
	// arm's into a fixed [thinking, text] pair put prose-then-reasoning on one
	// wire and reasoning-then-prose on the other (2026-09-28 audit, round 82,
	// F82-L1-2). The nameless entry's bytes are in it for the same reason they
	// are in carriedNarrationBlocks: they are the model's output and no other
	// arm drops them (round 82, F82-L1-4).
	lateRuns []lateNarrationRun

	// turnContent and turnThinking are this turn's own narration — every
	// chunk's prose and reasoning, in the order the chunks arrived — and
	// turnNarrationDone is closed once the turn has ended, so the loop can wait
	// for the last of it.
	//
	// The loop hands the MODEL an account of the turn that asked to search
	// (buildWebSearchAssistantMessage). The whole-document arm is handed the
	// merged message and gives it the whole turn; this arm started the loop
	// from the ONE chunk that carried the search call, so a model that wrote
	// its prose and then searched across two chunks was told it had said only
	// the second stretch — or NOTHING at all when the call stood in a chunk of
	// its own, though the client had already been shown every word. Two clients
	// of one upstream body then got their continuation from two different
	// accounts of the same turn, and the model answered a conversation that
	// contradicted the one the client was reading (2026-09-28 audit, round 83,
	// F83-L1-1). Only the streaming arm is wrong here, so only it waits: the
	// search itself still runs while the turn is being generated.
	turnContent       strings.Builder
	turnThinking      strings.Builder
	turnNarrationDone chan struct{}
	turnNarrationOnce sync.Once
}

// lateNarrationRun is one narration block absorbed while the loop was in
// flight: the kind the converter would open for it, and its text.
type lateNarrationRun struct {
	kind string
	text string
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
	// content is everything the client has already been given for this turn
	// before the failure: every completed iteration's narration and search
	// blocks, then the narration of the turn whose search just failed. The
	// error terminal is a terminal like its two siblings, and the streaming arm
	// has already streamed all of this — a failed search must not reach the two
	// arms as two different answers (2026-09-28 audit, round 71, F71-L1-2).
	content []anthropic.ContentBlock
}

func (e *webSearchLoopError) Error() string {
	if e.err == nil {
		return e.code
	}
	return fmt.Sprintf("%s: %v", e.code, e.err)
}

func (w *WebSearchAnthropicWriter) Write(data []byte) (int, error) {
	// RECORDED DIVERGENCE (2026-09-28 audit, round 85, R85-L1-1), measured and
	// left in place. A search turn whose call chunk is ALSO its last commits the
	// terminal result synchronously (below, in the hasWebSearch+stream branch),
	// so an error frame arriving behind that chunk is swallowed here: the
	// streamed arm hands the client a complete successful answer where the
	// buffered arm of the same upstream body refuses it with the sentence. The
	// other ordering of the same family is handled (round 72's fix, above).
	// Closing it means holding the loop result until the request ends, so a
	// frame that follows the chunk can still be read — and there is no hook for
	// "the request ended" on this writer (the flush after the last chunk is the
	// flush of every chunk), so the hold would have nothing to release it.
	// Nothing on this surface emits a frame after a done chunk today: the local
	// runner writes its final chunk and returns (llm/llama_server.go: `fn(finalResp);
	// return nil`), and cloud models bypass this middleware entirely
	// (server/routes.go: `/v1/messages` carries cloudPassthroughMiddleware).
	// Recorded rather than fixed, like F68-L1-1: a future producer — any runner
	// or relay that errors after a done chunk — trips it silently.
	if w.terminalSent {
		return len(data), nil
	}

	code := w.Status()
	if code != http.StatusOK {
		return w.inner.writeError(data)
	}

	// A mid-stream error frame ends the turn here too. This arm absorbed the
	// frame as narration while the loop was in flight and answered 200 with an
	// empty body — the model's last words discarded and no reason given, while
	// the buffered arm of the same turn refused it with the sentence
	// (2026-09-28 audit, round 72, F72-L1-1).
	if sentence, status, ok := upstreamErrorFrame(data); ok {
		w.terminalSent = true
		w.finishTurnNarration()
		return w.inner.failTurn(sentence, status)
	}

	var chatResponse api.ChatResponse
	if err := json.Unmarshal(data, &chatResponse); err != nil {
		return 0, err
	}
	w.recordObservedUsage(chatResponse.Metrics)

	// Every chunk of a search turn belongs to the account the loop hands the
	// model, not only the chunk that carried the call (F83-L1-1). Accumulated
	// as it arrives — from the turn's FIRST chunk, which is written before
	// anything knows a search is coming — because by the time the loop reads it
	// the turn is over.
	w.turnContent.WriteString(chatResponse.Message.Content)
	w.turnThinking.WriteString(chatResponse.Message.Thinking)

	if w.stream && w.loopInFlight {
		w.absorbLateNarration(chatResponse)
		if !chatResponse.Done {
			return len(data), nil
		}
		// The turn is over and this is the last of its narration: the loop may
		// read the account now (F83-L1-1).
		w.finishTurnNarration()
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
			// A call the model asked for, in a turn that may still turn out to
			// be a search turn. Held, not written: see the field.
			//
			// The hold lasts as long as the doubt does — until a chunk carries
			// a web_search call (the loop takes the turn over), or until the
			// turn ends (the calls are the client's after all). A chunk in
			// between carries no tool call and says nothing either way: a model
			// that calls Bash, writes a line of prose and only then searches
			// produces exactly that, and releasing on the prose handed the
			// client a tool_use the whole-document arm of the same turn drops
			// (2026-09-28 audit, round 71, F71-L1-1). Held chunks are written
			// out in the order they arrived.
			if !chatResponse.Done && (len(chatResponse.Message.ToolCalls) > 0 || len(w.pendingCallChunks) > 0) {
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
			// The chunk that asked to search was the turn's last, so its
			// narration is the turn's whole: the loop may read it now
			// (F83-L1-1).
			w.finishTurnNarration()
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
		return len(data), w.sendError(loopErr.code, loopErr.query, loopErr.usage, loopErr.content)
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
				code:    "invalid_request",
				query:   "",
				usage:   usage,
				content: append(slices.Clone(serverContent), carriedNarrationBlocks(currentResponse)...),
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
				code:    "unavailable",
				query:   query,
				usage:   usage,
				err:     err,
				content: append(slices.Clone(serverContent), carriedNarrationBlocks(currentResponse)...),
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
		// The turn the FIRST iteration answers was streamed to this arm one
		// chunk at a time, and the loop was started by the chunk that carried
		// the search call alone: what the model is told it said has to be the
		// whole turn, the same account the whole-document arm builds from its
		// merged message (F83-L1-1). Every later iteration's response is a
		// whole message off the follow-up call, so it is its own account.
		if loop == 1 && w.turnNarrationDone != nil {
			content, thinking, err := w.waitForTurnNarration(ctx)
			if err != nil {
				logutil.TraceContext(ctx, "anthropic middleware: turn narration never completed",
					"loop", loop,
					"error", err,
				)
				return anthropic.MessagesResponse{}, &webSearchLoopError{
					code:    "api_error",
					query:   query,
					usage:   usage,
					err:     err,
					content: append(slices.Clone(serverContent), carriedNarrationBlocks(currentResponse)...),
				}
			}
			assistantMsg.Content = content
			assistantMsg.Thinking = thinking
		}
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
				code:    "api_error",
				query:   query,
				usage:   usage,
				err:     err,
				content: append(slices.Clone(serverContent), carriedNarrationBlocks(currentResponse)...),
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
	// The turn this loop was started by is still being streamed: the loop reads
	// its narration once this closes (F83-L1-1).
	w.turnNarrationDone = make(chan struct{})
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

// finishTurnNarration marks this turn's narration complete: every chunk of it
// has been seen, so the loop may hand the model its account of the turn
// (F83-L1-1). Called from every path that ends the turn — the done chunk, and
// the error frame that ends it instead. On the arm where no loop is in flight
// it does nothing: there is no one waiting.
func (w *WebSearchAnthropicWriter) finishTurnNarration() {
	if w.turnNarrationDone == nil {
		return
	}
	w.turnNarrationOnce.Do(func() { close(w.turnNarrationDone) })
}

// waitForTurnNarration blocks until this turn has ended, and returns the turn's
// own prose and reasoning as the model wrote them — what the whole-document arm
// of the same body is handed in its merged message. The loop's search is not
// delayed by this: it runs while the turn is still being generated, and only
// the follow-up that needs the account waits for it (F83-L1-1).
//
// ctx is the loop's own: a client that goes away, or an upstream that never
// ends its turn, must not leave the worker waiting on a turn that will never
// finish.
func (w *WebSearchAnthropicWriter) waitForTurnNarration(ctx context.Context) (string, string, error) {
	select {
	case <-w.turnNarrationDone:
		return w.turnContent.String(), w.turnThinking.String(), nil
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

func (w *WebSearchAnthropicWriter) writeLoopResult() error {
	if w.loopResultCh == nil {
		// No worker ever started, so nothing of this turn has been written and
		// nothing is carried: there is no other arm to disagree with.
		return w.sendError("api_error", "", w.currentObservedUsage(), nil)
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
		// The late narration (chunks this arm discarded after the takeover)
		// belongs to the same reading the siblings give: the whole-document
		// arm's merged message carries the model's whole prose, and the
		// streaming arm has already written the part of it that arrived before
		// the search call.
		errResp := w.webSearchErrorResponse(result.loopErr.code, result.loopErr.query, usage, result.loopErr.content)
		w.mergeLateNarration(&errResp)
		return w.writeTerminalResponse(errResp)
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
// The order here is the order the turn's output ARRIVED in, which is not the
// merged message's field order. This arm read the merged message as a fixed
// [thinking, text] pair and the buffered lane of the server hands one over with
// its OutputRuns, the ordered run list it built while merging (server/routes.go,
// writeChatResponse) — so a turn the model wrote as prose-then-reasoning reached
// a client that streamed it as [text, thinking] and a client that did not as
// [thinking, text] (2026-09-28 audit, round 81, F81-L1-1). The runs also say
// where an entry the upstream never named stands: those bytes are the model's
// own output and reach the client as TEXT on every other arm of this leg
// (F77-L1-1), and this one dropped them by reading only Content and Thinking
// (round 81, F81-L1-3).
func carriedNarrationBlocks(response api.ChatResponse) []anthropic.ContentBlock {
	if response.Message.OutputRunsAccountFor() {
		var blocks []anthropic.ContentBlock
		calls := response.Message.ToolCalls
		next := 0
		// addText appends prose, joining it to the text block before it: a
		// run boundary is where the streaming arm opens a block, and a call it
		// writes no block for — here, one the loop superseded — does not break
		// the text around it (round 76, F76-L1-1).
		addText := func(s string) {
			if s == "" {
				return
			}
			if n := len(blocks); n > 0 && blocks[n-1].Type == "text" && blocks[n-1].Text != nil {
				joined := *blocks[n-1].Text + s
				blocks[n-1].Text = &joined
				return
			}
			t := s
			blocks = append(blocks, anthropic.ContentBlock{Type: "text", Text: &t})
		}
		// addThinking joins reasoning the same way, for the same reason: a run
		// boundary is a block boundary only where the KIND changes. A call run
		// this loop superseded writes no block here either — its entry is not a
		// call the turn kept — so reasoning written either side of it is one
		// thinking block on the streaming arm (the converter's `Process` opens
		// one and keeps writing into it, and round 81's F81-L1-1 read the two
		// arms of this bridge as one run per kind) and was two here, because
		// this case appended unconditionally: one upstream body reached a
		// buffered client as `<thinking T1><thinking T2>` and a streaming one as
		// `<thinking T1T2>` (2026-09-28 audit, round 82, F82-L1-1).
		addThinking := func(s string) {
			if s == "" {
				return
			}
			if n := len(blocks); n > 0 && blocks[n-1].Type == "thinking" && blocks[n-1].Thinking != nil {
				joined := *blocks[n-1].Thinking + s
				blocks[n-1].Thinking = &joined
				return
			}
			t := s
			blocks = append(blocks, anthropic.ContentBlock{Type: "thinking", Thinking: &t})
		}
		for _, run := range response.Message.OutputRuns {
			switch run.Kind {
			case "thinking":
				addThinking(run.Text)
			case "text":
				addText(run.Text)
			case "call":
				if next >= len(calls) {
					break
				}
				tc := calls[next]
				next++
				if strings.TrimSpace(tc.Function.Name) == "" && tc.Function.Arguments.Len() > 0 {
					args, err := json.Marshal(tc.Function.Arguments)
					if err != nil {
						continue
					}
					addText(string(args))
				}
			}
		}
		return blocks
	}

	var blocks []anthropic.ContentBlock
	if response.Message.Thinking != "" {
		thinking := response.Message.Thinking
		blocks = append(blocks, anthropic.ContentBlock{Type: "thinking", Thinking: &thinking})
	}
	if response.Message.Content != "" {
		text := response.Message.Content
		blocks = append(blocks, anthropic.ContentBlock{Type: "text", Text: &text})
	}
	// An entry the upstream never NAMED is not a call: its bytes are the model's
	// own output and reach the client as TEXT — the rule the runs path above
	// writes, `releasePendingCallChunks` keeps, and the other two legs apply
	// (round 77, F77-L1-1). A chunk that arrives without runs is the ordinary
	// shape on this arm — the merge lane that builds OutputRuns is the buffered
	// lane's — so the fallback dropped those bytes on every streamed takeover
	// whose chunk carried one, while the buffered arm of the same body relayed
	// them: measured, `[{index-less nameless {"k":"v"}}, {web_search}]` reached a
	// buffered client with the entry's text and a streaming one without it
	// (2026-09-28 audit, round 82, F82-L1-4).
	//
	// Last, because that is where the converter puts a call run: reasoning, then
	// prose, then the calls a chunk carried.
	for _, tc := range response.Message.ToolCalls {
		if strings.TrimSpace(tc.Function.Name) != "" || tc.Function.Arguments.Len() == 0 {
			continue
		}
		args, err := json.Marshal(tc.Function.Arguments)
		if err != nil {
			continue
		}
		s := string(args)
		// Joined to the prose before it when there is one, exactly as the runs
		// path's addText joins it: the model wrote one message, and a text block
		// boundary is where the KIND changes.
		if n := len(blocks); n > 0 && blocks[n-1].Type == "text" && blocks[n-1].Text != nil {
			joined := *blocks[n-1].Text + s
			blocks[n-1].Text = &joined
			continue
		}
		text := s
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
	if chatResponse.Message.Thinking != "" {
		w.lateRuns = append(w.lateRuns, lateNarrationRun{kind: "thinking", text: chatResponse.Message.Thinking})
	}
	if chatResponse.Message.Content != "" {
		w.lateRuns = append(w.lateRuns, lateNarrationRun{kind: "text", text: chatResponse.Message.Content})
	}
	// The chunk's own nameless entries, where the converter puts them: after
	// the reasoning and the prose of the same chunk.
	for _, tc := range chatResponse.Message.ToolCalls {
		if strings.TrimSpace(tc.Function.Name) != "" || tc.Function.Arguments.Len() == 0 {
			continue
		}
		args, err := json.Marshal(tc.Function.Arguments)
		if err != nil {
			continue
		}
		w.lateRuns = append(w.lateRuns, lateNarrationRun{kind: "text", text: string(args)})
	}
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
			// An entry the upstream never NAMED is not a call: its bytes are
			// the model's own output, relayed as text on every other arm of
			// this leg and by the other two legs (round 77, F77-L1-1). The
			// takeover drops the client's calls — the loop supersedes the turn
			// — and it dropped the prose that shared a chunk with them, so a
			// turn whose nameless entry sat before the search reached this
			// client with those bytes gone while the buffered arm of the same
			// body relayed them (2026-09-28 audit, round 81, F81-L1-3).
			var kept []api.ToolCall
			for _, tc := range chunk.Message.ToolCalls {
				if strings.TrimSpace(tc.Function.Name) == "" {
					kept = append(kept, tc)
				}
			}
			chunk.Message.ToolCalls = kept
			// Nothing left of it: a chunk that carried only the calls says
			// nothing once they are gone, and writing it would open a block
			// for an empty message.
			if chunk.Message.Content == "" && chunk.Message.Thinking == "" && len(kept) == 0 {
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
	if len(w.lateRuns) == 0 {
		return
	}
	lead := 0
	for lead < len(response.Content) && (response.Content[lead].Type == "thinking" || response.Content[lead].Type == "text") {
		lead++
	}
	// The terminal's own narration, then the absorbed runs, in that order and
	// each in its own: adjacent runs of one KIND are one block, exactly as the
	// runs path and the streaming converter write them.
	combined := make([]anthropic.ContentBlock, 0, lead+len(w.lateRuns))
	appendRun := func(kind, text string) {
		if text == "" {
			return
		}
		if n := len(combined); n > 0 && combined[n-1].Type == kind {
			if kind == "thinking" && combined[n-1].Thinking != nil {
				joined := *combined[n-1].Thinking + text
				combined[n-1].Thinking = &joined
				return
			}
			if kind == "text" && combined[n-1].Text != nil {
				joined := *combined[n-1].Text + text
				combined[n-1].Text = &joined
				return
			}
		}
		b := anthropic.ContentBlock{Type: kind}
		if kind == "thinking" {
			t := text
			b.Thinking = &t
		} else {
			t := text
			b.Text = &t
		}
		combined = append(combined, b)
	}
	for _, b := range response.Content[:lead] {
		switch b.Type {
		case "thinking":
			if b.Thinking != nil {
				appendRun("thinking", *b.Thinking)
			}
		case "text":
			if b.Text != nil {
				appendRun("text", *b.Text)
			}
		}
	}
	for _, run := range w.lateRuns {
		appendRun(run.kind, run.text)
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
			w.streamOpenBlockKind = e.ContentBlock.Type
			if e.Index+1 > w.streamNextIndex {
				w.streamNextIndex = e.Index + 1
			}
		case anthropic.ContentBlockStopEvent:
			if w.streamHasOpenBlock && w.streamOpenBlockIndex == e.Index {
				w.streamHasOpenBlock = false
				w.streamOpenBlockKind = ""
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

// writeStreamNarration writes the terminal's leading narration — the turn's own
// prose and reasoning, which the passthrough arm may already have begun — and
// CONTINUES an open block of the same kind rather than closing it and opening a
// second one. The buffered arm of the same body carries one run per kind, so a
// run that crossed the chunk boundary the takeover swallowed reached a
// streaming client as two blocks against the document arm's one, and the two
// arms of one upstream turn disagreed about the turn's block structure
// (2026-09-28 audit, round 81, F81-L1-2). A block of another kind is written as
// its own, which is where the streaming converter closes one too.
func (w *WebSearchAnthropicWriter) writeStreamNarration(blocks []anthropic.ContentBlock) error {
	for _, block := range blocks {
		// A block of ANOTHER kind ends the open one where it stands. Left open,
		// the stale index stayed the continuation target: on `[{text "P"}],
		// [{think "T", text "Y", web_search}]` the wire carried `start 0 text`,
		// `delta 0 "P"`, `start 1 thinking`, `delta 1 "T"`, `stop 1`,
		// `delta 0 "Y"`, `stop 0` — an index-keyed client read block 0 as "PY",
		// the model's later prose landed in the block BEFORE it, and the buffered
		// arm answered the same body `<text P><thinking T><text Y>`
		// (2026-09-28 audit, round 82, F82-L1-3). The same stale index framed
		// the turn illegally on `[{text "P"}], [{web_search}], [{think "T"}]`,
		// where `stop 1` went out before `stop 0`.
		if w.streamHasOpenBlock && w.streamOpenBlockKind != block.Type {
			if err := w.closeOpenStreamBlock(); err != nil {
				return err
			}
		}
		if w.streamHasOpenBlock && w.streamOpenBlockKind == block.Type {
			delta := anthropic.Delta{Type: "text_delta"}
			if block.Text != nil {
				delta.Text = *block.Text
			}
			if block.Type == "thinking" {
				delta = anthropic.Delta{Type: "thinking_delta"}
				if block.Thinking != nil {
					delta.Thinking = *block.Thinking
				}
			}
			if err := writeSSE(w.ResponseWriter, "content_block_delta", anthropic.ContentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: w.streamOpenBlockIndex,
				Delta: delta,
			}); err != nil {
				return err
			}
			continue
		}
		if err := w.writeStreamContentBlocks([]anthropic.ContentBlock{block}); err != nil {
			return err
		}
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
	// The turn's narration leads the terminal, and the passthrough arm may have
	// an open block of that kind already: those blocks CONTINUE it (see
	// writeStreamNarration), the rest are written after it is closed.
	lead := 0
	for lead < len(response.Content) && (response.Content[lead].Type == "thinking" || response.Content[lead].Type == "text") {
		lead++
	}
	if err := w.writeStreamNarration(response.Content[:lead]); err != nil {
		return err
	}
	if err := w.closeOpenStreamBlock(); err != nil {
		return err
	}
	if err := w.writeStreamContentBlocks(response.Content[lead:]); err != nil {
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

func (w *WebSearchAnthropicWriter) webSearchErrorResponse(errorCode, query string, usage anthropic.Usage, carried []anthropic.ContentBlock) anthropic.MessagesResponse {
	toolUseID := serverToolUseID(w.inner.id)

	content := append(slices.Clone(carried), anthropic.ContentBlock{
		Type:  "server_tool_use",
		ID:    toolUseID,
		Name:  "web_search",
		Input: queryArgs(query),
	}, anthropic.ContentBlock{
		Type:      "web_search_tool_result",
		ToolUseID: toolUseID,
		Content: anthropic.WebSearchToolResultError{
			Type:      "web_search_tool_result_error",
			ErrorCode: errorCode,
		},
	})

	return anthropic.MessagesResponse{
		ID:         w.inner.id,
		Type:       "message",
		Role:       "assistant",
		Model:      w.req.Model,
		Content:    content,
		StopReason: "end_turn",
		Usage:      usage,
	}
}

// sendError sends a web search error response. carried is what the turn has
// already been given (see webSearchLoopError.content).
func (w *WebSearchAnthropicWriter) sendError(errorCode, query string, usage anthropic.Usage, carried []anthropic.ContentBlock) error {
	response := w.webSearchErrorResponse(errorCode, query, usage, carried)
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

		// Which wire this request came in on. The routes' document arm keeps
		// upstream ollama's rule that a request which declared no tools does not
		// surface the model's tool calls; this surface's streaming arm relays
		// every chunk verbatim and has no such gate, and both other translation
		// legs relay the call. Recording the surface is what lets the two arms
		// of one handler agree (2026-09-28 audit, round 73, F73-L1-1).
		c.Set("anthropic_messages", true)

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

		// An upstream channel that closes without a single chunk is refused
		// inside the streaming arm itself (server/routes.go streamResponse, and
		// the document arm of writeChatResponse): nothing may be written here
		// after c.Next(), because the arm's own flush has already put the 200
		// status on the wire by then (2026-09-28 audit, round 73, F73-L1-2).
	}
}

// hasWebSearchTool checks if the request tools include a web_search tool. The
// test is anthropic.IsWebSearchToolType, which is the one every leg asks
// (2026-09-28 audit, round 83, F83-L1-2).
func hasWebSearchTool(tools []anthropic.Tool) bool {
	for _, tool := range tools {
		if anthropic.IsWebSearchToolType(tool.Type) {
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
