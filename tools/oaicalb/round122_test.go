package main

// Round 122 leg 3 (2026-09-29 audit): the backstop meter books what the backend served, from the
// keys the backend reads, bounded, and never truncates a body it inspects.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRound122MeterReadsStreamByTheBackendsExactKey(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
	}))
	defer sse.Close()
	h := serveWith(newStaticPool([]*backend{newBackend(sse.URL)}), func(bs []*backend, _ int) *backend { return bs[0] })
	// The backend reads only "stream"; a differently-cased key is ignored by it, so it streams.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"Stream":false}`))
	h(httptest.NewRecorder(), req)
	waitForRecords(t, records, 1)
	if rec := (*records)[0]; !rec.Stream || rec.PromptTokens != 11 || rec.CompletionTokens != 5 {
		t.Errorf("a served streamed turn was booked %+v, want stream 11/5", rec)
	}
}

func TestRound122MeterBoundsClientTextInTheRow(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })
	srv := vLLMWithUsage(t)
	h := serveWith(newStaticPool([]*backend{newBackend(srv.URL)}), func(bs []*backend, _ int) *backend { return bs[0] })
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+strings.Repeat("m", 1<<20)+`"}`))
	req.Header.Set("X-Session-Id", strings.Repeat("s", 1<<20))
	h(httptest.NewRecorder(), req)
	waitForRecords(t, records, 1)
	if rec := (*records)[0]; len(rec.Model) > 300 || len(rec.SessionID) > 200 {
		t.Errorf("the row carries a %d-byte model and a %d-byte session id", len(rec.Model), len(rec.SessionID))
	}
}

func TestRound122OversizeBodyIsForwardedWholeAndUnmetered(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })
	var got atomic.Int64
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got.Store(n)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer be.Close()
	h := serveWith(newStaticPool([]*backend{newBackend(be.URL)}), func(bs []*backend, _ int) *backend { return bs[0] })
	body := `{"model":"m","pad":"` + strings.Repeat("x", meterBodyLimit+1024) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.ContentLength = int64(len(body))
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusOK || got.Load() != int64(len(body)) {
		t.Errorf("status %d, backend received %d of %d bytes: an oversize body was cut", w.Code, got.Load(), len(body))
	}
	_ = records
}
