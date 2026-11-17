package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func TestJournalFlushAndRotation(t *testing.T) {
	store := openTestLedger(t)
	journal, err := NewJournal(store, 4096, "Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue("photo-a", 100, false); err != nil {
		t.Fatal(err)
	}
	if err := store.Update("photo-a", "", "validated", map[string]any{"source_sha1": "abc"}); err != nil {
		t.Fatal(err)
	}
	exported, err := journal.Flush()
	if err != nil || exported != 2 {
		t.Fatalf("Flush = %d, %v", exported, err)
	}
	again, err := journal.Flush()
	if err != nil || again != 0 {
		t.Fatalf("second Flush = %d, %v", again, err)
	}
	files, err := filepath.Glob(filepath.Join(journal.Dir(), "events-*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("records files: %v %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d", len(lines))
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["schema_version"] != float64(1) || first["event"] != "enqueue" {
		t.Fatalf("record: %v", first)
	}
	installation, _ := store.Meta("installation")
	if !strings.HasPrefix(first["event_id"].(string), installation+":") {
		t.Fatalf("event_id: %v", first["event_id"])
	}
	if _, ok := first["context"]; !ok {
		t.Fatal("missing context")
	}
	// 追加一个事件，随后制造不完整尾行，验证修复。
	if _, err := store.Enqueue("photo-b", 100, false); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Flush(); err != nil {
		t.Fatal(err)
	}
	fd, err := os.OpenFile(files[0], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fd.WriteString("{\"partial\":"); err != nil {
		t.Fatal(err)
	}
	fd.Close()
	if _, err := store.Enqueue("photo-c", 100, false); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Flush(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(files[0])
	lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %d not valid JSON after tail repair: %v", i, err)
		}
	}
	if len(lines) != 4 {
		t.Fatalf("lines after repair = %d", len(lines))
	}
}

func TestJournalRejectsSmallLimit(t *testing.T) {
	store := openTestLedger(t)
	if _, err := NewJournal(store, 100, "UTC"); err == nil {
		t.Fatal("expected RECORD_SIZE_LIMIT_TOO_SMALL")
	}
	if _, err := NewJournal(store, 4096, "Not/AZone"); err == nil {
		t.Fatal("expected INVALID_RECORD_TIMEZONE")
	}
}

func TestConfigBasics(t *testing.T) {
	if u, err := canonicalURL("http://immich-server:2283"); err != nil || u != "http://immich-server:2283/api" {
		t.Fatalf("canonicalURL = %q, %v", u, err)
	}
	if u, err := canonicalURL("http://immich-server:2283/api/"); err != nil || u != "http://immich-server:2283/api" {
		t.Fatalf("canonicalURL api = %q, %v", u, err)
	}
	if _, err := canonicalURL("http://user:pass@host"); err == nil {
		t.Fatal("expected INVALID_IMMICH_URL for credentials")
	}
	if _, err := canonicalURL("ftp://host"); err == nil {
		t.Fatal("expected INVALID_IMMICH_URL for scheme")
	}
	if _, err := assetID("not-a-uuid"); err == nil {
		t.Fatal("expected INVALID_ASSET_ID")
	}
	upper := "0B6F2D3A-1111-4222-8333-444455556666"
	id, err := assetID(upper)
	if err != nil || id != strings.ToLower(upper) {
		t.Fatalf("assetID = %q, %v", id, err)
	}
	t.Setenv("IMMICH_URL", "http://immich-server:2283")
	t.Setenv("BRIDGE_MODE", "apply")
	t.Setenv("IMMICH_WRITE_ENABLED", "false")
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IMMICH_API_KEY_FILE", secretFile)
	t.Setenv("WEBHOOK_SECRET_FILE", secretFile)
	t.Setenv("RECORD_TIMEZONE", "Asia/Shanghai")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected APPLY_REQUIRES_WRITE_ACK")
	}
	t.Setenv("IMMICH_WRITE_ENABLED", "true")
	t.Setenv("MAX_ATTEMPTS", "5")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "apply" || !cfg.Writes || cfg.MaxAttempts != 5 || cfg.MaxSeconds != 30 {
		t.Fatalf("config: %+v", cfg)
	}
}
