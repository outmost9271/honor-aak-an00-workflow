package bridge

import (
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 环境变量契约与生产 workflow.env 保持一致。
type Config struct {
	URL                string
	APIKey             string
	WebhookSecret      string
	StateDir           string
	Mode               string // observe | apply
	Writes             bool
	Model              string
	Versions           []string
	Timeout            int // 秒
	MediaTimeout       int
	UploadReadyTimeout int
	MaxBytes           int64
	MaxSeconds         int
	MaxQueue           int
	MaxAttempts        int
	Workers            int
	Port               int
	GraceSeconds       int64
	RequireAudio       bool
	Decode             bool
	RecordMaxBytes     int
	RecordTimezone     string
	ScanSession        string
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def, min, max int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		return 0, errCode("INVALID_" + name)
	}
	return n, nil
}

// readSecret 密钥只从文件读取：至少 24 个可打印 ASCII 字符。
func readSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errCode("INVALID_SECRET_FILE")
	}
	value := strings.TrimSpace(string(data))
	if len(value) < 24 {
		return "", errCode("INVALID_SECRET_FILE")
	}
	for i := range len(value) {
		if value[i] < 33 || value[i] > 126 {
			return "", errCode("INVALID_SECRET_FILE")
		}
	}
	return value, nil
}

// canonicalURL 固定 Immich 地址，拒绝凭据、查询串与非 http(s)。
func canonicalURL(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errCode("INVALID_IMMICH_URL")
	}
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, "/api") {
		path += "/api"
	}
	u.Path, u.RawPath = path, ""
	return u.String(), nil
}

func LoadConfig() (Config, error) {
	var c Config
	var err error
	if c.URL, err = canonicalURL(os.Getenv("IMMICH_URL")); err != nil {
		return c, err
	}
	if c.APIKey, err = readSecret(envOr("IMMICH_API_KEY_FILE", "/run/secrets/immich_api_key")); err != nil {
		return c, err
	}
	if c.WebhookSecret, err = readSecret(envOr("WEBHOOK_SECRET_FILE", "/run/secrets/webhook_secret")); err != nil {
		return c, err
	}
	c.StateDir = envOr("STATE_DIR", "/state")
	c.Mode = envOr("BRIDGE_MODE", "observe")
	c.Writes = os.Getenv("IMMICH_WRITE_ENABLED") == "true"
	if (c.Mode != "observe" && c.Mode != "apply") || (c.Mode == "apply" && !c.Writes) {
		return c, errCode("APPLY_REQUIRES_WRITE_ACK")
	}
	c.Model = envOr("HONOR_MODEL", "AAK-AN00")
	c.Versions = strings.Split(envOr("ALLOWED_IMMICH_VERSIONS", "3.4.0"), ",")
	c.RecordTimezone = envOr("RECORD_TIMEZONE", "UTC")
	if _, err = time.LoadLocation(c.RecordTimezone); err != nil {
		return c, errCode("INVALID_RECORD_TIMEZONE")
	}
	steps := []struct {
		name         string
		def, min, max int
		assign       func(int)
	}{
		{"HTTP_TIMEOUT", 30, 1, 300, func(v int) { c.Timeout = v }},
		{"MEDIA_TIMEOUT", 90, 1, 300, func(v int) { c.MediaTimeout = v }},
		{"UPLOAD_READY_TIMEOUT", 180, 1, 900, func(v int) { c.UploadReadyTimeout = v }},
		{"MAX_VIDEO_SECONDS", 30, 1, 120, func(v int) { c.MaxSeconds = v }},
		{"MAX_QUEUE", 10000, 1, 1000000, func(v int) { c.MaxQueue = v }},
		{"MAX_ATTEMPTS", 8, 1, 100, func(v int) { c.MaxAttempts = v }},
		{"WORKERS", 1, 1, 4, func(v int) { c.Workers = v }},
		{"PORT", 8080, 1024, 65535, func(v int) { c.Port = v }},
		{"RECORD_MAX_BYTES", 16 * 1024 * 1024, 4096, 1024 * 1024 * 1024, func(v int) { c.RecordMaxBytes = v }},
	}
	for _, s := range steps {
		v, err := envInt(s.name, s.def, s.min, s.max)
		if err != nil {
			return c, err
		}
		s.assign(v)
	}
	maxBytes, err := envInt("MAX_IMAGE_BYTES", 128*1024*1024, 1024, 512*1024*1024)
	if err != nil {
		return c, err
	}
	c.MaxBytes = int64(maxBytes)
	grace, err := envInt("CLEANUP_GRACE_SECONDS", 86400, 0, 2592000)
	if err != nil {
		return c, err
	}
	c.GraceSeconds = int64(grace)
	c.RequireAudio = os.Getenv("REQUIRE_AUDIO") == "true"
	c.Decode = envOr("VERIFY_DECODE", "true") == "true"
	if path := os.Getenv("IMMICH_SCAN_SESSION_FILE"); path != "" {
		if c.ScanSession, err = readSecret(path); err != nil {
			return c, err
		}
	}
	return c, nil
}
