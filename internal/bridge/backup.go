package bridge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// BackupRunner 台账一致性冷备份与 Kopia 可读出口，内嵌于主服务，每 15 分钟执行。
// 产物权限：文件 0644、目录 0755，属主即服务账号；
// Kopia（无特权 uid 0）经 other 位读取，热 state 仍 0700 不可读。
type BackupRunner struct {
	store       *Ledger
	exportDir   string
	interval    time.Duration
	mu          sync.Mutex
	lastSuccess atomic.Int64
}

func NewBackupRunner(store *Ledger, exportDir string) *BackupRunner {
	return &BackupRunner{store: store, exportDir: exportDir, interval: 15 * time.Minute}
}

// Fresh 最近一次成功备份距今不超过 30 分钟（供 /readyz）。
func (b *BackupRunner) Fresh() bool {
	last := b.lastSuccess.Load()
	return last > 0 && time.Now().Unix()-last < 1800
}

func (b *BackupRunner) LastSuccess() int64 { return b.lastSuccess.Load() }

// Loop 启动即执行一次，随后按周期执行；失败记录日志并等待下一周期。
func (b *BackupRunner) Loop(ctx context.Context) {
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		if err := b.RunOnce(ctx); err != nil && ctx.Err() == nil {
			logJSON(map[string]any{"event": "ledger_backup_error", "error": err.Error()})
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

var checkpointName = regexp.MustCompile(`^[A-Za-z0-9_-]+\.sqlite3$`)
var recoveryName = regexp.MustCompile(`^([0-9a-f-]{36})-([0-9a-f]{64})\.mp4$`)

// RunOnce 单次备份导出；进程内互斥替代原 cron flock。
func (b *BackupRunner) RunOnce(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	source := filepath.Join(b.store.dir, "ledger.sqlite3")
	if _, err := os.Stat(source); err != nil {
		return nil // 台账尚未创建，等待下一周期
	}
	folder := filepath.Join(b.store.dir, "backups")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return err
	}
	latest := filepath.Join(folder, "latest.sqlite3")
	digest, err := b.snapshot(latest)
	if err != nil {
		return err
	}
	exported, err := copyExport(latest, filepath.Join(b.exportDir, "latest.sqlite3"))
	if err != nil {
		return err
	}
	if exported != digest {
		return fmt.Errorf("export_checksum_mismatch")
	}
	checkpoints := 0
	entries, err := os.ReadDir(folder)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == "latest.sqlite3" || !strings.HasSuffix(name, ".sqlite3") {
			continue
		}
		if !checkpointName.MatchString(name) {
			return fmt.Errorf("invalid_checkpoint_name")
		}
		if _, err := copyExport(filepath.Join(folder, name), filepath.Join(b.exportDir, "checkpoints", name)); err != nil {
			return err
		}
		checkpoints++
	}
	recoveryCount := 0
	recoveryDir := filepath.Join(b.store.dir, "recovery")
	if entries, err := os.ReadDir(recoveryDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".mp4") {
				continue
			}
			m := recoveryName.FindStringSubmatch(name)
			if m == nil {
				return fmt.Errorf("invalid_recovery_name")
			}
			got, err := copyExport(filepath.Join(recoveryDir, name), filepath.Join(b.exportDir, "recovery", name))
			if err != nil {
				return err
			}
			if got != m[2] {
				return fmt.Errorf("recovery_checksum_mismatch")
			}
			recoveryCount++
		}
	}
	info, err := os.Stat(latest)
	if err != nil {
		return err
	}
	now := time.Now()
	status := map[string]any{
		"schema": 1, "backup_at": now.UTC().Format(time.RFC3339), "backup_epoch": now.Unix(),
		"sha256": digest, "bytes": info.Size(), "checkpoints": checkpoints,
		"recovery_files": recoveryCount, "records_restorable_from_sqlite_outbox": true,
	}
	if err := writeJSONAtomic(filepath.Join(folder, "latest-status.json"), status, 0o600); err != nil {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(b.exportDir, "latest-status.json"), status, 0o644); err != nil {
		return err
	}
	b.lastSuccess.Store(now.Unix())
	logJSON(map[string]any{"event": "ledger_backup_and_export_ok", "bytes": info.Size()})
	return nil
}

// snapshot 在线一致性备份（VACUUM INTO 含 WAL 已提交事务）+ 完整性校验 +
// 临时文件原子替换；产物为独立 DELETE journal 数据库。
func (b *BackupRunner) snapshot(latest string) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(latest), "ledger-*.partial")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()
	if _, err := b.store.db.Exec("VACUUM INTO ?", tmpPath); err != nil {
		return "", err
	}
	check, err := sql.Open("sqlite", "file:"+tmpPath+"?mode=ro")
	if err != nil {
		return "", err
	}
	var result string
	err = check.QueryRow("PRAGMA integrity_check").Scan(&result)
	check.Close()
	if err != nil {
		return "", err
	}
	if result != "ok" {
		return "", fmt.Errorf("backup_integrity_failed")
	}
	fd, err := os.OpenFile(tmpPath, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	if err := fd.Sync(); err != nil {
		fd.Close()
		return "", err
	}
	fd.Close()
	digest, err := fileSHA256(tmpPath)
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, latest); err != nil {
		return "", err
	}
	if err := syncDir(filepath.Dir(latest)); err != nil {
		return "", err
	}
	success = true
	return digest, nil
}

// copyExport 临时文件 + 原子替换 + 目录 fsync；复制前后比对源元数据；
// 目标已存在且哈希一致则跳过替换（避免 Kopia 无谓重扫）。返回内容 sha256。
func copyExport(src, dst string) (string, error) {
	info, err := os.Lstat(src)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("source_not_regular_file")
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return "", err
	}
	before, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	fd, err := os.CreateTemp(dir, "copy-*.partial")
	if err != nil {
		return "", err
	}
	tmp := fd.Name()
	success := false
	defer func() {
		if !success {
			os.Remove(tmp)
		}
	}()
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(fd, hash), in); err != nil {
		in.Close()
		fd.Close()
		return "", err
	}
	in.Close()
	if err := fd.Sync(); err != nil {
		fd.Close()
		return "", err
	}
	fd.Close()
	after, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return "", fmt.Errorf("source_changed_during_export")
	}
	digest := fmt.Sprintf("%x", hash.Sum(nil))
	if existing, err := fileSHA256(dst); err == nil && existing == digest {
		if err := os.Chmod(dst, 0o644); err != nil {
			return "", err
		}
		return digest, nil // 跳过替换，临时文件由 defer 清理
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	success = true
	return digest, nil
}

// writeJSONAtomic 原子写状态文件（临时文件 + rename + 目录 fsync）。
func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	fd, err := os.CreateTemp(filepath.Dir(path), "status-*.partial")
	if err != nil {
		return err
	}
	tmp := fd.Name()
	success := false
	defer func() {
		if !success {
			os.Remove(tmp)
		}
	}()
	enc := json.NewEncoder(fd)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		fd.Close()
		return err
	}
	if err := fd.Sync(); err != nil {
		fd.Close()
		return err
	}
	fd.Close()
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	success = true
	return nil
}

func fileSHA256(path string) (string, error) {
	fd, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fd.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, fd); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func logJSON(value map[string]any) {
	data, err := json.Marshal(value)
	if err == nil {
		fmt.Fprintln(os.Stderr, string(data))
	}
}

// sortedKeys 供稳定日志/输出使用。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
