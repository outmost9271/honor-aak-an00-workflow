package bridge

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// wrapMotionPhoto 用真实 MP4 构造 HONOR 动态照片（LIVE_ 尾标 = 视频长度 + 20）。
func wrapMotionPhoto(t *testing.T, mp4Path string) []byte {
	t.Helper()
	mp4, err := os.ReadFile(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	// 修正 mdat 之外的盒结构无需变化；直接拼接。
	photo := []byte{0xff, 0xd8}
	photo = append(photo, make([]byte, 64)...)
	photo = append(photo, mp4...)
	photo = append(photo, make([]byte, 40)...)
	tail := "LIVE_" + itoa(len(mp4)+20)
	for len(tail) < 20 {
		tail += " "
	}
	photo = append(photo, tail...)
	// 自检：解析范围必须指向 MP4 起点。
	s, e, _, err := ParseMotion(photo)
	if err != nil {
		t.Fatalf("wrap self-check: %v", err)
	}
	if e-s != len(mp4) {
		t.Fatal("wrap self-check range")
	}
	return photo
}

// ensureFixture 按需生成测试媒体（与 CI 惯例一致：fixtures 不入库，ffmpeg 现场生成）。
func ensureFixture(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	codec := map[string]string{"test-hevc.mp4": "libx265", "test-h264.mp4": "libx264"}[name]
	if codec == "" {
		t.Fatalf("unknown fixture %s", name)
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=3:size=320x240:rate=15",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:v", codec, "-c:a", "aac", "-shortest", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture %s: %v\n%s", name, err, out)
	}
	return path
}

// TestInspectRealMedia 使用真实 HEVC/H.264 样本走完整 inspect 链路（需 ffprobe/ffmpeg）。
func TestInspectRealMedia(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not available")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	p := &Processor{cfg: Config{MaxSeconds: 30, MediaTimeout: 90, Decode: true}}
	dir := t.TempDir()
	for _, name := range []string{"test-hevc.mp4", "test-h264.mp4"} {
		fixture := ensureFixture(t, dir, name)
		photo := wrapMotionPhoto(t, fixture)
		video, media, err := p.inspect(context.Background(), photo)
		if err != nil {
			t.Fatalf("%s inspect: %v", name, err)
		}
		mp4, _ := os.ReadFile(fixture)
		if len(video) != len(mp4) || sha256Hex(video) != sha256Hex(mp4) {
			t.Fatalf("%s extracted video not lossless", name)
		}
		var parsed struct {
			Duration float64 `json:"duration"`
			Streams  []struct {
				CodecType string `json:"codec_type"`
				CodecName string `json:"codec_name"`
				Width     int    `json:"width"`
				Height    int    `json:"height"`
			} `json:"streams"`
		}
		if err := json.Unmarshal([]byte(media), &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.Duration <= 0 || parsed.Duration > 30 {
			t.Fatalf("%s duration %v", name, parsed.Duration)
		}
		foundVideo, foundAudio := false, false
		for _, s := range parsed.Streams {
			if s.CodecType == "video" {
				foundVideo = true
				if s.Width != 320 || s.Height != 240 {
					t.Fatalf("%s dimensions %dx%d", name, s.Width, s.Height)
				}
			}
			if s.CodecType == "audio" {
				foundAudio = true
			}
		}
		if !foundVideo || !foundAudio {
			t.Fatalf("%s streams %+v", name, parsed.Streams)
		}
	}
}

// TestInspectRejectsPlainJPEG 无 LIVE 尾标照片走 skip 分支。
func TestInspectRejectsPlainJPEG(t *testing.T) {
	p := &Processor{cfg: Config{MaxSeconds: 30, MediaTimeout: 90, Decode: true}}
	_, _, err := p.inspect(context.Background(), []byte{0xff, 0xd8, 0x00, 0x01})
	if err == nil {
		t.Fatal("expected skip")
	}
	if _, ok := err.(*SkipError); !ok {
		t.Fatalf("expected SkipError, got %T %v", err, err)
	}
}
