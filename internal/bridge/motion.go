package bridge

import (
	"encoding/binary"
	"regexp"
	"strconv"
)

// MotionError 无效或不支持的动态照片（Python MotionError）。
type MotionError struct{ msg string }

func (e *MotionError) Error() string { return e.msg }

func motionErr(msg string) *MotionError { return &MotionError{msg: msg} }

var liveMarker = regexp.MustCompile(`^LIVE_(\d{1,16}) *$`)

// ParseMotion 返回视频起点、终点、MP4 顶层盒名称；不解码媒体。
// 与 honor_motion.py parse_motion 逐条校验一致。
func ParseMotion(data []byte) (int, int, []string, error) {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return 0, 0, nil, motionErr("不是 JPEG 文件")
	}
	tail := data
	if len(tail) > 20 {
		tail = tail[len(tail)-20:]
	}
	marker := liveMarker.FindSubmatch(tail)
	if len(data) < 60 || marker == nil {
		return 0, 0, nil, motionErr("未找到支持的荣耀 LIVE_ 尾标")
	}
	size, err := strconv.Atoi(string(marker[1]))
	if err != nil {
		return 0, 0, nil, motionErr("未找到支持的荣耀 LIVE_ 尾标")
	}
	start := len(data) - size - 40
	end := len(data) - 60
	if start < 2 || start >= end || end+8 > len(data) || string(data[start+4:start+8]) != "ftyp" {
		return 0, 0, nil, motionErr("视频偏移或 MP4 文件头无效")
	}
	pos := start
	var boxes []string
	for pos < end {
		if pos+8 > end {
			return 0, 0, nil, motionErr("MP4 盒头不完整")
		}
		boxSize := uint64(binary.BigEndian.Uint32(data[pos:]))
		kind := data[pos+4 : pos+8]
		header := uint64(8)
		if boxSize == 1 {
			if pos+16 > end {
				return 0, 0, nil, motionErr("MP4 扩展盒头不完整")
			}
			boxSize = binary.BigEndian.Uint64(data[pos+8:])
			header = 16
		} else if boxSize == 0 {
			boxSize = uint64(end - pos)
		}
		for _, c := range kind {
			if c < 32 || c >= 127 {
				return 0, 0, nil, motionErr("MP4 盒类型或长度无效")
			}
		}
		if boxSize < header || boxSize > uint64(end-pos) {
			return 0, 0, nil, motionErr("MP4 盒类型或长度无效")
		}
		boxes = append(boxes, string(kind))
		pos += int(boxSize)
	}
	hasMoov, hasMdat := false, false
	for _, b := range boxes {
		if b == "moov" {
			hasMoov = true
		} else if b == "mdat" {
			hasMdat = true
		}
	}
	if !hasMoov || !hasMdat {
		return 0, 0, nil, motionErr("缺少 MP4 索引或媒体数据")
	}
	return start, end, boxes, nil
}
