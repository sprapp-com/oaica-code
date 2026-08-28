package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func testHub(t *testing.T) (*meterHub, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "meter.db")
	tokenSum := sha256.Sum256([]byte("test-token"))
	hub, err := newMeterHub(meterConfig{
		DBPath: dbPath,
		ReportTokens: []reportToken{
			{SHA256: hex.EncodeToString(tokenSum[:]), Label: "a100b"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.db.Close() })
	return hub, "test-token"
}

func postIngest(t *testing.T, hub *meterHub, token string, rec usageRecord) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	hub.ingestHandler(w, req)
	return w
}

func TestIngest_RequiresAuth(t *testing.T) {
	hub, _ := testHub(t)
	req := httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	hub.ingestHandler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no auth header: status = %d, want 401", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/ingest", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer wrong-token")
	w = httptest.NewRecorder()
	hub.ingestHandler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", w.Code)
	}
}

func TestIngest_RequiresRequestID(t *testing.T) {
	hub, token := testHub(t)
	w := postIngest(t, hub, token, usageRecord{Model: "kat-awq"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing request_id: status = %d, want 400", w.Code)
	}
}

func TestIngest_StoresRecord(t *testing.T) {
	hub, token := testHub(t)
	rec := usageRecord{
		RequestID: "req_abc123", TS: "2026-08-29T00:00:00Z", Region: "a100b",
		KeyLabel: "internal-91", Model: "oaica-35b-a3b-vision", UpstreamModel: "oaica-35b-a3b-vision",
		Path: "/v1/chat/completions", Status: 200, PromptTokens: 100, CompletionTokens: 50,
		LatencyMS: 1200, UsageSeen: true,
	}
	w := postIngest(t, hub, token, rec)
	if w.Code != http.StatusNoContent {
		t.Fatalf("ingest status = %d, want 204", w.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/usage?key=internal-91", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w2 := httptest.NewRecorder()
	hub.usageHandler(w2, req)
	if w2.Code != http.StatusOK {
		t.Fatalf("usage query status = %d, want 200", w2.Code)
	}
	var resp struct {
		Records []usageRow `json:"records"`
		Count   int        `json:"count"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 {
		t.Fatalf("count = %d, want 1", resp.Count)
	}
	if resp.Records[0].PromptTokens != 100 || resp.Records[0].CompletionTokens != 50 {
		t.Errorf("stored record = %+v, want prompt=100 completion=50", resp.Records[0])
	}
}

func TestIngest_IdempotentOnDuplicateRequestID(t *testing.T) {
	hub, token := testHub(t)
	rec := usageRecord{RequestID: "req_dup", TS: "2026-08-29T00:00:00Z", Region: "a100b", KeyLabel: "k", Model: "m", PromptTokens: 10, CompletionTokens: 5}

	// Same request_id reported twice (simulating a gateway's retry-after-
	// timeout) must not double-count.
	postIngest(t, hub, token, rec)
	postIngest(t, hub, token, rec)

	req := httptest.NewRequest(http.MethodGet, "/usage?key=k", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	hub.usageHandler(w, req)
	var resp struct {
		Count int `json:"count"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Count != 1 {
		t.Errorf("duplicate request_id: count = %d, want 1 (idempotent insert)", resp.Count)
	}
}

func TestUsageHandler_FiltersByKeyModelRegion(t *testing.T) {
	hub, token := testHub(t)
	postIngest(t, hub, token, usageRecord{RequestID: "r1", TS: "t1", Region: "a100b", KeyLabel: "alice", Model: "m1", PromptTokens: 1, CompletionTokens: 1})
	postIngest(t, hub, token, usageRecord{RequestID: "r2", TS: "t2", Region: "a100b", KeyLabel: "bob", Model: "m1", PromptTokens: 1, CompletionTokens: 1})
	postIngest(t, hub, token, usageRecord{RequestID: "r3", TS: "t3", Region: "gcp-us", KeyLabel: "alice", Model: "m2", PromptTokens: 1, CompletionTokens: 1})

	query := func(qs string) int {
		req := httptest.NewRequest(http.MethodGet, "/usage?"+qs, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		hub.usageHandler(w, req)
		var resp struct {
			Count int `json:"count"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Count
	}
	if n := query("key=alice"); n != 2 {
		t.Errorf("key=alice: count = %d, want 2", n)
	}
	if n := query("model=m1"); n != 2 {
		t.Errorf("model=m1: count = %d, want 2", n)
	}
	if n := query("region=gcp-us"); n != 1 {
		t.Errorf("region=gcp-us: count = %d, want 1", n)
	}
	if n := query("key=alice&model=m2"); n != 1 {
		t.Errorf("key=alice&model=m2: count = %d, want 1", n)
	}
}

func TestSummaryHandler_AggregatesByKeyAndModel(t *testing.T) {
	hub, token := testHub(t)
	postIngest(t, hub, token, usageRecord{RequestID: "r1", TS: "t1", Region: "a100b", KeyLabel: "alice", Model: "m1", Status: 200, PromptTokens: 100, CompletionTokens: 20})
	postIngest(t, hub, token, usageRecord{RequestID: "r2", TS: "t2", Region: "a100b", KeyLabel: "alice", Model: "m1", Status: 200, PromptTokens: 50, CompletionTokens: 10})
	postIngest(t, hub, token, usageRecord{RequestID: "r3", TS: "t3", Region: "a100b", KeyLabel: "alice", Model: "m1", Status: 500, PromptTokens: 0, CompletionTokens: 0})

	req := httptest.NewRequest(http.MethodGet, "/usage/summary", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	hub.summaryHandler(w, req)
	var resp struct {
		Summary []summaryRow `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Summary) != 1 {
		t.Fatalf("summary rows = %d, want 1 (one key x model group)", len(resp.Summary))
	}
	s := resp.Summary[0]
	if s.Requests != 3 {
		t.Errorf("requests = %d, want 3", s.Requests)
	}
	if s.PromptTokens != 150 || s.CompletionTokens != 30 {
		t.Errorf("tokens = (%d, %d), want (150, 30)", s.PromptTokens, s.CompletionTokens)
	}
	if s.Errors != 1 {
		t.Errorf("errors = %d, want 1 (the 500 status row)", s.Errors)
	}
}

func TestUsageHandler_RequiresAuth(t *testing.T) {
	hub, _ := testHub(t)
	req := httptest.NewRequest(http.MethodGet, "/usage", nil)
	w := httptest.NewRecorder()
	hub.usageHandler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestSummaryHandler_RequiresAuth(t *testing.T) {
	hub, _ := testHub(t)
	req := httptest.NewRequest(http.MethodGet, "/usage/summary", nil)
	w := httptest.NewRecorder()
	hub.summaryHandler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func postSubscriberSet(t *testing.T, hub *meterHub, token string, s subscriberStatus) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/subscribers/set", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	hub.subscriberSetHandler(w, req)
	return w
}

func getSubscriber(t *testing.T, hub *meterHub, token, key string) (*httptest.ResponseRecorder, subscriberStatus) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/subscribers/get?key="+key, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	hub.subscriberGetHandler(w, req)
	var s subscriberStatus
	json.Unmarshal(w.Body.Bytes(), &s)
	return w, s
}

func TestSubscriberSet_RequiresAuth(t *testing.T) {
	hub, _ := testHub(t)
	req := httptest.NewRequest(http.MethodPost, "/subscribers/set", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	hub.subscriberSetHandler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestSubscriberSet_RequiresKeyLabel(t *testing.T) {
	hub, token := testHub(t)
	w := postSubscriberSet(t, hub, token, subscriberStatus{Status: "active"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing key_label: status = %d, want 400", w.Code)
	}
}

func TestSubscriberSet_RejectsInvalidStatus(t *testing.T) {
	hub, token := testHub(t)
	w := postSubscriberSet(t, hub, token, subscriberStatus{KeyLabel: "alice", Status: "bogus"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid status: status = %d, want 400", w.Code)
	}
}

func TestSubscriberSetAndGet_RoundTrip(t *testing.T) {
	hub, token := testHub(t)
	w := postSubscriberSet(t, hub, token, subscriberStatus{KeyLabel: "alice", Status: "active", Plan: "pro"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("set status = %d, want 204", w.Code)
	}
	_, s := getSubscriber(t, hub, token, "alice")
	if s.Status != "active" || s.Plan != "pro" || s.Source != "manual" {
		t.Errorf("got %+v, want status=active plan=pro source=manual", s)
	}
}

func TestSubscriberSet_UpsertOverwritesPreviousStatus(t *testing.T) {
	hub, token := testHub(t)
	postSubscriberSet(t, hub, token, subscriberStatus{KeyLabel: "alice", Status: "active"})
	postSubscriberSet(t, hub, token, subscriberStatus{KeyLabel: "alice", Status: "canceled"})
	_, s := getSubscriber(t, hub, token, "alice")
	if s.Status != "canceled" {
		t.Errorf("status = %q, want canceled (upsert must overwrite)", s.Status)
	}
}

func TestSubscriberGet_UnknownKeyReturns200Unknown(t *testing.T) {
	hub, token := testHub(t)
	w, s := getSubscriber(t, hub, token, "nobody")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (unknown is not an error)", w.Code)
	}
	if s.Status != "unknown" {
		t.Errorf("status = %q, want unknown", s.Status)
	}
}

func TestSubscriberGet_RequiresAuth(t *testing.T) {
	hub, _ := testHub(t)
	req := httptest.NewRequest(http.MethodGet, "/subscribers/get?key=alice", nil)
	w := httptest.NewRecorder()
	hub.subscriberGetHandler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestSubscriberList_FiltersByStatus(t *testing.T) {
	hub, token := testHub(t)
	postSubscriberSet(t, hub, token, subscriberStatus{KeyLabel: "alice", Status: "active"})
	postSubscriberSet(t, hub, token, subscriberStatus{KeyLabel: "bob", Status: "canceled"})

	req := httptest.NewRequest(http.MethodGet, "/subscribers/list?status=canceled", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	hub.subscriberListHandler(w, req)
	var resp struct {
		Subscribers []subscriberStatus `json:"subscribers"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Subscribers) != 1 || resp.Subscribers[0].KeyLabel != "bob" {
		t.Errorf("status=canceled filter = %+v, want just bob", resp.Subscribers)
	}
}

func TestSubscriberList_RequiresAuth(t *testing.T) {
	hub, _ := testHub(t)
	req := httptest.NewRequest(http.MethodGet, "/subscribers/list", nil)
	w := httptest.NewRecorder()
	hub.subscriberListHandler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func postWebhook(t *testing.T, hub *meterHub, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/subscribers/webhook", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	hub.subscriberWebhookHandler(w, req)
	return w
}

func TestWebhook_ActiveSubscriptionSetsStatus(t *testing.T) {
	hub, token := testHub(t)
	body := `{"type":"customer.subscription.updated","data":{"object":{"id":"sub_1","status":"active",
		"metadata":{"key_label":"alice"},"items":{"data":[{"price":{"nickname":"pro"}}]}}}}`
	w := postWebhook(t, hub, token, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	_, s := getSubscriber(t, hub, token, "alice")
	if s.Status != "active" || s.Plan != "pro" || s.Source != "stripe" || s.ExternalID != "sub_1" {
		t.Errorf("got %+v, want status=active plan=pro source=stripe external_id=sub_1", s)
	}
}

func TestWebhook_CanceledSubscriptionBlocksKey(t *testing.T) {
	hub, token := testHub(t)
	postWebhook(t, hub, token, `{"type":"customer.subscription.updated","data":{"object":{"id":"sub_1","status":"active","metadata":{"key_label":"alice"}}}}`)
	postWebhook(t, hub, token, `{"type":"customer.subscription.deleted","data":{"object":{"id":"sub_1","status":"canceled","metadata":{"key_label":"alice"}}}}`)
	_, s := getSubscriber(t, hub, token, "alice")
	if s.Status != "canceled" {
		t.Errorf("status = %q, want canceled", s.Status)
	}
}

func TestWebhook_MissingKeyLabelIsAcceptedNotRetried(t *testing.T) {
	hub, token := testHub(t)
	w := postWebhook(t, hub, token, `{"type":"customer.subscription.updated","data":{"object":{"id":"sub_1","status":"active"}}}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (accept-and-ignore, don't trigger Stripe retry)", w.Code)
	}
}

func TestWebhook_UnrecognizedStripeStatusIsAcceptedNotRetried(t *testing.T) {
	hub, token := testHub(t)
	w := postWebhook(t, hub, token, `{"type":"customer.subscription.updated","data":{"object":{"id":"sub_1","status":"incomplete_expired","metadata":{"key_label":"alice"}}}}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (accept-and-ignore unmapped status)", w.Code)
	}
	if _, s := getSubscriber(t, hub, token, "alice"); s.Status != "unknown" {
		t.Errorf("unrecognized status must not write a row, got %+v", s)
	}
}

func TestWebhook_RequiresAuth(t *testing.T) {
	hub, _ := testHub(t)
	req := httptest.NewRequest(http.MethodPost, "/subscribers/webhook", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	hub.subscriberWebhookHandler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}
