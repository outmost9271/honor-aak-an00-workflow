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

// TestInspectRealMedia 使用真实 HEVC/H.264 样本走完整 inspect 链路（需 ffprobe/ffmpeg）。
func TestInspectRealMedia(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not available")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	p := &Processor{cfg: Config{MaxSeconds: 30, MediaTimeout: 90, Decode: true}}
	for _, name := range []string{"test-hevc.mp4", "test-h264.mp4"} {
		photo := wrapMotionPhoto(t, filepath.Join("testdata", name))
		video, media, err := p.inspect(context.Background(), photo)
		if err != nil {
			t.Fatalf("%s inspect: %v", name, err)
		}
		mp4, _ := os.ReadFile(filepath.Join("testdata", name))
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
