package bridge

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Journal 事务 outbox → 按时区日期/大小轮转的持久 JSONL。
// 文件 fsync 先于 SQLite 游标；崩溃可能重复导出，不会静默漏记。
type Journal struct {
	store    *Ledger
	maxBytes int
	tz       *time.Location
	dir      string
	mu       sync.Mutex
	healthy  atomic.Bool
}

func NewJournal(store *Ledger, maxBytes int, timezoneName string) (*Journal, error) {
	if maxBytes < 4096 {
		return nil, errCode("RECORD_SIZE_LIMIT_TOO_SMALL")
	}
	tz, err := time.LoadLocation(timezoneName)
	if err != nil {
		return nil, errCode("INVALID_RECORD_TIMEZONE")
	}
	j := &Journal{store: store, maxBytes: maxBytes, tz: tz, dir: filepath.Join(store.dir, "records")}
	if err := os.MkdirAll(j.dir, 0o700); err != nil {
		return nil, err
	}
	j.healthy.Store(true)
	return j, nil
}

func (j *Journal) Healthy() bool { return j.healthy.Load() }

// Dir 记录目录（rebuild-records 归档用）。
func (j *Journal) Dir() string { return j.dir }

func syncDir(dir string) error {
	fd, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer fd.Close()
	return fd.Sync()
}

// target 返回当天应写入的分卷；修复崩溃留下的不完整尾行，必要时新建分卷。
func (j *Journal) target(day string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(j.dir, "events-"+day+"-*.jsonl"))
	if err != nil {
		return "", err
	}
	sort.Strings(matches)
	index := 1
	if len(matches) > 0 {
		path := matches[len(matches)-1]
		if err := repairTail(path); err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if info.Size() < int64(j.maxBytes) {
			return path, nil
		}
		index = parseIndex(path) + 1
	}
	path := filepath.Join(j.dir, fmt.Sprintf("events-%s-%06d.jsonl", day, index))
	fd, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if err := fd.Sync(); err != nil {
		fd.Close()
		return "", err
	}
	fd.Close()
	return path, syncDir(j.dir)
}

func parseIndex(path string) int {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	n, _ := strconv.Atoi(base[strings.LastIndex(base, "-")+1:])
	return n
}

// repairTail 截断最后不完整的行；SQLite 仍保留完整事务事件。
func repairTail(path string) error {
	fd, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer fd.Close()
	info, err := fd.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	size := info.Size()
	last := make([]byte, 1)
	if _, err := fd.ReadAt(last, size-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	end := int64(0)
	for position := size; position > 0; {
		start := max(0, position-8192)
		buf := make([]byte, position-start)
		if _, err := fd.ReadAt(buf, start); err != nil {
			return err
		}
		if i := strings.LastIndexByte(string(buf), '\n'); i >= 0 {
			end = start + int64(i) + 1
			break
		}
		position = start
	}
	if err := fd.Truncate(end); err != nil {
		return err
	}
	return fd.Sync()
}

func (j *Journal) append(record map[string]any) error {
	var buf strings.Builder
	enc := json.NewEncoder(&stringWriter{&buf})
	enc.SetEscapeHTML(false)
	if err := enc.Encode(record); err != nil { // Encode 自带换行
		return err
	}
	line := buf.String()
	if len(line) > j.maxBytes {
		return errCode("RECORD_EXCEEDS_ROTATION_LIMIT")
	}
	day := time.Now().In(j.tz).Format("2006-01-02")
	path, err := j.target(day)
	if err != nil {
		return err
	}
	if info, err := os.Stat(path); err != nil {
		return err
	} else if info.Size()+int64(len(line)) > int64(j.maxBytes) {
		next := filepath.Join(j.dir, fmt.Sprintf("events-%s-%06d.jsonl", day, parseIndex(path)+1))
		fd, err := os.OpenFile(next, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		fd.Close()
		if err := syncDir(j.dir); err != nil {
			return err
		}
		path = next
	}
	fd, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := fd.WriteString(line); err != nil {
		fd.Close()
		return err
	}
	if err := fd.Sync(); err != nil {
		fd.Close()
		return err
	}
	fd.Close()
	if info, err := os.Stat(path); err == nil && info.Size() >= int64(j.maxBytes) {
		_, err = j.target(day) // 精确达到上限立即创建下一分卷
		return err
	}
	return nil
}

type stringWriter struct{ b *strings.Builder }

func (w *stringWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

// Flush 导出新增审计事件；返回导出条数。
func (j *Journal) Flush() (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	total := 0
	fail := func(err error) (int, error) {
		j.healthy.Store(false)
		return total, err
	}
	for {
		cursorText, err := j.store.Meta("record_cursor")
		if err != nil {
			return fail(err)
		}
		cursor, _ := strconv.ParseInt(cursorText, 10, 64)
		rows, err := j.store.db.Query("SELECT sequence,at,event,detail FROM audit WHERE sequence>? ORDER BY sequence LIMIT 100", cursor)
		if err != nil {
			return fail(err)
		}
		type auditRow struct {
			seq    int64
			at     float64
			event  string
			detail string
		}
		var batch []auditRow
		for rows.Next() {
			var r auditRow
			if err := rows.Scan(&r.seq, &r.at, &r.event, &r.detail); err != nil {
				rows.Close()
				return fail(err)
			}
			batch = append(batch, r)
		}
		rows.Close()
		if len(batch) == 0 {
			j.healthy.Store(true)
			return total, nil
		}
		for _, r := range batch {
			var detail struct {
				EventUUID string         `json:"event_uuid"`
				Record    map[string]any `json:"record"`
				Data      map[string]any `json:"data"`
			}
			if err := json.Unmarshal([]byte(r.detail), &detail); err != nil {
				return fail(err)
			}
			installation, err := j.store.Meta("installation")
			if err != nil {
				return fail(err)
			}
			sec := int64(r.at)
			record := map[string]any{
				"schema_version": 1,
				"event_id":       installation + ":" + detail.EventUUID,
				"sequence":       r.seq,
				"timestamp":      time.Unix(sec, int64((r.at-float64(sec))*1e9)).UTC().Format("2006-01-02T15:04:05.999999-07:00"),
				"event":          r.event,
			}
			for k, v := range detail.Record {
				record[k] = v
			}
			context := map[string]any{}
			for _, k := range []string{"operation", "new_video_id", "previous_video_id"} {
				if v, ok := detail.Data[k]; ok {
					context[k] = v
				}
			}
			record["context"] = context
			if err := j.append(record); err != nil {
				return fail(err)
			}
			if err := j.store.tx(func(tx *sql.Tx) error {
				_, err := tx.Exec("INSERT INTO meta VALUES ('record_cursor',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", strconv.FormatInt(r.seq, 10))
				return err
			}); err != nil {
				return fail(err)
			}
			total++
		}
	}
}
