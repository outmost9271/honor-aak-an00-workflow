package bridge

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Asset 是 Immich 资源 JSON 的松散表示；只读取已知字段。
type Asset map[string]any

func (a Asset) str(key string) string {
	v, _ := a[key].(string)
	return v
}

func (a Asset) truth(key string) bool {
	v, _ := a[key].(bool)
	return v
}

// assetID 校验 UUID 形态，拒绝把任意字符串拼进 API 路径。
func assetID(value string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if len(v) != 36 {
		return "", errCode("INVALID_ASSET_ID")
	}
	for i, c := range v {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return "", errCode("INVALID_ASSET_ID")
			}
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'):
		default:
			return "", errCode("INVALID_ASSET_ID")
		}
	}
	return v, nil
}

func sha1Base64(data []byte) string {
	sum := sha1.Sum(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Client Immich v3.4 API。禁用代理与重定向，限制响应大小，写请求不自动重试。
type Client struct {
	cfg Config
	hc  *http.Client
}

func NewClient(cfg Config) *Client {
	return &Client{cfg: cfg, hc: &http.Client{
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

type apiRequest struct {
	method  string
	path    string
	body    any // map/slice → JSON；[]byte → 原样；nil 无请求体
	headers map[string]string
	binary  bool
	limit   int64
	scan    bool // 使用提升会话（仅只读白名单）
}

func (c *Client) call(ctx context.Context, r apiRequest) (any, error) {
	if !strings.HasPrefix(r.path, "/") || strings.HasPrefix(r.path, "//") {
		return nil, errCode("INVALID_API_PATH")
	}
	var raw []byte
	switch b := r.body.(type) {
	case nil:
	case []byte:
		raw = b
	default:
		var err error
		if raw, err = json.Marshal(b); err != nil {
			return nil, errCode("API_INVALID_RESPONSE")
		}
		if r.headers == nil {
			r.headers = map[string]string{}
		}
		r.headers["Content-Type"] = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.cfg.URL+r.path, bytes.NewReader(raw))
	if err != nil {
		return nil, errCode("INVALID_API_PATH")
	}
	req.Header.Set("Accept", "application/json")
	if r.scan {
		if c.cfg.ScanSession == "" {
			return nil, errCode("FULL_SCAN_REQUIRES_ELEVATED_SESSION")
		}
		allowed := (r.method == "GET" && (r.path == "/users/me" || r.path == "/auth/status")) ||
			(r.method == "POST" && r.path == "/search/metadata")
		if !allowed {
			return nil, errCode("SCAN_SESSION_READ_ONLY")
		}
		req.Header.Set("Authorization", "Bearer "+c.cfg.ScanSession)
	} else {
		req.Header.Set("x-api-key", c.cfg.APIKey)
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	maximum := r.limit
	if maximum == 0 {
		maximum = 16 * 1024 * 1024
		if r.binary {
			maximum = c.cfg.MaxBytes
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, errCode("API_NETWORK_ERROR")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 只识别上游固定错误消息，不把响应体写入日志。
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 16384))
		var parsed struct{ Message string }
		_ = json.Unmarshal(data, &parsed)
		missing := resp.StatusCode == 400 &&
			(parsed.Message == "Asset not found" || parsed.Message == "Not found or no asset.read access")
		return nil, &APIError{Status: resp.StatusCode, MissingOrDenied: missing}
	}
	if resp.ContentLength > maximum {
		return nil, errCode("RESPONSE_TOO_LARGE")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maximum+1))
	if err != nil {
		return nil, errCode("API_NETWORK_ERROR")
	}
	if int64(len(data)) > maximum {
		return nil, errCode("RESPONSE_TOO_LARGE")
	}
	if r.binary {
		return data, nil
	}
	if len(data) == 0 {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, errCode("API_INVALID_RESPONSE")
	}
	return value, nil
}

// jsonCall 发起 JSON 请求并期待对象响应。
func (c *Client) jsonCall(ctx context.Context, method, path string, body any, scan bool) (map[string]any, error) {
	v, err := c.call(ctx, apiRequest{method: method, path: path, body: body, scan: scan})
	if err != nil || v == nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errCode("API_INVALID_RESPONSE")
	}
	return m, nil
}

// GetAsset 不存在或无权限时返回 (nil, nil)；提升会话下可查锁定资源。
func (c *Client) GetAsset(ctx context.Context, id string) (Asset, error) {
	uid, err := assetID(id)
	if err != nil {
		return nil, err
	}
	m, err := c.jsonCall(ctx, "GET", "/assets/"+uid, nil, false)
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Status == 404 || apiErr.MissingOrDenied) {
		if c.cfg.ScanSession != "" {
			if err := c.RequireFullScan(ctx); err != nil {
				return nil, err
			}
			items, err := c.search(ctx, map[string]any{
				"id":         map[string]any{"eq": uid},
				"visibility": map[string]any{"in": []string{"timeline", "archive", "hidden", "locked"}},
				"or": []any{map[string]any{"trashedAt": map[string]any{"eq": nil}},
					map[string]any{"trashedAt": map[string]any{"ne": nil}}},
			}, true)
			if err != nil || len(items) == 0 {
				return nil, err
			}
			return items[0], nil
		}
		return nil, nil
	}
	return m, err
}

func (c *Client) Original(ctx context.Context, id string) ([]byte, error) {
	uid, err := assetID(id)
	if err != nil {
		return nil, err
	}
	data, err := c.call(ctx, apiRequest{method: "GET", path: "/assets/" + uid + "/original", binary: true})
	if err != nil {
		return nil, err
	}
	return data.([]byte), nil
}

func (c *Client) Link(ctx context.Context, photoID, videoID string) error {
	uid, err := assetID(photoID)
	if err != nil {
		return err
	}
	var link any
	if videoID != "" {
		if link, err = assetID(videoID); err != nil {
			return err
		}
	}
	_, err = c.jsonCall(ctx, "PUT", "/assets/"+uid, map[string]any{"livePhotoVideoId": link}, false)
	return err
}

func (c *Client) Delete(ctx context.Context, videoID string) error {
	uid, err := assetID(videoID)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, apiRequest{method: "DELETE", path: "/assets",
		body: map[string]any{"ids": []string{uid}, "force": true}})
	return err
}

func (c *Client) RefreshMetadata(ctx context.Context, photoID string) error {
	uid, err := assetID(photoID)
	if err != nil {
		return err
	}
	_, err = c.jsonCall(ctx, "POST", "/assets/jobs", map[string]any{
		"assetIds": []string{uid}, "name": "refresh-metadata"}, false)
	return err
}

// Duplicate 上传前去重；返回已存在的同校验和资源 ID。
func (c *Client) Duplicate(ctx context.Context, sha1 string) (string, error) {
	m, err := c.jsonCall(ctx, "POST", "/assets/bulk-upload-check",
		map[string]any{"assets": []any{map[string]any{"id": "motion", "checksum": sha1}}}, false)
	if err != nil {
		return "", err
	}
	entries, ok := m["results"].([]any)
	if !ok || len(entries) != 1 {
		return "", errCode("INVALID_DUPLICATE_RESPONSE")
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		return "", errCode("INVALID_DUPLICATE_RESPONSE")
	}
	if entry["action"] == "accept" {
		return "", nil
	}
	if entry["reason"] == "duplicate" && entry["assetId"] != nil {
		return assetID(fmt.Sprint(entry["assetId"]))
	}
	return "", errCode("DUPLICATE_CHECK_REJECTED")
}

// Upload 手工构造 multipart，字段与既有实现一致，禁止元数据注入。
func (c *Client) Upload(ctx context.Context, data []byte, photo Asset, installation string) (Asset, bool, error) {
	photoID, err := assetID(photo.str("id"))
	if err != nil {
		return nil, false, err
	}
	boundary := "motion-" + newUUID()
	name := "honor-aak-an00-workflow-" + photoID + "-" + sha256Hex(data)[:16] + ".mp4"
	fields := [][2]string{
		{"deviceAssetId", name},
		{"deviceId", "honor-aak-an00-workflow-" + installation},
		{"fileCreatedAt", photo.str("fileCreatedAt")},
		{"fileModifiedAt", photo.str("fileModifiedAt")},
	}
	var buf bytes.Buffer
	for _, f := range fields {
		if f[1] == "" || strings.ContainsAny(f[1], "\r\n") {
			return nil, false, errCode("INVALID_UPLOAD_METADATA")
		}
		fmt.Fprintf(&buf, "--%s\r\nContent-Disposition: form-data; name=\"%s\"\r\n\r\n%s\r\n", boundary, f[0], f[1])
	}
	fmt.Fprintf(&buf, "--%s\r\nContent-Disposition: form-data; name=\"assetData\"; filename=\"%s\"\r\nContent-Type: video/mp4\r\n\r\n", boundary, name)
	buf.Write(data)
	fmt.Fprintf(&buf, "\r\n--%s--\r\n", boundary)
	v, err := c.call(ctx, apiRequest{method: "POST", path: "/assets", body: buf.Bytes(),
		headers: map[string]string{"Content-Type": "multipart/form-data; boundary=" + boundary}})
	if err != nil {
		return nil, false, err
	}
	result, ok := v.(map[string]any)
	if !ok {
		return nil, false, errCode("API_INVALID_RESPONSE")
	}
	if _, err := assetID(fmt.Sprint(result["id"])); err != nil {
		return nil, false, err
	}
	return Asset(result), result["status"] == "created", nil
}

// search 元数据搜索分页，检测游标循环与异常分页形态。
func (c *Client) search(ctx context.Context, filter map[string]any, scan bool) ([]Asset, error) {
	var out []Asset
	cursor := ""
	seen := map[string]bool{}
	for range 100000 {
		payload := map[string]any{"filter": filter, "withExif": true, "withStacked": true, "size": 250}
		if cursor != "" {
			payload["cursor"] = cursor
		}
		m, err := c.jsonCall(ctx, "POST", "/search/metadata", payload, scan)
		if err != nil {
			return nil, err
		}
		assets, _ := m["assets"].(map[string]any)
		items, ok := assets["items"].([]any)
		if !ok {
			return nil, errCode("SEARCH_INCOMPLETE")
		}
		for _, item := range items {
			if a, ok := item.(map[string]any); ok {
				out = append(out, Asset(a))
			}
		}
		next, _ := assets["nextCursor"].(string)
		if next == "" {
			if p, ok := assets["nextPage"]; ok && p != nil && p != false && fmt.Sprint(p) != "0" {
				return nil, errCode("SEARCH_CURSOR_UNSUPPORTED")
			}
			return out, nil
		}
		if seen[next] {
			return nil, errCode("SEARCH_CURSOR_LOOP")
		}
		seen[next] = true
		cursor = next
	}
	return nil, errCode("SEARCH_TOO_LARGE")
}

// RequireFullScan 提升会话必须属于同一账号且已提权。
func (c *Client) RequireFullScan(ctx context.Context) error {
	status, err := c.jsonCall(ctx, "GET", "/auth/status", nil, true)
	if err != nil {
		return err
	}
	elevated, _ := status["isElevated"].(bool)
	owner, err := c.jsonCall(ctx, "GET", "/users/me", nil, false)
	if err != nil {
		return err
	}
	scanner, err := c.jsonCall(ctx, "GET", "/users/me", nil, true)
	if err != nil {
		return err
	}
	if !elevated || fmt.Sprint(owner["id"]) != fmt.Sprint(scanner["id"]) {
		return errCode("FULL_SCAN_SESSION_NOT_ELEVATED_OR_WRONG_OWNER")
	}
	return nil
}

// AllPhotos 全量图片（可选含锁定）；维护删除必须显式完整扫描。
func (c *Client) AllPhotos(ctx context.Context, full bool) ([]Asset, error) {
	visibility := []string{"timeline", "archive", "hidden"}
	if full {
		if err := c.RequireFullScan(ctx); err != nil {
			return nil, err
		}
		visibility = append(visibility, "locked")
	}
	return c.search(ctx, map[string]any{
		"type":       map[string]any{"eq": "IMAGE"},
		"visibility": map[string]any{"in": visibility},
		"or": []any{map[string]any{"trashedAt": map[string]any{"eq": nil}},
			map[string]any{"trashedAt": map[string]any{"ne": nil}}},
	}, full)
}
