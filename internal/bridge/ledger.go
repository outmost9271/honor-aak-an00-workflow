package bridge

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// Job 台账行。空字符串/零值对应 SQL NULL（与既有实现的 falsy 判断等价）。
type Job struct {
	PhotoID       string
	State         string
	Attempts      int
	Due           float64
	Created       float64
	Updated       float64
	Error         string
	SourceSHA1    string
	SourceSHA256  string
	VideoSHA1     string
	VideoSHA256   string
	VideoBytes    int64
	VideoID       string
	NativeVideoID string
	CreatedByUs   int
	UploadIntent  float64
	OriginalPath  string
	Media         string
}

var jobColumns = "photo_id,state,attempts,due,created,updated,error,source_sha1,source_sha256," +
	"video_sha1,video_sha256,video_bytes,video_id,native_video_id,created_by_us,upload_intent,original_path,media"

// nullString JSON 序列化时保持 NULL 语义。
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// rowMap 完整行映射（维护计划摘要用）。
func (j *Job) rowMap() map[string]any {
	var videoBytes, uploadIntent any
	if j.VideoBytes != 0 {
		videoBytes = j.VideoBytes
	}
	if j.UploadIntent != 0 {
		uploadIntent = j.UploadIntent
	}
	return map[string]any{
		"photo_id": j.PhotoID, "state": j.State, "attempts": j.Attempts,
		"due": j.Due, "created": j.Created, "updated": j.Updated, "error": nullString(j.Error),
		"source_sha1": nullString(j.SourceSHA1), "source_sha256": nullString(j.SourceSHA256),
		"video_sha1": nullString(j.VideoSHA1), "video_sha256": nullString(j.VideoSHA256),
		"video_bytes": videoBytes, "video_id": nullString(j.VideoID),
		"native_video_id": nullString(j.NativeVideoID), "created_by_us": j.CreatedByUs,
		"upload_intent": uploadIntent, "original_path": nullString(j.OriginalPath),
		"media": nullString(j.Media),
	}
}

// auditRecord 审计事件中的台账投影（键集合与既有实现一致）。
func (j *Job) auditRecord() map[string]any {
	full := j.rowMap()
	keys := []string{"photo_id", "video_id", "native_video_id", "source_sha1", "source_sha256",
		"video_sha1", "video_sha256", "video_bytes", "state", "attempts", "error", "created_by_us"}
	out := map[string]any{}
	for _, k := range keys {
		out[k] = full[k]
	}
	return out
}

// Ledger SQLite 持久队列、台账与操作审计。单卷只允许一个执行进程。
type Ledger struct {
	db  *sql.DB
	dir string
}

func nowUnix() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// OpenLedger 打开并初始化台账；schema 与既有生产数据库兼容（user_version 0/1/2）。
func OpenLedger(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=synchronous(FULL)"+
		"&_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_txlock=immediate", filepath.Join(dir, "ledger.sqlite3"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	l := &Ledger{db: db, dir: dir}
	if err := l.init(); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

func (l *Ledger) Close() error { return l.db.Close() }

func (l *Ledger) init() error {
	var version int
	if err := l.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != 1 && version != 2 {
		return errCode("UNKNOWN_LEDGER_SCHEMA")
	}
	_, err := l.db.Exec(`
		CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS jobs (
			photo_id TEXT PRIMARY KEY, state TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0, due REAL NOT NULL,
			created REAL NOT NULL, updated REAL NOT NULL, error TEXT,
			source_sha1 TEXT, source_sha256 TEXT, video_sha1 TEXT, video_sha256 TEXT,
			video_bytes INTEGER, video_id TEXT, native_video_id TEXT, created_by_us INTEGER NOT NULL DEFAULT 0,
			upload_intent REAL, original_path TEXT, media TEXT
		);
		CREATE TABLE IF NOT EXISTS audit (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT, at REAL NOT NULL,
			photo_id TEXT, event TEXT NOT NULL, detail TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS jobs_due ON jobs(state,due);`)
	if err != nil {
		return err
	}
	return l.tx(func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('jobs') WHERE name='native_video_id'").Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := tx.Exec("ALTER TABLE jobs ADD COLUMN native_video_id TEXT"); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("PRAGMA user_version=2"); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT OR IGNORE INTO meta VALUES ('installation',?)", newUUID()); err != nil {
			return err
		}
		key := make([]byte, 32)
		if _, err := tx.Exec("INSERT OR IGNORE INTO meta VALUES ('plan_key',?)", fmt.Sprintf("%x", randomBytes(key))); err != nil {
			return err
		}
		return nil
	})
}

// tx 写事务（DSN _txlock=immediate，等价 BEGIN IMMEDIATE）。
func (l *Ledger) tx(fn func(*sql.Tx) error) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func scanJob(s interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var errStr, s1, s256, v1, v256, vid, nvid, opath, media sql.NullString
	var vbytes sql.NullInt64
	var intent sql.NullFloat64
	err := s.Scan(&j.PhotoID, &j.State, &j.Attempts, &j.Due, &j.Created, &j.Updated,
		&errStr, &s1, &s256, &v1, &v256, &vbytes, &vid, &nvid, &j.CreatedByUs, &intent, &opath, &media)
	if err != nil {
		return nil, err
	}
	j.Error, j.SourceSHA1, j.SourceSHA256 = errStr.String, s1.String, s256.String
	j.VideoSHA1, j.VideoSHA256 = v1.String, v256.String
	j.VideoBytes, j.VideoID, j.NativeVideoID = vbytes.Int64, vid.String, nvid.String
	j.UploadIntent, j.OriginalPath, j.Media = intent.Float64, opath.String, media.String
	return &j, nil
}

func (l *Ledger) getTx(q interface {
	QueryRow(string, ...any) *sql.Row
}, photoID string) (*Job, error) {
	j, err := scanJob(q.QueryRow("SELECT "+jobColumns+" FROM jobs WHERE photo_id=?", photoID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

func (l *Ledger) Get(photoID string) (*Job, error) { return l.getTx(l.db, photoID) }

func (l *Ledger) Meta(key string) (string, error) {
	var value string
	err := l.db.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// Bind 绑定首次使用的实例与账号；换绑拒绝执行。
// 与旧版持久数据兼容：解析后深度比较，不受 JSON 分隔符格式影响。
func (l *Ledger) Bind(identity map[string]any) error {
	return l.tx(func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRow("SELECT value FROM meta WHERE key='identity'").Scan(&existing)
		if err == nil {
			var old map[string]any
			if json.Unmarshal([]byte(existing), &old) == nil {
				if !reflect.DeepEqual(old, identity) {
					return errCode("INSTANCE_OR_OWNER_CHANGED")
				}
				return nil
			}
			text, _ := json.Marshal(identity)
			if existing != string(text) {
				return errCode("INSTANCE_OR_OWNER_CHANGED")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		text, err := json.Marshal(identity)
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT OR IGNORE INTO meta VALUES ('identity',?)", string(text))
		return err
	})
}

func auditTx(tx *sql.Tx, event, photoID string, detail map[string]any) error {
	var record map[string]any
	if photoID != "" {
		j, err := scanJob(tx.QueryRow("SELECT "+jobColumns+" FROM jobs WHERE photo_id=?", photoID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if j != nil {
			record = j.auditRecord()
		}
	}
	if record == nil {
		record = map[string]any{"photo_id": nullString(photoID)}
	}
	if detail == nil {
		detail = map[string]any{}
	}
	payload, err := json.Marshal(map[string]any{
		"event_uuid": newUUID(), "record": record, "data": detail})
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO audit(at,photo_id,event,detail) VALUES (?,?,?,?)",
		nowUnix(), nullString(photoID), event, string(payload))
	return err
}

func (l *Ledger) Audit(event, photoID string, detail map[string]any) error {
	return l.tx(func(tx *sql.Tx) error { return auditTx(tx, event, photoID, detail) })
}

// Enqueue 持久排队；linked 等终态不被 Webhook 重置，避免丢失来源台账。
func (l *Ledger) Enqueue(photoID string, maximum int, retryTerminal bool) (bool, error) {
	added := false
	return added, l.tx(func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRow("SELECT state FROM jobs WHERE photo_id=?", photoID).Scan(&state)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		exists := err == nil
		if exists {
			switch state {
			case "pending", "retry", "processing":
				return nil
			}
			if !retryTerminal {
				return nil
			}
			switch state {
			case "linked", "retired", "migrated", "cleanup_pending", "migration_wait":
				return nil
			}
		}
		var active int
		if err := tx.QueryRow("SELECT count(*) FROM jobs WHERE state IN ('pending','retry','processing')").Scan(&active); err != nil {
			return err
		}
		if active >= maximum {
			return errCode("QUEUE_FULL")
		}
		now := nowUnix()
		if exists {
			if _, err := tx.Exec("UPDATE jobs SET state='pending',attempts=0,due=?,updated=?,error=NULL WHERE photo_id=?", now, now, photoID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec("INSERT INTO jobs(photo_id,state,due,created,updated) VALUES (?,'pending',?,?,?)", photoID, now, now, now); err != nil {
				return err
			}
		}
		added = true
		return auditTx(tx, "enqueue", photoID, nil)
	})
}

// Recover 启动时回收 processing；仅持有进程排他锁的执行器调用。
func (l *Ledger) Recover() error {
	return l.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT photo_id FROM jobs WHERE state='processing'")
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if _, err := tx.Exec("UPDATE jobs SET state='retry',due=?,error='PROCESS_RESTART' WHERE state='processing'", nowUnix()); err != nil {
			return err
		}
		for _, id := range ids {
			if err := auditTx(tx, "recovered_after_restart", id, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// Claim 认领下一到期任务；无任务返回 (nil, nil)。
func (l *Ledger) Claim() (*Job, error) {
	var job *Job
	return job, l.tx(func(tx *sql.Tx) error {
		j, err := scanJob(tx.QueryRow("SELECT "+jobColumns+
			" FROM jobs WHERE state IN ('pending','retry') AND due<=? ORDER BY due,photo_id LIMIT 1", nowUnix()))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE jobs SET state='processing',attempts=attempts+1,updated=? WHERE photo_id=?", nowUnix(), j.PhotoID); err != nil {
			return err
		}
		if err := auditTx(tx, "processing", j.PhotoID, nil); err != nil {
			return err
		}
		if job, err = l.getTx(tx, j.PhotoID); err != nil {
			return err
		}
		return nil
	})
}

func (l *Ledger) Rows() ([]Job, error) {
	rows, err := l.db.Query("SELECT " + jobColumns + " FROM jobs ORDER BY photo_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

var updatableFields = map[string]bool{
	"attempts": true, "due": true, "error": true, "source_sha1": true, "source_sha256": true,
	"video_sha1": true, "video_sha256": true, "video_bytes": true, "video_id": true,
	"native_video_id": true, "created_by_us": true, "upload_intent": true, "original_path": true, "media": true,
}

// Update 更新白名单字段并可选写入状态与审计事件。
func (l *Ledger) Update(photoID, state, event string, fields map[string]any) error {
	if fields == nil {
		fields = map[string]any{}
	}
	for k := range fields {
		if !updatableFields[k] {
			return fmt.Errorf("invalid ledger field %s", k)
		}
	}
	return l.tx(func(tx *sql.Tx) error {
		set := "updated=?"
		args := []any{nowUnix()}
		if state != "" {
			set += ",state=?"
			args = append(args, state)
		}
		// 固定顺序保证 SQL 可预测。
		for _, k := range []string{"attempts", "due", "error", "source_sha1", "source_sha256",
			"video_sha1", "video_sha256", "video_bytes", "video_id", "native_video_id",
			"created_by_us", "upload_intent", "original_path", "media"} {
			if v, ok := fields[k]; ok {
				set += "," + k + "=?"
				args = append(args, v)
			}
		}
		args = append(args, photoID)
		if _, err := tx.Exec("UPDATE jobs SET "+set+" WHERE photo_id=?", args...); err != nil {
			return err
		}
		if event != "" {
			detail := map[string]any{}
			if state != "" {
				detail["state"] = state
			}
			return auditTx(tx, event, photoID, detail)
		}
		return nil
	})
}

// ResetRecordCursor 重建记录前重置导出游标（rebuild-records 用）。
func (l *Ledger) ResetRecordCursor() error {
	return l.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM meta WHERE key='record_cursor'")
		return err
	})
}

func (l *Ledger) Stats() (map[string]any, error) {
	rows, err := l.db.Query("SELECT state,count(*) FROM jobs GROUP BY state")
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			rows.Close()
			return nil, err
		}
		counts[state] = n
	}
	rows.Close()
	var oldest sql.NullFloat64
	if err := l.db.QueryRow("SELECT min(created) FROM jobs WHERE state IN ('pending','retry')").Scan(&oldest); err != nil {
		return nil, err
	}
	age := 0
	if oldest.Valid {
		age = int(nowUnix() - oldest.Float64)
	}
	return map[string]any{"states": counts, "oldest_pending_age_seconds": age}, nil
}

// Backup 在线一致性备份（VACUUM INTO 包含 WAL 已提交事务），目标必须不存在。
func (l *Ledger) Backup(target string) error {
	if _, err := os.Stat(target); err == nil {
		return errCode("BACKUP_TARGET_EXISTS")
	}
	fd, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errCode("BACKUP_TARGET_EXISTS")
	}
	fd.Close()
	success := false
	defer func() {
		if !success {
			os.Remove(target)
		}
	}()
	if _, err := l.db.Exec("VACUUM INTO ?", target); err != nil {
		return err
	}
	check, err := sql.Open("sqlite", "file:"+target)
	if err != nil {
		return err
	}
	defer check.Close()
	var result string
	if err := check.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return errCode("BACKUP_INTEGRITY_FAILED")
	}
	success = true
	return nil
}

// ProcessLock 单执行器排他锁（flock，进程退出自动释放）。
func ProcessLock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "process.lock"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errCode("EXECUTOR_ALREADY_RUNNING")
	}
	return f, nil
}

func randomBytes(b []byte) []byte {
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
