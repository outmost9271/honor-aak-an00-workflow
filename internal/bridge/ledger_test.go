package bridge

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func openTestLedger(t *testing.T) *Ledger {
	t.Helper()
	store, err := OpenLedger(t.TempDir())
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestLedgerQueueLifecycle(t *testing.T) {
	store := openTestLedger(t)
	photo := "0b6f2d3a-1111-4222-8333-444455556666"
	added, err := store.Enqueue(photo, 100, false)
	if err != nil || !added {
		t.Fatalf("first enqueue = %v, %v", added, err)
	}
	added, err = store.Enqueue(photo, 100, false)
	if err != nil || added {
		t.Fatalf("duplicate pending enqueue = %v, %v", added, err)
	}
	row, err := store.Claim()
	if err != nil || row == nil {
		t.Fatalf("Claim: %v %v", row, err)
	}
	if row.State != "processing" || row.Attempts != 1 {
		t.Fatalf("unexpected claimed row %+v", row)
	}
	if err := store.Update(photo, "linked", "link_confirmed", map[string]any{
		"error": nil, "source_sha1": "abc", "video_id": "vid"}); err != nil {
		t.Fatal(err)
	}
	// linked 不能被普通 webhook 重置。
	added, err = store.Enqueue(photo, 100, false)
	if err != nil || added {
		t.Fatalf("linked enqueue = %v, %v", added, err)
	}
	added, err = store.Enqueue(photo, 100, true)
	if err != nil || added {
		t.Fatalf("linked retry-terminal enqueue = %v, %v", added, err)
	}
	got, err := store.Get(photo)
	if err != nil || got == nil || got.SourceSHA1 != "abc" || got.State != "linked" {
		t.Fatalf("Get: %+v %v", got, err)
	}
	// processing 回收
	if err := store.Update(photo, "processing", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(photo)
	if got.State != "retry" || got.Error != "PROCESS_RESTART" {
		t.Fatalf("Recover: %+v", got)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats["states"].(map[string]int)["retry"] != 1 {
		t.Fatalf("Stats: %v", stats)
	}
}

func TestLedgerBindAndMeta(t *testing.T) {
	store := openTestLedger(t)
	identity := map[string]any{"url": "http://x/api", "owner": "me"}
	// 模拟旧版（Python json.dumps 带空格分隔符）写入的持久 identity。
	if err := store.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO meta VALUES ('identity',?)", `{"owner": "me", "url": "http://x/api"}`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Bind(identity); err != nil {
		t.Fatalf("legacy identity format must be accepted: %v", err)
	}
	if err := store.Bind(identity); err != nil {
		t.Fatal(err)
	}
	if err := store.Bind(map[string]any{"url": "http://y/api", "owner": "me"}); err == nil {
		t.Fatal("expected INSTANCE_OR_OWNER_CHANGED")
	}
	installation, err := store.Meta("installation")
	if err != nil || len(installation) != 36 {
		t.Fatalf("installation: %q %v", installation, err)
	}
	key, err := store.Meta("plan_key")
	if err != nil || len(key) != 64 {
		t.Fatalf("plan_key: %q %v", key, err)
	}
}

func TestLedgerBackup(t *testing.T) {
	store := openTestLedger(t)
	if _, err := store.Enqueue("photo-1", 100, false); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "backup.sqlite3")
	if err := store.Backup(target); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := store.Backup(target); err == nil {
		t.Fatal("expected BACKUP_TARGET_EXISTS")
	}
	check, err := sql.Open("sqlite", "file:"+target+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var count int
	if err := check.QueryRow("SELECT count(*) FROM jobs").Scan(&count); err != nil || count != 1 {
		t.Fatalf("backup jobs count = %d, %v", count, err)
	}
	var version int
	if err := check.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("backup user_version = %d, %v", version, err)
	}
}

func TestProcessLockExclusive(t *testing.T) {
	dir := t.TempDir()
	lock, err := ProcessLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := ProcessLock(dir); err == nil {
		t.Fatal("expected EXECUTOR_ALREADY_RUNNING")
	}
}

func TestBackupRunnerExport(t *testing.T) {
	store := openTestLedger(t)
	if _, err := store.Enqueue("photo-1", 100, false); err != nil {
		t.Fatal(err)
	}
	exportDir := filepath.Join(t.TempDir(), "export")
	runner := NewBackupRunner(store, exportDir)
	if err := runner.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !runner.Fresh() {
		t.Fatal("expected fresh after success")
	}
	latest := filepath.Join(exportDir, "latest.sqlite3")
	info, err := os.Stat(latest)
	if err != nil || info.Size() <= 100 {
		t.Fatalf("latest.sqlite3: %v %v", info, err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("latest mode = %v", info.Mode())
	}
	if dirInfo, err := os.Stat(exportDir); err != nil || dirInfo.Mode().Perm() != 0o755 {
		t.Fatalf("export dir mode = %v %v", dirInfo.Mode(), err)
	}
	var status map[string]any
	data, err := os.ReadFile(filepath.Join(exportDir, "latest-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := jsonUnmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	digest, _ := fileSHA256(latest)
	if status["sha256"] != digest || status["bytes"] != float64(info.Size()) {
		t.Fatalf("status mismatch: %v", status)
	}
	// 幂等：无新事件时第二次运行不替换出口文件。
	if err := runner.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	again, err := os.Stat(latest)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, again) || !again.ModTime().Equal(info.ModTime()) {
		t.Fatal("export file replaced despite identical content (hash instability?)")
	}
	// checkpoints 白名单与 recovery 哈希校验
	if err := store.Backup(filepath.Join(store.dir, "backups", "after-test.sqlite3")); err != nil {
		t.Fatal(err)
	}
	recoveryDir := filepath.Join(store.dir, "recovery")
	if err := os.MkdirAll(recoveryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("recovery-video")
	name := "0b6f2d3a-1111-4222-8333-444455556666-" + sha256Hex(content) + ".mp4"
	if err := os.WriteFile(filepath.Join(recoveryDir, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(exportDir, "checkpoints", "after-test.sqlite3")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(exportDir, "recovery", name)); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(exportDir, "latest-status.json"))
	if err := jsonUnmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	if status["checkpoints"] != float64(1) || status["recovery_files"] != float64(1) {
		t.Fatalf("status counts: %v", status)
	}
}
