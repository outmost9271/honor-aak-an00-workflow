package bridge

import (
	"encoding/binary"
	"testing"
)

func mp4Box(kind string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(len(out)))
	copy(out[4:8], kind)
	copy(out[8:], payload)
	return out
}

func makePhoto(t *testing.T) []byte {
	t.Helper()
	mp4 := append(mp4Box("ftyp", []byte("isom0000")), mp4Box("moov", nil)...)
	mp4 = append(mp4, mp4Box("mdat", []byte("video"))...)
	photo := []byte{0xff, 0xd8}
	photo = append(photo, make([]byte, 100)...)
	photo = append(photo, mp4...)
	photo = append(photo, make([]byte, 40)...)
	tail := "LIVE_" + itoa(len(mp4)+20) // 尾标数值 = 视频字节数 + 20
	for len(tail) < 20 {
		tail += " "
	}
	return append(photo, tail...)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestParseMotion(t *testing.T) {
	photo := makePhoto(t)
	start, end, boxes, err := ParseMotion(photo)
	if err != nil {
		t.Fatalf("ParseMotion: %v", err)
	}
	if start != 102 || end != len(photo)-60 {
		t.Fatalf("unexpected range %d %d", start, end)
	}
	if len(boxes) != 3 || boxes[0] != "ftyp" || boxes[1] != "moov" || boxes[2] != "mdat" {
		t.Fatalf("unexpected boxes %v", boxes)
	}
	video := photo[start:end]
	if len(video) < 8 || string(video[4:8]) != "ftyp" {
		t.Fatal("video slice invalid")
	}
}

func TestParseMotionRejects(t *testing.T) {
	if _, _, _, err := ParseMotion([]byte("not-a-jpeg")); err == nil {
		t.Fatal("expected error for non-JPEG")
	}
	photo := makePhoto(t)
	truncated := photo[:len(photo)-20] // 去掉 LIVE 尾标
	if _, _, _, err := ParseMotion(truncated); err == nil {
		t.Fatal("expected error for missing trailer")
	}
	// 缺 moov
	mp4 := append(mp4Box("ftyp", []byte("isom0000")), mp4Box("mdat", nil)...)
	data := []byte{0xff, 0xd8}
	data = append(data, make([]byte, 100)...)
	data = append(data, mp4...)
	data = append(data, make([]byte, 40)...)
	tail := "LIVE_" + itoa(len(mp4)+20)
	for len(tail) < 20 {
		tail += " "
	}
	data = append(data, tail...)
	if _, _, _, err := ParseMotion(data); err == nil {
		t.Fatal("expected error for missing moov")
	}
	// 盒长度损坏
	broken := makePhoto(t)
	broken[102] = 0xff
	broken[103] = 0xff
	if _, _, _, err := ParseMotion(broken); err == nil {
		t.Fatal("expected error for invalid box size")
	}
}
