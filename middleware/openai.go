package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/openai"
)

// maxDecompressedBodySize limits the size of a decompressed request body
const maxDecompressedBodySize = 20 << 20

type BaseWriter struct {
	gin.ResponseWriter
}

type ChatWriter struct {
	stream        bool
	streamOptions *openai.StreamOptions
	id            string
	toolCallSent  bool
	// failed is set when an error frame ended this turn. Everything after it is
	// nothing: the client has been told why the turn stopped, and a converter
	// fed the frames that follow would write chunks for a turn that is over.
	// The Anthropic writer has carried this since round 72 (`failed`,
	// middleware/anthropic.go); this surface had no reading of the error frame
	// at all until round 91 (2026-09-29 audit, F91-L1-1).
	failed bool
	BaseWriter
}

type CompleteWriter struct {
	stream        bool
	streamOptions *openai.StreamOptions
	id            string
	// failed — see ChatWriter.failed (2026-09-29 audit, F91-L1-1).
	failed bool
	BaseWriter
}

type ListWriter struct {
	BaseWriter
}

type RetrieveWriter struct {
	BaseWriter
	model string
}

type EmbedWriter struct {
	BaseWriter
	model          string
	encodingFormat string
}

func (w *BaseWriter) writeError(data []byte) (int, error) {
	var serr api.StatusError
	if err := json.Unmarshal(data, &serr); err != nil {
		// If the error response isn't valid JSON, use the raw bytes as the
		// error message rather than surfacing a confusing JSON parse error.
		serr.ErrorMessage = string(data)
	}

	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w.ResponseWriter).Encode(openai.NewError(w.ResponseWriter.Status(), serr.Error())); err != nil {
		return 0, err
	}

	return len(data), nil
}

func (w *ChatWriter) writeResponse(data []byte) (int, error) {
	var chatResponse api.ChatResponse
	err := json.Unmarshal(data, &chatResponse)
	if err != nil {
		return 0, err
	}

	// chat chunk
	if w.stream {
		chunks := openai.ToChunks(w.id, chatResponse, w.toolCallSent)
		w.ResponseWriter.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			d, err := json.Marshal(c)
			if err != nil {
				return 0, err
			}
			if !w.toolCallSent && len(c.Choices) > 0 && len(c.Choices[0].Delta.ToolCalls) > 0 {
				w.toolCallSent = true
			}
			_, err = w.ResponseWriter.Write([]byte(fmt.Sprintf("data: %s\n\n", d)))
			if err != nil {
				return 0, err
			}
		}

		if chatResponse.Done {
			c := openai.ToChunk(w.id, chatResponse, w.toolCallSent)
			if len(chunks) > 0 {
				c = chunks[len(chunks)-1]
			} else {
				slog.Warn("ToChunks returned no chunks; falling back to ToChunk for usage chunk", "id", w.id, "model", chatResponse.Model)
			}
			if w.streamOptions != nil && w.streamOptions.IncludeUsage {
				u := openai.ToUsage(chatResponse)
				c.Usage = &u
				// The OpenAI clients that read timings want them beside the usage
				// trailer; safeDiv guards the divisions a zero-duration run would make
				// NaN (upstream 16b4376ae).
				c.Timings = openai.ToTimings(chatResponse.Metrics)
				c.Choices = []openai.ChunkChoice{}
				d, err := json.Marshal(c)
				if err != nil {
					return 0, err
				}
				_, err = w.ResponseWriter.Write([]byte(fmt.Sprintf("data: %s\n\n", d)))
				if err != nil {
					return 0, err
				}
			}
			_, err = w.ResponseWriter.Write([]byte("data: [DONE]\n\n"))
			if err != nil {
				return 0, err
			}
		}

		return len(data), nil
	}

	// chat completion
	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w.ResponseWriter).Encode(openai.ToChatCompletion(w.id, chatResponse))
	if err != nil {
		return 0, err
	}

	return len(data), nil
}

func (w *ChatWriter) Write(data []byte) (int, error) {
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

// failOpenAITurn states a mid-stream failure to the client in this wire's own
// shape: one `data:` chunk carrying the very envelope the buffered arm answers
// with, and NO `[DONE]` — the sentinel says the turn finished, and this turn did
// not. The buffered arm is unchanged and needs no help here: an unwritten writer
// is answered by the chat lane's own refusal (server/routes.go, `c.JSON(status,
// gin.H{"error": …})`), which reaches this writer with a non-200 status and goes
// through `writeError` — measured 500 `{"error":{"message":"…","type":
// "api_error"}}` on both `/v1/chat/completions` and `/v1/completions`. Without
// this function the STREAMED arm of those two surfaces dropped the runner's
// failure entirely: the frame the chat path writes into a 200 stream when the
// generation dies partway (`server/routes.go:2149-2166`) has no `error` field of
// its own as far as `api.ChatResponse` is concerned, so it unmarshalled to a zero
// chunk and was relayed as an empty delta followed by nothing — 200, no sentence,
// no `[DONE]`. The Anthropic writer has read that frame since round 86
// (`upstreamErrorFrame` → `failTurn`), so one upstream body was answered with the
// runner's own sentence on `/v1/messages` and with silence on
// `/v1/chat/completions` (2026-09-29 audit, round 91, F91-L1-1).
func failOpenAITurn(rw gin.ResponseWriter, stream bool, failed *bool, sentence string, status int) (int, error) {
	if *failed {
		return 0, nil
	}
	*failed = true
	if stream {
		rw.Header().Set("Content-Type", "text/event-stream")
		d, err := json.Marshal(openai.NewError(status, sentence))
		if err != nil {
			return 0, err
		}
		if _, err := rw.Write([]byte(fmt.Sprintf("data: %s\n\n", d))); err != nil {
			return 0, err
		}
		return 0, nil
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	return 0, json.NewEncoder(rw).Encode(openai.NewError(status, sentence))
}

func (w *ChatWriter) failTurn(sentence string, status int) (int, error) {
	return failOpenAITurn(w.ResponseWriter, w.stream, &w.failed, sentence, status)
}

func (w *CompleteWriter) writeResponse(data []byte) (int, error) {
	var generateResponse api.GenerateResponse
	err := json.Unmarshal(data, &generateResponse)
	if err != nil {
		return 0, err
	}

	// completion chunk
	if w.stream {
		c := openai.ToCompleteChunk(w.id, generateResponse)
		// No usage on an intermediate chunk. This used to attach a zeroed
		// Usage object to EVERY chunk whenever include_usage was set, and a
		// client (or a metering intermediary) that reads the first non-null
		// usage records 0/0 for a turn that really used 7/3 — the chat path in
		// this file attaches usage only to the final, empty-choices chunk, and
		// this now agrees with it (2026-09-26 audit, fourth round).
		d, err := json.Marshal(c)
		if err != nil {
			return 0, err
		}

		w.ResponseWriter.Header().Set("Content-Type", "text/event-stream")
		_, err = w.ResponseWriter.Write([]byte(fmt.Sprintf("data: %s\n\n", d)))
		if err != nil {
			return 0, err
		}

		if generateResponse.Done {
			if w.streamOptions != nil && w.streamOptions.IncludeUsage {
				u := openai.ToUsageGenerate(generateResponse)
				c.Usage = &u
				c.Timings = openai.ToTimings(generateResponse.Metrics)
				c.Choices = []openai.CompleteChunkChoice{}
				d, err := json.Marshal(c)
				if err != nil {
					return 0, err
				}
				_, err = w.ResponseWriter.Write([]byte(fmt.Sprintf("data: %s\n\n", d)))
				if err != nil {
					return 0, err
				}
			}
			_, err = w.ResponseWriter.Write([]byte("data: [DONE]\n\n"))
			if err != nil {
				return 0, err
			}
		}

		return len(data), nil
	}

	// completion
	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w.ResponseWriter).Encode(openai.ToCompletion(w.id, generateResponse))
	if err != nil {
		return 0, err
	}

	return len(data), nil
}

func (w *CompleteWriter) Write(data []byte) (int, error) {
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

func (w *CompleteWriter) failTurn(sentence string, status int) (int, error) {
	return failOpenAITurn(w.ResponseWriter, w.stream, &w.failed, sentence, status)
}

func (w *ListWriter) writeResponse(data []byte) (int, error) {
	var listResponse api.ListResponse
	err := json.Unmarshal(data, &listResponse)
	if err != nil {
		return 0, err
	}

	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w.ResponseWriter).Encode(openai.ToListCompletion(listResponse))
	if err != nil {
		return 0, err
	}

	return len(data), nil
}

func (w *ListWriter) Write(data []byte) (int, error) {
	code := w.ResponseWriter.Status()
	if code != http.StatusOK {
		return w.writeError(data)
	}

	return w.writeResponse(data)
}

func (w *RetrieveWriter) writeResponse(data []byte) (int, error) {
	var showResponse api.ShowResponse
	err := json.Unmarshal(data, &showResponse)
	if err != nil {
		return 0, err
	}

	// retrieve completion
	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w.ResponseWriter).Encode(openai.ToModel(showResponse, w.model))
	if err != nil {
		return 0, err
	}

	return len(data), nil
}

func (w *RetrieveWriter) Write(data []byte) (int, error) {
	code := w.ResponseWriter.Status()
	if code != http.StatusOK {
		return w.writeError(data)
	}

	return w.writeResponse(data)
}

func (w *EmbedWriter) writeResponse(data []byte) (int, error) {
	var embedResponse api.EmbedResponse
	err := json.Unmarshal(data, &embedResponse)
	if err != nil {
		return 0, err
	}

	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w.ResponseWriter).Encode(openai.ToEmbeddingList(w.model, embedResponse, w.encodingFormat))
	if err != nil {
		return 0, err
	}

	return len(data), nil
}

func (w *EmbedWriter) Write(data []byte) (int, error) {
	code := w.ResponseWriter.Status()
	if code != http.StatusOK {
		return w.writeError(data)
	}

	return w.writeResponse(data)
}

func ListMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		w := &ListWriter{
			BaseWriter: BaseWriter{ResponseWriter: c.Writer},
		}

		c.Writer = w

		c.Next()
	}
}

func RetrieveMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		var b bytes.Buffer
		if err := json.NewEncoder(&b).Encode(api.ShowRequest{Name: c.Param("model")}); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, openai.NewError(http.StatusInternalServerError, err.Error()))
			return
		}

		c.Request.Body = io.NopCloser(&b)

		w := &RetrieveWriter{
			BaseWriter: BaseWriter{ResponseWriter: c.Writer},
			model:      c.Param("model"),
		}

		c.Writer = w

		c.Next()
	}
}

func CompletionsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req openai.CompletionRequest
		err := c.ShouldBindJSON(&req)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, err.Error()))
			return
		}

		var b bytes.Buffer
		genReq, err := openai.FromCompleteRequest(req)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, err.Error()))
			return
		}

		if err := json.NewEncoder(&b).Encode(genReq); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, openai.NewError(http.StatusInternalServerError, err.Error()))
			return
		}

		c.Request.Body = io.NopCloser(&b)

		w := &CompleteWriter{
			BaseWriter:    BaseWriter{ResponseWriter: c.Writer},
			stream:        req.Stream,
			id:            fmt.Sprintf("cmpl-%d", rand.Intn(999)),
			streamOptions: req.StreamOptions,
		}

		c.Writer = w
		c.Next()
	}
}

func EmbeddingsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req openai.EmbedRequest
		err := c.ShouldBindJSON(&req)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, err.Error()))
			return
		}

		// Validate encoding_format parameter
		if req.EncodingFormat != "" {
			if !strings.EqualFold(req.EncodingFormat, "float") && !strings.EqualFold(req.EncodingFormat, "base64") {
				c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, fmt.Sprintf("Invalid value for 'encoding_format' = %s. Supported values: ['float', 'base64'].", req.EncodingFormat)))
				return
			}
		}

		if req.Input == "" {
			req.Input = []string{""}
		}

		if req.Input == nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, "invalid input"))
			return
		}

		if v, ok := req.Input.([]any); ok && len(v) == 0 {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, "invalid input"))
			return
		}

		var b bytes.Buffer
		if err := json.NewEncoder(&b).Encode(api.EmbedRequest{Model: req.Model, Input: req.Input, Dimensions: req.Dimensions}); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, openai.NewError(http.StatusInternalServerError, err.Error()))
			return
		}

		c.Request.Body = io.NopCloser(&b)

		w := &EmbedWriter{
			BaseWriter:     BaseWriter{ResponseWriter: c.Writer},
			model:          req.Model,
			encodingFormat: req.EncodingFormat,
		}

		c.Writer = w

		c.Next()
	}
}

func ChatMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req openai.ChatCompletionRequest
		err := c.ShouldBindJSON(&req)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, err.Error()))
			return
		}

		if len(req.Messages) == 0 {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, "[] is too short - 'messages'"))
			return
		}

		var b bytes.Buffer

		chatReq, err := openai.FromChatRequest(req)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, err.Error()))
			return
		}

		if err := json.NewEncoder(&b).Encode(chatReq); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, openai.NewError(http.StatusInternalServerError, err.Error()))
			return
		}

		c.Request.Body = io.NopCloser(&b)

		w := &ChatWriter{
			BaseWriter:    BaseWriter{ResponseWriter: c.Writer},
			stream:        req.Stream,
			id:            fmt.Sprintf("chatcmpl-%d", rand.Intn(999)),
			streamOptions: req.StreamOptions,
		}

		c.Writer = w

		// The surface this request arrived on, for the one rule the native
		// /api/chat keeps and a translated wire may not: a request that declared
		// no tools does not surface the model's tool calls. The streaming arm of
		// this very surface relays them regardless, so applying the gate here
		// answered one client body two ways — the call missing when the client
		// asked for no stream and present when it streamed, with the prose that
		// surrounded it kept either way (2026-09-29 audit, round 92, F92-L1-2).
		c.Set(TranslatedSurfaceKey, true)

		c.Next()
	}
}

type ResponsesWriter struct {
	BaseWriter
	converter  *openai.ResponsesStreamConverter
	model      string
	stream     bool
	responseID string
	itemID     string
	request    openai.ResponsesRequest
	// failed is set when an error frame ended this turn — see ChatWriter.failed.
	// This surface had no reading of the error frame at all until round 92: a
	// turn that died mid-stream was relayed as its own partial text and then
	// nothing, 200 and no `response.failed`, so a client that asked for a
	// completion event never got one and read the truncated answer as the whole
	// one (2026-09-29 audit, F92-L1-1).
	failed bool
}

func (w *ResponsesWriter) writeEvent(eventType string, data any) error {
	d, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = w.ResponseWriter.Write([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, d)))
	if err != nil {
		return err
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (w *ResponsesWriter) writeResponse(data []byte) (int, error) {
	var chatResponse api.ChatResponse
	if err := json.Unmarshal(data, &chatResponse); err != nil {
		return 0, err
	}

	if w.stream {
		w.ResponseWriter.Header().Set("Content-Type", "text/event-stream")

		events := w.converter.Process(chatResponse)
		for _, event := range events {
			if err := w.writeEvent(event.Event, event.Data); err != nil {
				return 0, err
			}
		}
		return len(data), nil
	}

	// Non-streaming response
	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	response := openai.ToResponse(w.model, w.responseID, w.itemID, chatResponse, w.request)
	completedAt := time.Now().Unix()
	response.CompletedAt = &completedAt
	return len(data), json.NewEncoder(w.ResponseWriter).Encode(response)
}

// failTurn states the failure to the client in the shape this wire defines for
// it: the terminal `response.failed` event on the streaming arm, and the wire's
// own error envelope, under the producer's status, on the buffered one — the
// same split the OpenAI and Anthropic writers make (failOpenAITurn,
// AnthropicWriter.failTurn).
func (w *ResponsesWriter) failTurn(sentence string, status int) (int, error) {
	if w.failed {
		return 0, nil
	}
	w.failed = true
	if w.stream {
		w.ResponseWriter.Header().Set("Content-Type", "text/event-stream")
		for _, event := range w.converter.Failure(sentence, status) {
			if err := w.writeEvent(event.Event, event.Data); err != nil {
				return 0, err
			}
		}
		return 0, nil
	}
	w.ResponseWriter.Header().Set("Content-Type", "application/json")
	w.ResponseWriter.WriteHeader(status)
	return 0, json.NewEncoder(w.ResponseWriter).Encode(openai.NewError(status, sentence))
}

func (w *ResponsesWriter) Write(data []byte) (int, error) {
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

func ResponsesMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Content-Encoding") == "zstd" {
			reader, err := zstd.NewReader(c.Request.Body, zstd.WithDecoderMaxMemory(8<<20))
			if err != nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, "failed to decompress zstd body"))
				return
			}
			defer reader.Close()
			c.Request.Body = http.MaxBytesReader(c.Writer, io.NopCloser(reader), maxDecompressedBodySize)
			c.Request.Header.Del("Content-Encoding")
		}

		var req openai.ResponsesRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, err.Error()))
			return
		}

		chatReq, err := openai.FromResponsesRequest(req)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, err.Error()))
			return
		}

		// Check if client requested streaming (defaults to false)
		streamRequested := req.Stream != nil && *req.Stream

		// Pass streaming preference to the underlying chat request
		chatReq.Stream = &streamRequested

		var b bytes.Buffer
		if err := json.NewEncoder(&b).Encode(chatReq); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, openai.NewError(http.StatusInternalServerError, err.Error()))
			return
		}

		c.Request.Body = io.NopCloser(&b)

		responseID := fmt.Sprintf("resp_%d", rand.Intn(999999))
		itemID := fmt.Sprintf("msg_%d", rand.Intn(999999))

		w := &ResponsesWriter{
			BaseWriter: BaseWriter{ResponseWriter: c.Writer},
			converter:  openai.NewResponsesStreamConverter(responseID, itemID, req.Model, req),
			model:      req.Model,
			stream:     streamRequested,
			responseID: responseID,
			itemID:     itemID,
			request:    req,
		}

		// Set headers based on streaming mode
		if streamRequested {
			c.Writer.Header().Set("Content-Type", "text/event-stream")
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.Writer.Header().Set("Connection", "keep-alive")
		}

		c.Writer = w

		// The surface this request arrived on — see ChatMiddleware. This wire's
		// buffered arm dropped the model's tool call when the client declared
		// none, while its streaming arm relayed it (2026-09-29 audit, round 92,
		// F92-L1-2).
		c.Set(TranslatedSurfaceKey, true)

		c.Next()
	}
}

// TranslatedSurfaceKey is the context key a translation middleware sets so the
// handlers know the request did not arrive on the native Ollama wire. The gate
// that hides a model's tool calls from a request that declared none belongs to
// that wire alone: every translated surface relays the call on its streaming arm
// whatever the client declared, so gating the buffered arm of the same surface
// makes one client body answer two ways (2026-09-28 audit, round 73, F73-L1-1;
// 2026-09-29 audit, round 92, F92-L1-2).
const TranslatedSurfaceKey = "translated_surface"

// TranscriptionWriter collects streamed chat responses and outputs a transcription response.
type TranscriptionWriter struct {
	BaseWriter
	responseFormat string
	text           strings.Builder
}

func (w *TranscriptionWriter) Write(data []byte) (int, error) {
	code := w.ResponseWriter.Status()
	if code != http.StatusOK {
		return w.writeError(data)
	}

	var chatResponse api.ChatResponse
	if err := json.Unmarshal(data, &chatResponse); err != nil {
		return 0, err
	}

	w.text.WriteString(chatResponse.Message.Content)

	if chatResponse.Done {
		text := strings.TrimSpace(w.text.String())

		if w.responseFormat == "text" {
			w.ResponseWriter.Header().Set("Content-Type", "text/plain")
			_, err := w.ResponseWriter.Write([]byte(text))
			if err != nil {
				return 0, err
			}
			return len(data), nil
		}

		w.ResponseWriter.Header().Set("Content-Type", "application/json")
		resp := openai.TranscriptionResponse{Text: text}
		if err := json.NewEncoder(w.ResponseWriter).Encode(resp); err != nil {
			return 0, err
		}
	}

	return len(data), nil
}

// TranscriptionMiddleware handles /v1/audio/transcriptions requests.
// It accepts multipart/form-data with an audio file and converts it to a chat request.
func TranscriptionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Parse multipart form (limit 25MB).
		if err := c.Request.ParseMultipartForm(25 << 20); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, "failed to parse multipart form: "+err.Error()))
			return
		}

		model := c.Request.FormValue("model")
		if model == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, "model is required"))
			return
		}

		file, _, err := c.Request.FormFile("file")
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, "file is required: "+err.Error()))
			return
		}
		defer file.Close()

		audioData, err := io.ReadAll(file)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, openai.NewError(http.StatusInternalServerError, "failed to read audio file"))
			return
		}

		if len(audioData) == 0 {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, "audio file is empty"))
			return
		}

		req := openai.TranscriptionRequest{
			Model:          model,
			AudioData:      audioData,
			ResponseFormat: c.Request.FormValue("response_format"),
			Language:       c.Request.FormValue("language"),
			Prompt:         c.Request.FormValue("prompt"),
		}

		chatReq, err := openai.FromTranscriptionRequest(req)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, openai.NewError(http.StatusBadRequest, err.Error()))
			return
		}

		var b bytes.Buffer
		if err := json.NewEncoder(&b).Encode(chatReq); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, openai.NewError(http.StatusInternalServerError, err.Error()))
			return
		}

		c.Request.Body = io.NopCloser(&b)
		c.Request.ContentLength = int64(b.Len())
		c.Request.Header.Set("Content-Type", "application/json")

		w := &TranscriptionWriter{
			BaseWriter:     BaseWriter{ResponseWriter: c.Writer},
			responseFormat: req.ResponseFormat,
		}

		c.Writer = w
		c.Next()
	}
}
