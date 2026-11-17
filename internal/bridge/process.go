package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Processor 单张照片处理流水线：只新增关联，不覆盖关联，不自动删除。
type Processor struct {
	cfg    Config
	store  *Ledger
	api    *Client
	journal *Journal
	owner  string
}

func NewProcessor(cfg Config, store *Ledger, api *Client, journal *Journal) *Processor {
	return &Processor{cfg: cfg, store: store, api: api, journal: journal}
}

// Identity 校验版本白名单、绑定实例与账号、检查 API 权限。
func (p *Processor) Identity(ctx context.Context) (map[string]any, error) {
	version, err := p.api.jsonCall(ctx, "GET", "/server/version", nil, false)
	if err != nil {
		return nil, err
	}
	text := fmt.Sprintf("%v.%v.%v", version["major"], version["minor"], version["patch"])
	if fmt.Sprint(version["prerelease"]) != "" && version["prerelease"] != nil {
		return nil, errCode("UNAPPROVED_IMMICH_VERSION")
	}
	approved := false
	for _, v := range p.cfg.Versions {
		if text == v {
			approved = true
		}
	}
	if !approved {
		return nil, errCode("UNAPPROVED_IMMICH_VERSION")
	}
	me, err := p.api.jsonCall(ctx, "GET", "/users/me", nil, false)
	if err != nil {
		return nil, err
	}
	owner, err := assetID(fmt.Sprint(me["id"]))
	if err != nil {
		return nil, err
	}
	if err := p.store.Bind(map[string]any{"url": p.cfg.URL, "owner": owner}); err != nil {
		return nil, err
	}
	p.owner = owner
	required := []string{"asset.read", "asset.download", "user.read"}
	if p.cfg.Mode == "apply" {
		required = append(required, "asset.upload", "asset.update")
	}
	if err := p.RequirePermissions(ctx, required); err != nil {
		return nil, err
	}
	return map[string]any{"url": p.cfg.URL, "owner": owner, "version": text}, nil
}

func (p *Processor) RequirePermissions(ctx context.Context, required []string) error {
	m, err := p.api.jsonCall(ctx, "GET", "/api-keys/me", nil, false)
	if err != nil {
		return err
	}
	granted := map[string]bool{}
	if list, ok := m["permissions"].([]any); ok {
		for _, item := range list {
			granted[fmt.Sprint(item)] = true
		}
	}
	if granted["all"] {
		return nil
	}
	for _, r := range required {
		if !granted[r] {
			return errCode("API_KEY_PERMISSIONS_INSUFFICIENT")
		}
	}
	return nil
}

// writing 写入前置确认：显式双开关 + 先落审计。
func (p *Processor) writing() error {
	if !p.cfg.Writes {
		return errCode("WRITE_DISABLED")
	}
	if p.journal != nil {
		_, err := p.journal.Flush()
		return err
	}
	return nil
}

func (p *Processor) ownedAsset(asset Asset, kind string) error {
	if asset == nil || asset.str("ownerId") != p.owner || asset.str("type") != kind {
		return conflict("ASSET_OWNER_OR_TYPE_MISMATCH")
	}
	if asset.truth("isExternal") || asset.str("libraryId") != "" {
		return conflict("EXTERNAL_LIBRARY_UNSUPPORTED")
	}
	return nil
}

func (p *Processor) checkPhoto(photo Asset) error {
	if err := p.ownedAsset(photo, "IMAGE"); err != nil {
		return err
	}
	if photo.truth("isTrashed") || photo.truth("isOffline") || photo.str("visibility") == "locked" {
		return skip("PHOTO_UNAVAILABLE")
	}
	make_, _ := photo["exifInfo"].(map[string]any)
	makeStr := ""
	model := ""
	if make_ != nil {
		makeStr, _ = make_["make"].(string)
		model, _ = make_["model"].(string)
	}
	if !equalFoldASCII(makeStr, "HONOR") || (p.cfg.Model != "" && model != p.cfg.Model) {
		return skip("NOT_TARGET_CAMERA")
	}
	return nil
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		ca, cb := a[i], b[i]
		if ca >= 'a' && ca <= 'z' {
			ca -= 32
		}
		if cb >= 'a' && cb <= 'z' {
			cb -= 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// inspect 无损提取并校验视频：MP4 结构、轨道、时长、尺寸，可选全片解码。
func (p *Processor) inspect(ctx context.Context, data []byte) ([]byte, string, error) {
	start, end, _, err := ParseMotion(data)
	if err != nil {
		return nil, "", skip("NO_SUPPORTED_MOTION_TRAILER")
	}
	video := data[start:end]
	dir, err := os.MkdirTemp("", "motion-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	source := filepath.Join(dir, "source.mp4")
	if err := os.WriteFile(source, video, 0o600); err != nil {
		return nil, "", err
	}
	mctx, cancel := context.WithTimeout(ctx, time.Duration(p.cfg.MediaTimeout)*time.Second)
	defer cancel()
	probe := exec.CommandContext(mctx, "ffprobe", "-v", "error", "-protocol_whitelist", "file,pipe",
		"-show_streams", "-show_format", "-of", "json", source)
	out, err := probe.Output()
	if err != nil {
		return nil, "", conflict("MEDIA_VALIDATION_FAILED")
	}
	var info struct {
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, "", conflict("MEDIA_VALIDATION_FAILED")
	}
	duration, err := strconv.ParseFloat(info.Format.Duration, 64)
	if err != nil {
		return nil, "", conflict("MEDIA_VALIDATION_FAILED")
	}
	videos := 0
	audio := false
	for _, s := range info.Streams {
		switch s.CodecType {
		case "video":
			videos++
			if s.Width <= 0 || s.Width > 8192 || s.Height <= 0 || s.Height > 8192 {
				return nil, "", conflict("UNSAFE_VIDEO_DIMENSIONS")
			}
		case "audio":
			audio = true
		}
	}
	if videos == 0 || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > float64(p.cfg.MaxSeconds) {
		return nil, "", conflict("UNSAFE_VIDEO_DURATION")
	}
	if p.cfg.RequireAudio && !audio {
		return nil, "", conflict("AUDIO_REQUIRED")
	}
	if p.cfg.Decode {
		cmd := exec.CommandContext(mctx, "ffmpeg", "-v", "error", "-nostdin", "-xerror",
			"-protocol_whitelist", "file,pipe", "-threads", "1", "-i", source,
			"-map", "0:v:0", "-map", "0:a:0?", "-fps_mode", "passthrough", "-f", "null", "-")
		if err := cmd.Run(); err != nil {
			return nil, "", conflict("MEDIA_VALIDATION_FAILED")
		}
	}
	type streamOut struct {
		CodecType  string `json:"codec_type,omitempty"`
		CodecName  string `json:"codec_name,omitempty"`
		Width      int    `json:"width,omitempty"`
		Height     int    `json:"height,omitempty"`
		SampleRate string `json:"sample_rate,omitempty"`
		Channels   int    `json:"channels,omitempty"`
	}
	streams := make([]streamOut, 0, len(info.Streams))
	for _, s := range info.Streams {
		streams = append(streams, streamOut{s.CodecType, s.CodecName, s.Width, s.Height, s.SampleRate, s.Channels})
	}
	media, err := json.Marshal(map[string]any{"duration": duration, "streams": streams})
	if err != nil {
		return nil, "", err
	}
	return video, string(media), nil
}

func (p *Processor) assertPhotoUnchanged(ctx context.Context, photoID, checksum, expectedLink string) (Asset, error) {
	current, err := p.api.GetAsset(ctx, photoID)
	if err != nil {
		return nil, err
	}
	if err := p.checkPhoto(current); err != nil {
		return nil, err
	}
	if current.str("checksum") != checksum {
		return nil, conflict("PHOTO_CHANGED")
	}
	if current.str("livePhotoVideoId") != expectedLink {
		return nil, conflict("ASSOCIATION_CHANGED")
	}
	return current, nil
}

func (p *Processor) validateVideo(ctx context.Context, videoID, checksum string) (Asset, error) {
	asset, err := p.api.GetAsset(ctx, videoID)
	if err != nil {
		return nil, err
	}
	if asset == nil {
		return nil, conflict("RECORDED_VIDEO_MISSING")
	}
	if err := p.ownedAsset(asset, "VIDEO"); err != nil {
		return nil, err
	}
	if asset.truth("isTrashed") || asset.str("visibility") == "locked" || asset.str("checksum") != checksum {
		return nil, conflict("RECORDED_VIDEO_CHANGED")
	}
	return asset, nil
}

// waitUploadProcessing 等待上传流水线越过存储模板与缩略图，再允许隐藏关联。
func (p *Processor) waitUploadProcessing(ctx context.Context, photoID, videoID, checksum string) error {
	if err := p.store.Audit("waiting_upload_processing", photoID, nil); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Duration(p.cfg.UploadReadyTimeout) * time.Second)
	for {
		video, err := p.validateVideo(ctx, videoID, checksum)
		if err != nil {
			return err
		}
		if video.str("thumbhash") != "" {
			return p.store.Update(photoID, "", "upload_processing_ready", map[string]any{"original_path": video.str("originalPath")})
		}
		if time.Now().After(deadline) {
			// 保留已确认的视频 ID，重试复用，不重复上传、不提前隐藏。
			return errCode("UPLOAD_PROCESSING_PENDING")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Process 处理单条任务；返回错误由 RunOne 分类落账。
func (p *Processor) Process(ctx context.Context, row *Job) error {
	if _, err := p.Identity(ctx); err != nil { // 每项开始检查版本与实例
		return err
	}
	pid := row.PhotoID
	photo, err := p.api.GetAsset(ctx, pid)
	if err != nil {
		return err
	}
	if photo == nil {
		return skip("PHOTO_MISSING")
	}
	if err := p.checkPhoto(photo); err != nil {
		return err
	}
	if current := photo.str("livePhotoVideoId"); current != "" {
		if row.SourceSHA1 != "" && photo.str("checksum") != row.SourceSHA1 {
			return conflict("PHOTO_CHANGED")
		}
		if row.VideoID == current && row.CreatedByUs != 0 {
			if _, err := p.validateVideo(ctx, current, row.VideoSHA1); err != nil {
				return err
			}
			return p.store.Update(pid, "linked", "already_linked", map[string]any{"error": nil})
		}
		if row.VideoID != "" {
			return conflict("ASSOCIATION_CHANGED")
		}
		return skip("ALREADY_HAS_MOTION")
	}
	source, err := p.api.Original(ctx, pid)
	if err != nil {
		return err
	}
	sourceSHA1 := sha1Base64(source)
	sourceSHA256 := sha256Hex(source)
	if sourceSHA1 != photo.str("checksum") {
		return conflict("PHOTO_CHECKSUM_MISMATCH")
	}
	if row.SourceSHA256 != "" && row.SourceSHA256 != sourceSHA256 {
		return conflict("PHOTO_CHANGED")
	}
	video, media, err := p.inspect(ctx, source)
	if err != nil {
		return err
	}
	videoSHA1, videoSHA256 := sha1Base64(video), sha256Hex(video)
	if row.VideoSHA256 != "" && row.VideoSHA256 != videoSHA256 {
		return conflict("EXTRACTED_VIDEO_CHANGED")
	}
	if err := p.store.Update(pid, "", "validated", map[string]any{
		"source_sha1": sourceSHA1, "source_sha256": sourceSHA256,
		"video_sha1": videoSHA1, "video_sha256": videoSHA256,
		"video_bytes": len(video), "media": media}); err != nil {
		return err
	}
	if p.cfg.Mode == "observe" {
		return p.store.Update(pid, "observed", "observe_complete", map[string]any{"error": nil})
	}
	if err := p.writing(); err != nil {
		return err
	}
	if _, err := p.assertPhotoUnchanged(ctx, pid, sourceSHA1, ""); err != nil {
		return err
	}
	vid := row.VideoID
	if vid != "" {
		if row.CreatedByUs == 0 {
			return conflict("VIDEO_PROVENANCE_UNPROVEN")
		}
		if _, err := p.validateVideo(ctx, vid, videoSHA1); err != nil {
			return err
		}
	} else {
		duplicate, err := p.api.Duplicate(ctx, videoSHA1)
		if err != nil {
			return err
		}
		if duplicate != "" {
			if err := p.store.Update(pid, "", "duplicate_unclaimed", map[string]any{"video_id": duplicate, "created_by_us": 0}); err != nil {
				return err
			}
			if row.UploadIntent != 0 {
				return conflict("UPLOAD_OUTCOME_AMBIGUOUS")
			}
			return conflict("PREEXISTING_VIDEO")
		}
		// 网络写入前落盘意图；超时或崩溃不能冒认所有权。
		if err := p.store.Update(pid, "", "upload_intent", map[string]any{"upload_intent": nowUnix()}); err != nil {
			return err
		}
		if err := p.writing(); err != nil {
			return err
		}
		installation, err := p.store.Meta("installation")
		if err != nil {
			return err
		}
		response, created, err := p.api.Upload(ctx, video, photo, installation)
		if err != nil {
			return err
		}
		vid = response.str("id")
		createdFlag := 0
		if created {
			createdFlag = 1
		}
		if err := p.store.Update(pid, "", "upload_result", map[string]any{"video_id": vid, "created_by_us": createdFlag}); err != nil {
			return err
		}
		if !created {
			return conflict("UPLOAD_DUPLICATE_NOT_OWNED")
		}
		remote, err := p.validateVideo(ctx, vid, videoSHA1)
		if err != nil {
			return err
		}
		if err := p.store.Update(pid, "", "", map[string]any{"original_path": remote.str("originalPath")}); err != nil {
			return err
		}
	}
	if err := p.waitUploadProcessing(ctx, pid, vid, videoSHA1); err != nil {
		return err
	}
	if _, err := p.assertPhotoUnchanged(ctx, pid, sourceSHA1, ""); err != nil {
		return err
	}
	// Immich 没有跨客户端 CAS；保守地再次读取，不承诺绝对竞态消除。
	if err := p.writing(); err != nil {
		return err
	}
	if err := p.api.Link(ctx, pid, vid); err != nil {
		return err
	}
	linked, err := p.api.GetAsset(ctx, pid)
	if err != nil {
		return err
	}
	if linked == nil || linked.str("livePhotoVideoId") != vid {
		return conflict("LINK_NOT_CONFIRMED")
	}
	remote, err := p.validateVideo(ctx, vid, videoSHA1)
	if err != nil {
		return err
	}
	if remote.str("visibility") != "hidden" {
		return conflict("VIDEO_NOT_HIDDEN")
	}
	return p.store.Update(pid, "linked", "link_confirmed", map[string]any{
		"error": nil, "original_path": remote.str("originalPath")})
}

// RunOne 认领并处理一条任务；处理失败落账，不向上抛出。
func (p *Processor) RunOne(ctx context.Context) (bool, error) {
	row, err := p.store.Claim()
	if err != nil || row == nil {
		return false, err
	}
	err = p.Process(ctx, row)
	if err == nil {
		return true, nil
	}
	var skipErr *SkipError
	var conflictErr *ConflictError
	var apiErr *APIError
	var bridgeErr *BridgeError
	switch {
	case errors.As(err, &skipErr):
		return true, p.store.Update(row.PhotoID, "skipped", "skipped", map[string]any{"error": skipErr.Code})
	case errors.As(err, &conflictErr):
		return true, p.store.Update(row.PhotoID, "conflict", "conflict", map[string]any{"error": conflictErr.Code})
	case errors.As(err, &apiErr):
		terminal := apiErr.Status == 400 || apiErr.Status == 401 || apiErr.Status == 403 ||
			apiErr.Status == 404 || apiErr.Status == 409 || apiErr.Status == 413 || apiErr.Status == 422
		return true, p.failure(row, apiErr.Error(), terminal)
	case errors.As(err, &bridgeErr):
		return true, p.failure(row, bridgeErr.Code, false)
	default:
		return true, p.failure(row, "UNEXPECTED_ERROR", false)
	}
}

func (p *Processor) failure(row *Job, code string, terminal bool) error {
	if row.Attempts >= p.cfg.MaxAttempts {
		terminal = true
	}
	delay := math.Min(3600, 5*math.Pow(2, math.Min(float64(row.Attempts), 10))) + rand.Float64()*3
	state := "retry"
	if terminal {
		state = "dead"
	}
	return p.store.Update(row.PhotoID, state, "failure", map[string]any{
		"error": code, "due": nowUnix() + delay})
}

// ScanTargets 扫描当前账号目标机型的未关联图片（enqueue --scan）。
func (p *Processor) ScanTargets(ctx context.Context) ([]string, error) {
	photos, err := p.api.AllPhotos(ctx, false)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, photo := range photos {
		if photo.str("ownerId") != p.owner || photo.truth("isTrashed") || photo.str("livePhotoVideoId") != "" {
			continue
		}
		exif, _ := photo["exifInfo"].(map[string]any)
		makeStr, _ := exif["make"].(string)
		model, _ := exif["model"].(string)
		if !equalFoldASCII(makeStr, "HONOR") || (p.cfg.Model != "" && model != p.cfg.Model) {
			continue
		}
		id, err := assetID(photo.str("id"))
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// Reconcile 只读对账：不换绑、不删除，保留原始来源记录。
func (p *Processor) Reconcile(ctx context.Context) ([]map[string]any, error) {
	if _, err := p.Identity(ctx); err != nil {
		return nil, err
	}
	full := p.cfg.ScanSession != ""
	photos, err := p.api.AllPhotos(ctx, full)
	if err != nil {
		return nil, err
	}
	refs := map[string][]string{}
	for _, photo := range photos {
		if photo.str("ownerId") != p.owner {
			continue
		}
		if vid := photo.str("livePhotoVideoId"); vid != "" {
			refs[vid] = append(refs[vid], photo.str("id"))
		}
	}
	rows, err := p.store.Rows()
	if err != nil {
		return nil, err
	}
	var report []map[string]any
	for _, row := range rows {
		entry := map[string]any{"photo_id": row.PhotoID, "video_id": nullString(row.VideoID)}
		switch row.State {
		case "migrated":
			photo, err := p.api.GetAsset(ctx, row.PhotoID)
			if err != nil {
				return nil, err
			}
			var native Asset
			if row.NativeVideoID != "" {
				if native, err = p.api.GetAsset(ctx, row.NativeVideoID); err != nil {
					return nil, err
				}
			}
			good := photo != nil && native != nil && photo.str("livePhotoVideoId") == row.NativeVideoID &&
				encodedPath(native) && native.str("checksum") == row.VideoSHA1
			entry["native_video_id"] = nullString(row.NativeVideoID)
			entry["status"] = map[bool]string{true: "migrated_native", false: "migration_state_changed"}[good]
		case "retired":
			var visible Asset
			if row.VideoID != "" {
				var err error
				if visible, err = p.api.GetAsset(ctx, row.VideoID); err != nil {
					return nil, err
				}
			}
			entry["status"] = map[bool]string{true: "retired_video_still_visible", false: "retired_api_unavailable_files_unverified"}[visible != nil]
		default:
			if row.VideoID == "" {
				entry["status"] = row.State
				entry["error"] = nullString(row.Error)
				break
			}
			photo, err := p.api.GetAsset(ctx, row.PhotoID)
			if err != nil {
				return nil, err
			}
			video, err := p.api.GetAsset(ctx, row.VideoID)
			if err != nil {
				return nil, err
			}
			if video != nil && video.str("ownerId") == p.owner {
				if err := p.store.Update(row.PhotoID, "", "", map[string]any{"original_path": video.str("originalPath")}); err != nil {
					return nil, err
				}
			}
			switch {
			case video == nil:
				entry["status"] = "remote_video_unavailable_possible_disk_residue"
			case photo != nil && row.SourceSHA1 != "" && photo.str("checksum") != row.SourceSHA1:
				entry["status"] = "source_checksum_changed"
			case video.str("checksum") != row.VideoSHA1:
				entry["status"] = "video_checksum_changed"
			case row.CreatedByUs == 0:
				entry["status"] = "unproven_provenance_manual_review"
			case photo != nil && photo.str("livePhotoVideoId") == row.VideoID:
				entry["status"] = "linked"
			case len(refs[row.VideoID]) > 0:
				entry["status"] = "referenced_elsewhere"
			case full:
				entry["status"] = "owned_unreferenced_candidate"
			default:
				entry["status"] = "unreferenced_in_non_locked_scope_only"
			}
			entry["reference_scope"] = map[bool]string{true: "all_including_locked", false: "non_locked_only"}[full]
			if list := refs[row.VideoID]; len(list) > 0 {
				entry["references"] = list
			} else {
				entry["references"] = []string{}
			}
		}
		report = append(report, entry)
	}
	return report, nil
}
