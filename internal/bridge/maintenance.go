package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Maintenance 只读对账辅助、签名计划与显式维护。
// 没有定时删除、直接数据库或文件系统操作；一次性计划 15 分钟有效。
type Maintenance struct {
	p *Processor
}

func NewMaintenance(p *Processor) *Maintenance { return &Maintenance{p: p} }

// encodedPath v3.4 以存储路径区分原生提取视频；不用于移动文件。
func encodedPath(asset Asset) bool {
	if asset == nil {
		return false
	}
	return strings.Contains(asset.str("originalPath"), "/encoded-video/")
}

func rowDigest(row *Job) string {
	if row == nil {
		return ""
	}
	data, _ := json.Marshal(row.rowMap())
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func snapshotAsset(asset Asset) map[string]any {
	if asset == nil {
		return nil
	}
	out := map[string]any{}
	for _, k := range []string{"id", "ownerId", "type", "checksum", "livePhotoVideoId", "isTrashed", "originalPath", "visibility"} {
		out[k] = asset[k]
	}
	return out
}

func (m *Maintenance) references(ctx context.Context, vid string) ([]string, error) {
	photos, err := m.p.api.AllPhotos(ctx, true)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, photo := range photos {
		if photo.str("livePhotoVideoId") == vid {
			refs = append(refs, photo.str("id"))
		}
	}
	sort.Strings(refs)
	if refs == nil {
		refs = []string{}
	}
	return refs, nil
}

func (m *Maintenance) verifyOwned(ctx context.Context, row *Job, video Asset) ([]byte, error) {
	if row != nil && (row.State == "retired" || row.State == "migrated") {
		return nil, conflict("MAINTENANCE_ALREADY_COMPLETED")
	}
	if row == nil || row.CreatedByUs == 0 || row.VideoID == "" {
		return nil, conflict("VIDEO_PROVENANCE_UNPROVEN")
	}
	created := row.UploadIntent
	if created == 0 {
		created = row.Created
	}
	if nowUnix()-created < float64(m.p.cfg.GraceSeconds) {
		return nil, conflict("CLEANUP_GRACE_NOT_ELAPSED")
	}
	if video == nil {
		return nil, nil
	}
	if _, err := m.p.validateVideo(ctx, row.VideoID, row.VideoSHA1); err != nil {
		return nil, err
	}
	if encodedPath(video) {
		return nil, conflict("NATIVE_VIDEO_MUST_NOT_BE_DELETED")
	}
	raw, err := m.p.api.Original(ctx, row.VideoID)
	if err != nil {
		return nil, err
	}
	if sha256Hex(raw) != row.VideoSHA256 {
		return nil, conflict("VIDEO_CONTENT_CHANGED")
	}
	return raw, nil
}

// nativeProof 原生识别样本：同机型、未经兼容层处理、原生存储且字节一致。
func (m *Maintenance) nativeProof(ctx context.Context, pid string) (map[string]any, error) {
	pid, err := assetID(pid)
	if err != nil {
		return nil, err
	}
	row, err := m.p.store.Get(pid)
	if err != nil {
		return nil, err
	}
	if row != nil {
		return nil, conflict("PROOF_MUST_BE_UNTOUCHED_PHOTO")
	}
	photo, err := m.p.api.GetAsset(ctx, pid)
	if err != nil {
		return nil, err
	}
	if err := m.p.checkPhoto(photo); err != nil {
		return nil, err
	}
	vid := photo.str("livePhotoVideoId")
	if vid == "" {
		return nil, conflict("NATIVE_SUPPORT_NOT_PROVEN")
	}
	video, err := m.p.api.GetAsset(ctx, vid)
	if err != nil {
		return nil, err
	}
	if err := m.p.ownedAsset(video, "VIDEO"); err != nil {
		return nil, err
	}
	if !encodedPath(video) {
		return nil, conflict("PROOF_NOT_NATIVE_STORAGE")
	}
	original, err := m.p.api.Original(ctx, pid)
	if err != nil {
		return nil, err
	}
	raw, _, err := m.p.inspect(ctx, original)
	if err != nil {
		return nil, err
	}
	nativeRaw, err := m.p.api.Original(ctx, vid)
	if err != nil {
		return nil, err
	}
	if string(nativeRaw) != string(raw) {
		return nil, conflict("PROOF_VIDEO_NOT_LOSSLESS")
	}
	return map[string]any{
		"photo_id": pid, "checksum": photo.str("checksum"),
		"video_id": vid, "video_sha256": sha256Hex(raw),
	}, nil
}

func (m *Maintenance) planKey() ([]byte, error) {
	text, err := m.p.store.Meta("plan_key")
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(text)
}

func signPlan(key []byte, content any) (string, error) {
	data, err := json.Marshal(content)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Plan 生成只读维护计划。
func (m *Maintenance) Plan(ctx context.Context, operation string, ids []string, proofID string) (map[string]any, error) {
	identity, err := m.p.Identity(ctx)
	if err != nil {
		return nil, err
	}
	switch operation {
	case "cleanup", "rollback", "migrate":
	default:
		return nil, errCode("INVALID_PLAN_REQUEST")
	}
	if len(ids) < 1 || len(ids) > 100 {
		return nil, errCode("INVALID_PLAN_REQUEST")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return nil, errCode("DUPLICATE_PLAN_PHOTO")
		}
		seen[id] = true
	}
	var native map[string]any
	if operation == "migrate" {
		if proofID == "" {
			return nil, conflict("NATIVE_PROOF_REQUIRED")
		}
		if native, err = m.nativeProof(ctx, proofID); err != nil {
			return nil, err
		}
	}
	var actions []map[string]any
	for _, rawID := range ids {
		pid, err := assetID(rawID)
		if err != nil {
			return nil, err
		}
		row, err := m.p.store.Get(pid)
		if err != nil {
			return nil, err
		}
		var video Asset
		if row != nil && row.VideoID != "" {
			if video, err = m.p.api.GetAsset(ctx, row.VideoID); err != nil {
				return nil, err
			}
		}
		if _, err := m.verifyOwned(ctx, row, video); err != nil {
			return nil, err
		}
		photo, err := m.p.api.GetAsset(ctx, pid)
		if err != nil {
			return nil, err
		}
		if photo != nil {
			if err := m.p.ownedAsset(photo, "IMAGE"); err != nil {
				return nil, err
			}
			if photo.truth("isTrashed") || photo.str("checksum") != row.SourceSHA1 {
				return nil, conflict("PHOTO_CHANGED_OR_TRASHED")
			}
			original, err := m.p.api.Original(ctx, pid)
			if err != nil {
				return nil, err
			}
			if sha256Hex(original) != row.SourceSHA256 {
				return nil, conflict("PHOTO_CONTENT_CHANGED")
			}
		} else if operation == "migrate" {
			return nil, conflict("MIGRATION_PHOTO_MISSING")
		}
		refs, err := m.references(ctx, row.VideoID)
		if err != nil {
			return nil, err
		}
		for _, r := range refs {
			if operation == "rollback" || operation == "migrate" {
				if r != pid {
					return nil, conflict("VIDEO_STILL_REFERENCED")
				}
			} else {
				return nil, conflict("VIDEO_STILL_REFERENCED")
			}
		}
		if operation == "cleanup" && len(refs) > 0 {
			return nil, conflict("VIDEO_STILL_REFERENCED")
		}
		if operation == "migrate" {
			if current := photo.str("livePhotoVideoId"); current != "" && current != row.VideoID {
				other, err := m.p.api.GetAsset(ctx, current)
				if err != nil {
					return nil, err
				}
				if !encodedPath(other) {
					return nil, conflict("ASSOCIATION_CHANGED")
				}
			}
		}
		actions = append(actions, map[string]any{
			"photo_id": pid, "row_digest": rowDigest(row), "photo": snapshotAsset(photo),
			"video": snapshotAsset(video), "video_id": row.VideoID, "references": refs,
		})
	}
	installation, err := m.p.store.Meta("installation")
	if err != nil {
		return nil, err
	}
	content := map[string]any{
		"schema": 1, "operation": operation, "identity": identity,
		"installation": installation, "created": nowUnix(), "expires": nowUnix() + 900,
		"native_proof": native, "actions": actions,
	}
	key, err := m.planKey()
	if err != nil {
		return nil, err
	}
	signature, err := signPlan(key, content)
	if err != nil {
		return nil, err
	}
	return map[string]any{"plan": content, "confirmation": signature}, nil
}

func (m *Maintenance) recoveryPath(row *Job) string {
	return filepath.Join(m.p.store.dir, "recovery", row.PhotoID+"-"+row.VideoSHA256+".mp4")
}

// spool 落盘恢复副本：同目录临时文件 + fsync + 原子替换。
func (m *Maintenance) spool(row *Job, raw []byte) error {
	path := m.recoveryPath(row)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if existing, err := fileSHA256(path); err == nil {
		if existing != row.VideoSHA256 {
			return conflict("RECOVERY_FILE_CHANGED")
		}
		return nil
	}
	if raw == nil {
		return conflict("RECOVERY_FILE_MISSING")
	}
	fd, err := os.CreateTemp(filepath.Dir(path), ".recovery-*")
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
	if _, err := fd.Write(raw); err != nil {
		fd.Close()
		return err
	}
	if err := fd.Sync(); err != nil {
		fd.Close()
		return err
	}
	fd.Close()
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	success = true
	return nil
}

// Execute 校验并执行签名计划；一次性消费，中断后必须重新生成。
func (m *Maintenance) Execute(ctx context.Context, signed map[string]any, confirmation string, confirmed bool) (map[string]any, error) {
	if !confirmed || m.p.cfg.Mode != "apply" {
		return nil, errCode("MAINTENANCE_WINDOW_REQUIRED")
	}
	if err := m.p.writing(); err != nil {
		return nil, err
	}
	content, ok := signed["plan"].(map[string]any)
	if !ok {
		return nil, errCode("PLAN_SIGNATURE_INVALID")
	}
	key, err := m.planKey()
	if err != nil {
		return nil, err
	}
	expected, err := signPlan(key, content)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(confirmation), []byte(expected)) || signed["confirmation"] != expected {
		return nil, errCode("PLAN_SIGNATURE_INVALID")
	}
	if content["schema"] != float64(1) && content["schema"] != 1 {
		return nil, errCode("PLAN_EXPIRED_OR_UNSUPPORTED")
	}
	expires, _ := content["expires"].(float64)
	if expires < nowUnix() {
		return nil, errCode("PLAN_EXPIRED_OR_UNSUPPORTED")
	}
	identity, err := m.p.Identity(ctx)
	if err != nil {
		return nil, err
	}
	identityJSON, _ := json.Marshal(identity)
	contentIdentity, _ := json.Marshal(content["identity"])
	if string(identityJSON) != string(contentIdentity) {
		return nil, errCode("PLAN_INSTANCE_CHANGED")
	}
	installation, err := m.p.store.Meta("installation")
	if err != nil {
		return nil, err
	}
	if content["installation"] != installation {
		return nil, errCode("PLAN_INSTANCE_CHANGED")
	}
	operation := fmt.Sprint(content["operation"])
	if operation == "migrate" {
		if err := m.p.RequirePermissions(ctx, []string{"asset.delete", "job.create"}); err != nil {
			return nil, err
		}
		proof, _ := content["native_proof"].(map[string]any)
		if proof == nil {
			return nil, conflict("NATIVE_PROOF_CHANGED")
		}
		current, err := m.nativeProof(ctx, fmt.Sprint(proof["photo_id"]))
		if err != nil {
			return nil, err
		}
		currentJSON, _ := json.Marshal(current)
		proofJSON, _ := json.Marshal(proof)
		if string(currentJSON) != string(proofJSON) {
			return nil, conflict("NATIVE_PROOF_CHANGED")
		}
	} else {
		if err := m.p.RequirePermissions(ctx, []string{"asset.delete"}); err != nil {
			return nil, err
		}
	}
	// 一次性消费；不能重放陈旧快照。
	if err := m.p.store.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO meta VALUES (?,?)", "used_plan:"+expected, fmt.Sprint(nowUnix()))
		return err
	}); err != nil {
		return nil, errCode("PLAN_ALREADY_USED")
	}
	actions, _ := content["actions"].([]any)
	for _, item := range actions {
		action, _ := item.(map[string]any)
		if err := m.executeAction(ctx, operation, action); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"status": "completed", "physical_file_cleanup": "not_observable_via_api",
		"recovery_copies": "retained_until_explicit_purge",
	}, nil
}

func (m *Maintenance) executeAction(ctx context.Context, operation string, action map[string]any) error {
	pid, vid := fmt.Sprint(action["photo_id"]), fmt.Sprint(action["video_id"])
	row, err := m.p.store.Get(pid)
	if err != nil {
		return err
	}
	if rowDigest(row) != fmt.Sprint(action["row_digest"]) {
		return conflict("LEDGER_CHANGED_REPLAN_REQUIRED")
	}
	photo, err := m.p.api.GetAsset(ctx, pid)
	if err != nil {
		return err
	}
	video, err := m.p.api.GetAsset(ctx, vid)
	if err != nil {
		return err
	}
	photoJSON, _ := json.Marshal(snapshotAsset(photo))
	videoJSON, _ := json.Marshal(snapshotAsset(video))
	wantPhoto, _ := json.Marshal(action["photo"])
	wantVideo, _ := json.Marshal(action["video"])
	if string(photoJSON) != string(wantPhoto) || string(videoJSON) != string(wantVideo) {
		return conflict("REMOTE_CHANGED_REPLAN_REQUIRED")
	}
	raw, err := m.verifyOwned(ctx, row, video)
	if err != nil {
		return err
	}
	if photo != nil {
		original, err := m.p.api.Original(ctx, pid)
		if err != nil {
			return err
		}
		if sha256Hex(original) != row.SourceSHA256 {
			return conflict("PHOTO_CONTENT_CHANGED")
		}
	}
	refs, err := m.references(ctx, vid)
	if err != nil {
		return err
	}
	refsJSON, _ := json.Marshal(refs)
	wantRefs, _ := json.Marshal(action["references"])
	if string(refsJSON) != string(wantRefs) {
		return conflict("REFERENCES_CHANGED_REPLAN_REQUIRED")
	}
	if err := m.spool(row, raw); err != nil {
		return err
	}
	if err := m.p.store.Audit("maintenance_before", pid, map[string]any{"operation": operation, "ledger": row.rowMap()}); err != nil {
		return err
	}
	state := "cleanup_pending"
	if operation == "migrate" {
		state = "migration_wait"
	}
	if err := m.p.store.Update(pid, state, "maintenance_begin", nil); err != nil {
		return err
	}
	if photo != nil && photo.str("livePhotoVideoId") == vid {
		if operation == "cleanup" {
			return conflict("VIDEO_STILL_REFERENCED")
		}
		if err := m.p.writing(); err != nil {
			return err
		}
		if err := m.p.api.Link(ctx, pid, ""); err != nil {
			return err
		}
	}
	refs, err = m.references(ctx, vid)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return conflict("VIDEO_STILL_REFERENCED")
	}
	if video != nil {
		if err := m.p.writing(); err != nil {
			return err
		}
		if err := m.p.api.Delete(ctx, vid); err != nil {
			return err
		}
		deadline := time.Now().Add(60 * time.Second)
		for {
			gone, err := m.p.api.GetAsset(ctx, vid)
			if err != nil {
				return err
			}
			if gone == nil {
				break
			}
			if time.Now().After(deadline) {
				return errCode("DELETE_PENDING_REPLAN_REQUIRED")
			}
			if !sleepCtx(ctx, time.Second) {
				return ctx.Err()
			}
		}
	}
	if err := m.p.store.Audit("api_delete_confirmed_files_unverified", pid, map[string]any{
		"video_id": vid, "last_known_path": row.OriginalPath}); err != nil {
		return err
	}
	if operation == "migrate" {
		if err := m.p.writing(); err != nil {
			return err
		}
		if err := m.p.api.RefreshMetadata(ctx, pid); err != nil {
			return err
		}
		return m.finishMigration(ctx, pid, row)
	}
	return m.p.store.Update(pid, "retired", "retired", map[string]any{"error": nil})
}

func (m *Maintenance) finishMigration(ctx context.Context, pid string, row *Job) error {
	deadline := time.Now().Add(time.Duration(m.p.cfg.MediaTimeout) * time.Second)
	for time.Now().Before(deadline) {
		photo, err := m.p.api.GetAsset(ctx, pid)
		if err != nil {
			return err
		}
		if photo == nil || photo.str("checksum") != row.SourceSHA1 {
			return conflict("MIGRATION_PHOTO_CHANGED")
		}
		if vid := photo.str("livePhotoVideoId"); vid != "" {
			video, err := m.p.api.GetAsset(ctx, vid)
			if err != nil {
				return err
			}
			if err := m.p.ownedAsset(video, "VIDEO"); err != nil {
				return err
			}
			raw, err := m.p.api.Original(ctx, vid)
			if err != nil {
				return err
			}
			if !encodedPath(video) || sha1Base64(raw) != row.VideoSHA1 {
				return conflict("NATIVE_RESULT_NOT_CANONICAL")
			}
			if err := m.p.store.Audit("native_migration_complete", pid, map[string]any{"new_video_id": vid}); err != nil {
				return err
			}
			return m.p.store.Update(pid, "migrated", "migrated", map[string]any{
				"native_video_id": vid, "error": nil})
		}
		if !sleepCtx(ctx, time.Second) {
			return ctx.Err()
		}
	}
	if err := m.p.store.Update(pid, "migration_wait", "migration_pending", map[string]any{"error": "NATIVE_EXTRACTION_PENDING"}); err != nil {
		return err
	}
	return errCode("NATIVE_EXTRACTION_PENDING")
}

// RestoreBridge 中断维护后重新排队恢复桥接；不覆盖现有关联。
func (m *Maintenance) RestoreBridge(ctx context.Context, pid string, confirmed bool) error {
	if !confirmed || m.p.cfg.Mode != "apply" {
		return errCode("MAINTENANCE_WINDOW_REQUIRED")
	}
	if err := m.p.writing(); err != nil {
		return err
	}
	if _, err := m.p.Identity(ctx); err != nil {
		return err
	}
	uid, err := assetID(pid)
	if err != nil {
		return err
	}
	row, err := m.p.store.Get(uid)
	if err != nil {
		return err
	}
	if row == nil || (row.State != "migration_wait" && row.State != "cleanup_pending") {
		return conflict("NOT_IN_INTERRUPTED_MAINTENANCE")
	}
	photo, err := m.p.api.GetAsset(ctx, uid)
	if err != nil {
		return err
	}
	if err := m.p.checkPhoto(photo); err != nil {
		return err
	}
	if photo.str("checksum") != row.SourceSHA1 || photo.str("livePhotoVideoId") != "" {
		return conflict("PHOTO_CHANGED_OR_ALREADY_LINKED")
	}
	if digest, err := fileSHA256(m.recoveryPath(row)); err != nil || digest != row.VideoSHA256 {
		return conflict("RECOVERY_FILE_MISSING_OR_CHANGED")
	}
	if row.VideoID != "" {
		old, err := m.p.api.GetAsset(ctx, row.VideoID)
		if err != nil {
			return err
		}
		if old != nil {
			return conflict("OLD_VIDEO_STILL_EXISTS_REPLAN_REQUIRED")
		}
	}
	if err := m.p.store.Audit("restore_bridge_requested", uid, map[string]any{"previous_video_id": row.VideoID}); err != nil {
		return err
	}
	return m.p.store.Update(uid, "pending", "restore_queued", map[string]any{
		"video_id": nil, "native_video_id": nil, "created_by_us": 0, "upload_intent": nil,
		"attempts": 0, "due": nowUnix(), "error": nil})
}

// PurgeRecovery 显式删除已完成维护的本地恢复副本；不删除审计。
func (m *Maintenance) PurgeRecovery(pid, confirmation string) error {
	uid, err := assetID(pid)
	if err != nil {
		return err
	}
	row, err := m.p.store.Get(uid)
	if err != nil {
		return err
	}
	if row == nil || (row.State != "retired" && row.State != "migrated") || confirmation != row.VideoSHA256 {
		return conflict("RECOVERY_PURGE_NOT_CONFIRMED")
	}
	if err := os.Remove(m.recoveryPath(row)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return m.p.store.Audit("recovery_purged", uid, nil)
}
