// honor-aak-an00-workflow：HONOR 动态照片 Immich 兼容服务（Go 实现）。
// 单容器：鉴权 Webhook + 持久队列 + 定时台账备份导出。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bridge "github.com/outmost9271/honor-aak-an00-workflow/internal/bridge"
)

const version = "0.3.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		code := "LOCAL_OR_UNEXPECTED_ERROR"
		var bridgeErr *bridge.BridgeError
		var skipErr *bridge.SkipError
		var conflictErr *bridge.ConflictError
		var apiErr *bridge.APIError
		switch {
		case errors.As(err, &bridgeErr):
			code = bridgeErr.Code
		case errors.As(err, &skipErr):
			code = skipErr.Code
		case errors.As(err, &conflictErr):
			code = conflictErr.Code
		case errors.As(err, &apiErr):
			code = apiErr.Error()
		}
		output(map[string]any{"error": code}, "")
		os.Exit(1)
	}
}

func output(value any, path string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return err
	}
	if path == "" {
		fmt.Print(buf.String())
		return nil
	}
	fd, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fd.Write(buf.Bytes()); err != nil {
		fd.Close()
		return err
	}
	if err := fd.Sync(); err != nil {
		fd.Close()
		return err
	}
	return fd.Close()
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func run(args []string) error {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Println(version)
		return nil
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: honor-aak-an00-workflow <serve|healthcheck|status|enqueue|work|reconcile|backup|export-records|rebuild-records|plan|execute|restore-bridge|purge-records> [options]")
	}
	command, rest := args[0], args[1:]
	if command == "healthcheck" {
		port := 8080
		if v := os.Getenv("PORT"); v != "" {
			fmt.Sscanf(v, "%d", &port)
		}
		return bridge.Healthcheck(port)
	}
	if command == "serve" {
		cfg, err := bridge.LoadConfig()
		if err != nil {
			return err
		}
		return bridge.Serve(cfg)
	}
	cfg, err := bridge.LoadConfig()
	if err != nil {
		return err
	}
	store, err := bridge.OpenLedger(cfg.StateDir)
	if err != nil {
		return err
	}
	defer store.Close()
	journal, err := bridge.NewJournal(store, cfg.RecordMaxBytes, cfg.RecordTimezone)
	if err != nil {
		return err
	}
	processor := bridge.NewProcessor(cfg, store, bridge.NewClient(cfg), journal)
	maintenance := bridge.NewMaintenance(processor)
	ctx := context.Background()

	switch command {
	case "status":
		stats, err := store.Stats()
		if err != nil {
			return err
		}
		return output(stats, "")
	case "backup":
		fs := newFlagSet("backup")
		target := fs.String("output", "", "备份目标路径（必须不存在）")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *target == "" {
			return fmt.Errorf("backup requires --output")
		}
		if err := store.Backup(*target); err != nil {
			return err
		}
		return output(map[string]any{"status": "backed_up", "records_can_be_rebuilt_from_outbox": true}, "")
	case "enqueue":
		fs := newFlagSet("enqueue")
		var photos multiString
		fs.Var(&photos, "photo", "照片 ID，可重复")
		scan := fs.Bool("scan", false, "扫描当前账号目标机型未关联图片")
		retryTerminal := fs.Bool("retry-terminal", false, "重新检查终止项")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		var ids []string
		if *scan {
			if _, err := processor.Identity(ctx); err != nil {
				return err
			}
			if ids, err = processor.ScanTargets(ctx); err != nil {
				return err
			}
		} else {
			ids = photos
		}
		if len(ids) == 0 {
			return fmt.Errorf("enqueue requires --photo or --scan")
		}
		count := 0
		for _, id := range ids {
			added, err := store.Enqueue(id, cfg.MaxQueue, *retryTerminal)
			if err != nil {
				return err
			}
			if added {
				count++
			}
		}
		// 正在运行的服务负责导出文件，避免另一个进程同时轮转。
		return output(map[string]any{"newly_queued": count, "durable_outbox": true}, "")
	}
	// 其余命令需要单执行器排他锁。
	lock, err := bridge.ProcessLock(cfg.StateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	switch command {
	case "work":
		once := false
		fs := newFlagSet("work")
		fs.BoolVar(&once, "once", false, "只处理一条")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if err := store.Recover(); err != nil {
			return err
		}
		if _, err := journal.Flush(); err != nil {
			return err
		}
		for {
			worked, err := processor.RunOne(ctx)
			if err != nil {
				return err
			}
			if _, err := journal.Flush(); err != nil {
				return err
			}
			if !worked || once {
				break
			}
		}
		stats, err := store.Stats()
		if err != nil {
			return err
		}
		if err := output(stats, ""); err != nil {
			return err
		}
		if states, ok := stats["states"].(map[string]int); ok {
			for _, s := range []string{"retry", "dead", "conflict", "processing"} {
				if states[s] > 0 {
					return fmt.Errorf("unfinished work present")
				}
			}
		}
		return nil
	case "reconcile":
		fs := newFlagSet("reconcile")
		target := fs.String("output", "", "报告输出路径")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		report, err := processor.Reconcile(ctx)
		if err != nil {
			return err
		}
		return output(report, *target)
	case "export-records":
		exported, err := journal.Flush()
		if err != nil {
			return err
		}
		return output(map[string]any{"exported": exported}, "")
	case "rebuild-records":
		// 先重置游标；即使随后崩溃，最多重复导出。
		if err := store.ResetRecordCursor(); err != nil {
			return err
		}
		archive := filepath.Join(cfg.StateDir, fmt.Sprintf("records-archive-%d", time.Now().UnixNano()))
		if err := os.Rename(journal.Dir(), archive); err != nil {
			return err
		}
		if err := os.MkdirAll(journal.Dir(), 0o700); err != nil {
			return err
		}
		exported, err := journal.Flush()
		if err != nil {
			return err
		}
		return output(map[string]any{"exported": exported}, "")
	case "plan":
		if len(rest) < 1 {
			return fmt.Errorf("usage: honor-aak-an00-workflow plan <cleanup|rollback|migrate> --photo ID... --output PATH [--native-proof-photo ID]")
		}
		operation := rest[0]
		fs := newFlagSet("plan")
		var photos multiString
		fs.Var(&photos, "photo", "照片 ID，可重复")
		proof := fs.String("native-proof-photo", "", "原生识别样本照片 ID")
		target := fs.String("output", "", "计划输出路径")
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		if *target == "" || len(photos) == 0 {
			return fmt.Errorf("plan requires --photo and --output")
		}
		plan, err := maintenance.Plan(ctx, operation, photos, *proof)
		if err != nil {
			return err
		}
		if err := output(plan, *target); err != nil {
			return err
		}
		content := plan["plan"].(map[string]any)
		actions, _ := content["actions"].([]any)
		return output(map[string]any{
			"confirmation": plan["confirmation"], "expires": content["expires"], "actions": len(actions),
		}, "")
	case "execute":
		fs := newFlagSet("execute")
		planPath := fs.String("plan", "", "签名计划路径")
		confirm := fs.String("confirm", "", "计划确认值")
		confirmed := fs.Bool("maintenance-confirmed", false, "确认处于维护窗口")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *planPath == "" {
			return fmt.Errorf("execute requires --plan")
		}
		info, err := os.Stat(*planPath)
		if err != nil {
			return err
		}
		if info.Size() > 2*1024*1024 {
			return fmt.Errorf("PLAN_TOO_LARGE")
		}
		data, err := os.ReadFile(*planPath)
		if err != nil {
			return err
		}
		var signed map[string]any
		if err := json.Unmarshal(data, &signed); err != nil {
			return err
		}
		result, err := maintenance.Execute(ctx, signed, *confirm, *confirmed)
		if err != nil {
			return err
		}
		return output(result, "")
	case "restore-bridge":
		fs := newFlagSet("restore-bridge")
		photo := fs.String("photo", "", "照片 ID")
		confirmed := fs.Bool("maintenance-confirmed", false, "确认处于维护窗口")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *photo == "" {
			return fmt.Errorf("restore-bridge requires --photo")
		}
		if err := maintenance.RestoreBridge(ctx, *photo, *confirmed); err != nil {
			return err
		}
		return output(map[string]any{"status": "queued"}, "")
	case "purge-recovery":
		fs := newFlagSet("purge-recovery")
		photo := fs.String("photo", "", "照片 ID")
		confirm := fs.String("confirm-video-sha256", "", "视频内容 SHA256")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *photo == "" {
			return fmt.Errorf("purge-recovery requires --photo")
		}
		if err := maintenance.PurgeRecovery(*photo, *confirm); err != nil {
			return err
		}
		return output(map[string]any{"status": "local_recovery_purged"}, "")
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

type multiString []string

func (m *multiString) String() string     { return fmt.Sprint(*m) }
func (m *multiString) Set(v string) error { *m = append(*m, v); return nil }
