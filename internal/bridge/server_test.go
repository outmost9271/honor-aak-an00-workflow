package bridge

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func testRuntime(t *testing.T) *runtime {
	t.Helper()
	store := openTestLedger(t)
	journal, err := NewJournal(store, 1<<20, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{WebhookSecret: "0123456789abcdef0123456789abcdef", MaxQueue: 100, StateDir: store.dir}
	return &runtime{
		processor: NewProcessor(cfg, store, nil, journal),
		journal:   journal,
		backup:    NewBackupRunner(store, t.TempDir()),
	}
}

func webhookBody(id string) string {
	return `{"type":"AssetV1","trigger":"AssetMetadataExtraction","data":{"asset":{"id":"` + id + `"}}}`
}

func TestWebhookAccepted(t *testing.T) {
	rt := testRuntime(t)
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(webhookBody("0b6f2d3a-1111-4222-8333-444455556666")))
	req.Header.Set("X-Motion-Secret", "0123456789abcdef0123456789abcdef")
	rec := httptest.NewRecorder()
	rt.webhook(rec, req)
	if rec.Code != 202 {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["accepted"] != true || body["new"] != true {
		t.Fatalf("body: %v", body)
	}
	row, err := rt.processor.store.Get("0b6f2d3a-1111-4222-8333-444455556666")
	if err != nil || row == nil || row.State != "pending" {
		t.Fatalf("row: %+v %v", row, err)
	}
	// 重复事件不重复入队。
	req = httptest.NewRequest("POST", "/webhook", strings.NewReader(webhookBody("0b6f2d3a-1111-4222-8333-444455556666")))
	req.Header.Set("X-Motion-Secret", "0123456789abcdef0123456789abcdef")
	rec = httptest.NewRecorder()
	rt.webhook(rec, req)
	if rec.Code != 202 || !strings.Contains(rec.Body.String(), `"new":false`) {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body)
	}
}

func TestWebhookRejections(t *testing.T) {
	rt := testRuntime(t)
	// 错误密钥
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(webhookBody("0b6f2d3a-1111-4222-8333-444455556666")))
	req.Header.Set("X-Motion-Secret", "wrong-secret-wrong-secret-wrong")
	rec := httptest.NewRecorder()
	rt.webhook(rec, req)
	if rec.Code != 401 {
		t.Fatalf("bad secret status = %d", rec.Code)
	}
	// 错误事件类型
	req = httptest.NewRequest("POST", "/webhook", strings.NewReader(`{"type":"Other","trigger":"X"}`))
	req.Header.Set("X-Motion-Secret", "0123456789abcdef0123456789abcdef")
	rec = httptest.NewRecorder()
	rt.webhook(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad event status = %d", rec.Code)
	}
	// 非法 UUID
	req = httptest.NewRequest("POST", "/webhook", strings.NewReader(webhookBody("not-a-uuid")))
	req.Header.Set("X-Motion-Secret", "0123456789abcdef0123456789abcdef")
	rec = httptest.NewRecorder()
	rt.webhook(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad uuid status = %d", rec.Code)
	}
	// 非 POST
	req = httptest.NewRequest("GET", "/webhook", nil)
	rec = httptest.NewRecorder()
	rt.webhook(rec, req)
	if rec.Code != 404 {
		t.Fatalf("GET status = %d", rec.Code)
	}
}
