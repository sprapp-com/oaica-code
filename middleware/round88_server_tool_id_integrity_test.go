package middleware

// round88_server_tool_id_integrity_test.go — leg 1, F88-L1-1 (2026-09-29 audit,
// round 88).
//
// One server tool id names ONE call. Every search the loop runs appends its own
// pair to the turn's carried content under loopServerToolUseID(id, loop), so by
// the time the loop fails — a search that will not run, a follow-up that will not
// answer — the turn already holds pairs at slots 1..n. webSearchErrorResponse
// states the failure as one more pair, and it named that pair
// loopServerToolUseID(id, 1): the base id, which is slot 1, which the turn had
// already written whenever the failure came at loop 2 or later. The client then
// met the same id twice — a call reported as answered once and asked again, or on
// the follow-up terminals the second pair reporting a query the first one's result
// block had already answered. The max-loop terminal already took the next free
// slot (maxWebSearchLoops+1); the fix is that same rule, read off what carried
// actually holds — the count of server_tool_use blocks already there.
//
// Measured 2026-09-29 before the fix, both arms, one id naming four blocks:
// "one search, follow-up 500" -> use/res/use/res under one id; "two searches,
// second follow-up 500" -> the error pair reports {"query":"second query"} under
// the id the first search already owned; "two searches, second search fails" ->
// the query-less and search-failure terminals collide too, at loop >= 2.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
)

// r88Turn drives one turn whose follow-up answers are a SEQUENCE, so the loop can
// be made to iterate more than once, and whose search and follow-up servers can
// each be made to fail at a chosen call. It reports the raw server tool ids in
// the order the client met them: "use:<id> <queryArgs>" and "res:<id>".
func r88Turn(t *testing.T, stream bool, chunks []api.ChatResponse, followups []api.ChatResponse, failSearchAt, failFollowupAt int32) (int, []string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	enableCloudForTest(t)

	var n int32
	followup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&n, 1))
		if failFollowupAt > 0 && int32(i) == failFollowupAt {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"follow-up down"}`))
			return
		}
		resp := followups[len(followups)-1]
		if i-1 < len(followups) {
			resp = followups[i-1]
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer followup.Close()
	t.Setenv("OLLAMA_HOST", followup.URL)

	var calls int32
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := atomic.AddInt32(&calls, 1); failSearchAt > 0 && c == failSearchAt {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"search down"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(anthropic.OllamaWebSearchResponse{
			Results: []anthropic.OllamaWebSearchResult{{Title: "R", URL: "https://e.com", Content: "c"}},
		})
	}))
	defer search.Close()
	orig := anthropic.WebSearchEndpoint
	anthropic.WebSearchEndpoint = search.URL
	defer func() { anthropic.WebSearchEndpoint = orig }()

	router := gin.New()
	router.Use(AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		c.Writer.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = c.Writer.Write(data)
		}
	})

	streamLit := "false"
	if stream {
		streamLit = "true"
	}
	body := `{"model":"test-model:cloud","max_tokens":100,"stream":` + streamLit + `,` +
		`"messages":[{"role":"user","content":"What is the latest news?"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec.Code, r88Pairs(t, rec.Body.String(), stream)
}

// r88Pairs reads the turn as the ordered list of server tool references the
// client met, raw ids and all.
func r88Pairs(t *testing.T, body string, stream bool) []string {
	t.Helper()
	var out []string
	add := func(b anthropic.ContentBlock) {
		switch b.Type {
		case "server_tool_use":
			q := ""
			if raw, err := json.Marshal(b.Input); err == nil {
				q = " " + string(raw)
			}
			out = append(out, "use:"+b.ID+q)
		case "web_search_tool_result":
			out = append(out, "res:"+b.ToolUseID)
		}
	}
	if stream {
		for _, ev := range parseSSEEvents(t, body) {
			if ev.event != "content_block_start" {
				continue
			}
			var e anthropic.ContentBlockStartEvent
			if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
				t.Fatalf("block start: %v", err)
			}
			add(e.ContentBlock)
		}
		return out
	}
	// A document arm may write more than one JSON document on one body.
	dec := json.NewDecoder(strings.NewReader(body))
	for {
		var doc anthropic.MessagesResponse
		if err := dec.Decode(&doc); err != nil {
			break
		}
		for _, b := range doc.Content {
			add(b)
		}
	}
	return out
}

// r88Slot reads the pairs as "use#k"/"res#k", k the slot the raw id first
// appeared in — the arms never agree on the id strings (they are minted from the
// message id), and they must agree on the slots.
func r88Slot(pairs []string) []string {
	slot := map[string]string{}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		kind, rest, _ := strings.Cut(p, ":")
		id, _, _ := strings.Cut(rest, " ")
		k, ok := slot[id]
		if !ok {
			k = "id#" + strconv.Itoa(len(slot))
			slot[id] = k
		}
		out = append(out, kind+":"+k)
	}
	return out
}

// TestOneServerToolIdNamesOneCall is the F88-L1-1 pin: on every terminal the loop
// can fail on, each id the client is handed names exactly one call and exactly
// one result, and the two arms read the turn the same way.
func TestOneServerToolIdNamesOneCall(t *testing.T) {
	second := api.ChatResponse{
		Model: "test-model",
		Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{
			{ID: "call_ws_2", Function: api.ToolCallFunction{Name: "web_search", Arguments: makeArgs("query", "second query")}},
		}},
		Done: true, DoneReason: "stop", Metrics: api.Metrics{PromptEvalCount: 20, EvalCount: 4},
	}
	final := api.ChatResponse{
		Model: "test-model", Message: api.Message{Role: "assistant", Content: "FINAL"},
		Done: true, DoneReason: "stop", Metrics: api.Metrics{PromptEvalCount: 30, EvalCount: 5},
	}
	chunks := []api.ChatResponse{
		{Model: "test-model", Message: api.Message{Role: "assistant", Content: "look. "}},
		{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Search()}}},
		r70Stop(),
	}

	for _, tc := range []struct {
		name      string
		followups []api.ChatResponse
		failAt    int32
		failFU    int32
	}{
		{name: "one search, follow-up 500", followups: []api.ChatResponse{final}, failFU: 1},
		{name: "two searches, second follow-up 500", followups: []api.ChatResponse{second, final}, failFU: 2},
		{name: "two searches, second search fails", followups: []api.ChatResponse{second, final}, failAt: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var arms [2][]string
			for i, stream := range []bool{false, true} {
				code, pairs := r88Turn(t, stream, chunks, tc.followups, tc.failAt, tc.failFU)
				if code != http.StatusOK {
					t.Fatalf("premise: a failed search/follow-up is a 200 turn, got %d (stream=%v)", code, stream)
				}
				uses, res := map[string]int{}, map[string]int{}
				for _, p := range pairs {
					if rest, ok := strings.CutPrefix(p, "use:"); ok {
						uses[strings.SplitN(rest, " ", 2)[0]]++
					} else if rest, ok := strings.CutPrefix(p, "res:"); ok {
						res[rest]++
					}
				}
				for id, n := range uses {
					if n != 1 {
						t.Errorf("stream=%v: id %s is written by %d server_tool_use blocks: %v — the failure is one more call, and a call the turn already made is not it (2026-09-29 audit, round 88, F88-L1-1)",
							stream, id, n, pairs)
					}
				}
				for id, n := range res {
					if n != 1 {
						t.Errorf("stream=%v: id %s is named by %d web_search_tool_result blocks: %v (2026-09-29 audit, round 88, F88-L1-1)", stream, id, n, pairs)
					}
					if uses[id] != 1 {
						t.Errorf("stream=%v: id %s is named by a result block with no call of its own: %v (2026-09-29 audit, round 88, F88-L1-1)", stream, id, pairs)
					}
				}
				arms[i] = r88Slot(pairs)
			}
			if strings.Join(arms[0], ",") != strings.Join(arms[1], ",") {
				t.Errorf("one failed turn reaches the two arms as different calls:\n document=%v\n streamed=%v", arms[0], arms[1])
			}
		})
	}
}
