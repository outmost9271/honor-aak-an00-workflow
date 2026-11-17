package bridge

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// runtime 鉴权 Webhook、持久队列执行与健康检查；备份循环内嵌。
type runtime struct {
	processor *Processor
	journal   *Journal
	backup    *BackupRunner
	ready     atomic.Bool
	lastCheck atomic.Int64 // 最近 identity 成功（UnixNano）
	stopped   atomic.Bool
	lastError atomic.Value // string，去重 worker_paused 日志
}

// Serve 启动执行器；持有进程排他锁直到退出。
func Serve(cfg Config) error {
	lock, err := ProcessLock(cfg.StateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	store, err := OpenLedger(cfg.StateDir)
	if err != nil {
		return err
	}
	defer store.Close()
	journal, err := NewJournal(store, cfg.RecordMaxBytes, cfg.RecordTimezone)
	if err != nil {
		return err
	}
	if err := store.Recover(); err != nil {
		return err
	}
	rt := &runtime{
		processor: NewProcessor(cfg, store, NewClient(cfg), journal),
		journal:   journal,
		backup:    NewBackupRunner(store, envOr("BACKUP_EXPORT_DIR", "/backup-export")),
	}
	if _, err := journal.Flush(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go rt.backup.Loop(ctx)
	var wg sync.WaitGroup
	for range cfg.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt.worker(ctx)
		}()
	}
	slots := make(chan struct{}, 32)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		alive := !rt.stopped.Load()
		replyJSON(w, map[bool]int{true: 200, false: 503}[alive], map[string]any{"alive": alive})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ready := rt.ready.Load() && journal.Healthy() && rt.backup.Fresh() &&
			time.Since(time.Unix(0, rt.lastCheck.Load())) < 90*time.Second
		replyJSON(w, map[bool]int{true: 200, false: 503}[ready], map[string]any{
			"ready": ready, "backup_fresh": rt.backup.Fresh()})
	})
	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			rt.webhook(w, r)
		default:
			replyJSON(w, 503, map[string]any{"error": "SERVER_BUSY"})
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, 404, map[string]any{"error": "NOT_FOUND"})
	})
	server := &http.Server{
		Addr: fmt.Sprintf("0.0.0.0:%d", cfg.Port), Handler: mux,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384,
	}
	go func() {
		<-ctx.Done()
		rt.stopped.Store(true)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()
	err = server.ListenAndServe()
	if !errors.Is(err, http.ErrServerClosed) {
		rt.stopped.Store(true)
		stop()
		wg.Wait()
		return err
	}
	stop()
	wg.Wait()
	_, err = journal.Flush()
	return err
}

func replyJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		status = 500
		data = []byte(`{"error":"ENCODE_FAILED"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.WriteHeader(status)
	w.Write(data)
}

// webhook 接收 Immich 工作流事件；鉴权失败与格式错误不入队。
func (rt *runtime) webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		replyJSON(w, 404, map[string]any{"error": "NOT_FOUND"})
		return
	}
	secret := rt.processor.cfg.WebhookSecret
	if !hmac.Equal([]byte(r.Header.Get("X-Motion-Secret")), []byte(secret)) {
		replyJSON(w, 401, map[string]any{"error": "UNAUTHORIZED"})
		return
	}
	if r.Header.Get("Transfer-Encoding") != "" || r.ContentLength <= 0 {
		replyJSON(w, 411, map[string]any{"error": "CONTENT_LENGTH_REQUIRED"})
		return
	}
	if r.ContentLength > 65536 {
		replyJSON(w, 413, map[string]any{"error": "BODY_TOO_LARGE"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
	if err != nil {
		replyJSON(w, 400, map[string]any{"error": "INVALID_EVENT"})
		return
	}
	var event struct {
		Type    string `json:"type"`
		Trigger string `json:"trigger"`
		Data    struct {
			Asset struct {
				ID string `json:"id"`
			} `json:"asset"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &event); err != nil ||
		event.Type != "AssetV1" || event.Trigger != "AssetMetadataExtraction" {
		replyJSON(w, 400, map[string]any{"error": "INVALID_EVENT"})
		return
	}
	pid, err := assetID(event.Data.Asset.ID)
	if err != nil {
		replyJSON(w, 400, map[string]any{"error": "INVALID_EVENT"})
		return
	}
	added, err := rt.processor.store.Enqueue(pid, rt.processor.cfg.MaxQueue, false)
	var bridgeErr *BridgeError
	if errors.As(err, &bridgeErr) {
		status := 400
		if bridgeErr.Code == "QUEUE_FULL" {
			status = 503
		}
		replyJSON(w, status, map[string]any{"error": bridgeErr.Code})
		return
	}
	if err != nil {
		rt.ready.Store(false)
		replyJSON(w, 503, map[string]any{"error": "PERSISTENCE_UNAVAILABLE"})
		return
	}
	if _, err := rt.journal.Flush(); err != nil {
		rt.ready.Store(false)
		replyJSON(w, 503, map[string]any{"error": "PERSISTENCE_UNAVAILABLE"})
		return
	}
	replyJSON(w, 202, map[string]any{"accepted": true, "new": added})
}

// worker 周期校验实例、导出记录并处理队列；异常降级为暂停而非退出。
func (rt *runtime) worker(ctx context.Context) {
	for !rt.stopped.Load() {
		if time.Since(time.Unix(0, rt.lastCheck.Load())) > 30*time.Second {
			_, err := rt.processor.Identity(ctx)
			if err == nil {
				rt.lastCheck.Store(time.Now().UnixNano())
			} else {
				rt.pause(err)
				if !sleepCtx(ctx, 5*time.Second) {
					return
				}
				continue
			}
		}
		if _, err := rt.journal.Flush(); err != nil {
			rt.pause(err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		rt.ready.Store(true)
		worked, err := rt.processor.RunOne(ctx)
		if _, flushErr := rt.journal.Flush(); flushErr != nil && err == nil {
			err = flushErr
		}
		if err != nil {
			rt.pause(err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		rt.lastError.Store("")
		if !worked && !sleepCtx(ctx, time.Second) {
			return
		}
	}
}

func (rt *runtime) pause(err error) {
	rt.ready.Store(false)
	code := "LOCAL_PERSISTENCE_OR_WORKER_ERROR"
	var bridgeErr *BridgeError
	if errors.As(err, &bridgeErr) {
		code = bridgeErr.Code
	}
	if rt.lastError.Load() != code {
		logJSON(map[string]any{"event": "worker_paused", "code": code})
		rt.lastError.Store(code)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// healthcheck CLI：仅访问本容器 HTTP 健康端点，不访问 Immich。
func Healthcheck(port int) error {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("unhealthy: %d", resp.StatusCode)
	}
	return nil
}
