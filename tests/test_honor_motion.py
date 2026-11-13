"""所有夹具均人工构造；不使用真实照片、手机标识或定位信息。"""
import contextlib
import io
import json
from pathlib import Path
import shutil
import struct
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import honor_motion as hm


def box(kind, payload=b"", extended=False):
    if extended:
        return struct.pack(">I4sQ", 1, kind, len(payload) + 16) + payload
    return struct.pack(">I4s", len(payload) + 8, kind) + payload


def photo(video=None):
    if video is None:
        video = box(b"ftyp", b"isom0000") + box(b"moov") + box(b"mdat", b"synthetic", True)
    return (b"\xff\xd8synthetic JPEG\xff\xd9" + video + b"v2_f0".ljust(20)
            + b"0:1000".ljust(20) + f"LIVE_{len(video) + 20}".encode().ljust(20))


class ParserTests(unittest.TestCase):
    def test_extract_and_extended_box(self):
        data = photo()
        start, end, boxes = hm.parse_motion(data)
        self.assertEqual(boxes, ["ftyp", "moov", "mdat"])
        self.assertEqual(data[start + 4:start + 8], b"ftyp")
        self.assertEqual(end, len(data) - 60)

    def test_zero_size_box(self):
        video = box(b"ftyp") + box(b"moov") + struct.pack(">I4s", 0, b"mdat") + b"payload"
        self.assertEqual(hm.parse_motion(photo(video))[2][-1], "mdat")

    def test_invalid_inputs(self):
        for data in (b"", b"\xff\xd8still\xff\xd9", photo()[1:], photo()[:-1],
                     photo()[:-20] + b"LIVE_999999999".ljust(20),
                     photo(box(b"ftyp") + box(b"mdat")),
                     photo(box(b"ftyp") + box(b"moov") + struct.pack(">I4s", 1000, b"mdat")),
                     photo(box(b"ftyp") + box(b"moov") + struct.pack(">I4s", 1, b"mdat")),
                     photo(box(b"ftyp") + box(b"moov") + b"tail")):
            with self.subTest(data=data[-30:]), self.assertRaises(hm.MotionError):
                hm.parse_motion(data)


class CLITests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.source = self.root / "input" / "sample.jpg"
        self.source.parent.mkdir()
        self.source.write_bytes(photo())
        self.out = self.root / "out"

    def tearDown(self):
        self.temp.cleanup()

    def call(self, *args):
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            return hm.main([str(a) for a in args])

    def test_lossless_and_image(self):
        before = self.source.read_bytes()
        report = self.root / "report.json"
        self.assertEqual(self.call(self.source, "--video-dir", self.out, "--image-dir", self.root / "images", "--report", report), 0)
        a, b, _ = hm.parse_motion(before)
        self.assertEqual((self.out / "sample.mp4").read_bytes(), before[a:b])
        self.assertEqual((self.root / "images/sample.jpg").read_bytes(), before[:a])
        self.assertEqual(self.source.read_bytes(), before)
        text = report.read_text()
        self.assertNotIn(str(self.root), text)
        self.assertEqual(json.loads(text)[0]["status"], "ok")

    def test_dry_run_no_writes(self):
        self.assertEqual(self.call(self.source, "--video-dir", self.out, "--dry-run", "--report", self.root / "report.json", "--ffprobe", "nonexistent"), 0)
        self.assertFalse(self.out.exists())
        self.assertFalse((self.root / "report.json").exists())

    def test_existing_policies(self):
        self.assertEqual(self.call(self.source, "--video-dir", self.out), 0)
        target = self.out / "sample.mp4"
        target.write_bytes(b"sentinel")
        self.assertEqual(self.call(self.source, "--video-dir", self.out), 1)
        self.assertEqual(self.call(self.source, "--video-dir", self.out, "--existing", "skip"), 0)
        self.assertEqual(target.read_bytes(), b"sentinel")
        self.assertEqual(self.call(self.source, "--video-dir", self.out, "--existing", "overwrite"), 0)
        self.assertNotEqual(target.read_bytes(), b"sentinel")

    def test_input_protected(self):
        self.assertEqual(self.call(self.source, "--image-dir", self.source.parent, "--existing", "overwrite"), 1)
        self.assertEqual(self.source.read_bytes(), photo())

    def test_collision(self):
        self.source.with_suffix(".jpeg").write_bytes(photo())
        self.assertEqual(self.call(self.source.parent, "--video-dir", self.out), 1)
        self.assertFalse(self.out.exists())

    def test_output_types_cannot_collide(self):
        self.assertEqual(self.call(self.source, "--video-dir", self.out, "--compatible-dir", self.out), 1)

    def test_recursive_and_multiple_inputs(self):
        sub = self.source.parent / "nested"
        sub.mkdir()
        (sub / "other.JPG").write_bytes(photo())
        second = self.root / "second.jpg"
        second.write_bytes(photo())
        self.assertEqual(self.call(self.source.parent, second, "-r", "--video-dir", self.out), 0)
        self.assertTrue((self.out / "input-1/nested/other.mp4").exists())
        self.assertTrue((self.out / "input-2/second.mp4").exists())

    def test_continue(self):
        (self.source.parent / "bad.jpg").write_bytes(b"invalid")
        self.assertEqual(self.call(self.source.parent, "--video-dir", self.out, "--on-error", "continue"), 1)
        self.assertTrue((self.out / "sample.mp4").exists())

    def test_report_collision(self):
        self.assertEqual(self.call(self.source, "--video-dir", self.out, "--report", self.out / "sample.mp4"), 1)

    def test_html_escapes_and_urls(self):
        page = hm.gallery(self.root / "index.html", [self.root / 'a&b #.mp4']).decode()
        self.assertIn('a%26b%20%23.mp4', page)
        self.assertIn('a&amp;b #.mp4', page)
        self.assertNotIn('https://', page)

    def test_atomic_no_overwrite(self):
        target = self.root / "result"
        target.write_bytes(b"old")
        with self.assertRaises(FileExistsError):
            hm.write_result(target, b"new", False)
        self.assertEqual(target.read_bytes(), b"old")
        self.assertEqual(list(self.root.glob(".motion-*")), [])

    def test_failed_conversion_commits_nothing(self):
        with patch.object(hm, "probe", return_value={}), patch.object(hm, "run", side_effect=RuntimeError("合成失败")):
            self.assertEqual(self.call(self.source, "--video-dir", self.out, "--compatible-dir", self.root / "compat"), 1)
        self.assertFalse((self.out / "sample.mp4").exists())
        self.assertEqual(list(self.root.rglob(".motion-*")), [])

    def test_invalid_arguments(self):
        for args in (("--crf", "52"), ("--timeout", "0"), ("--max-width", "3"), ("--html", "x.html")):
            with self.subTest(args=args), self.assertRaises(SystemExit):
                self.call(self.source, "--dry-run", *args)


@unittest.skipUnless(shutil.which("ffmpeg") and shutil.which("ffprobe"), "需要 FFmpeg")
class MediaTests(unittest.TestCase):
    setUp = CLITests.setUp
    tearDown = CLITests.tearDown
    call = CLITests.call

    def test_generated_media(self):
        raw = self.root / "generated.mp4"
        result = subprocess.run(["ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=160x120:r=10:d=0.5",
                                 "-f", "lavfi", "-i", "sine=frequency=440:duration=0.5", "-c:v", "libx264", "-c:a", "aac", "-shortest", str(raw)], capture_output=True)
        if result.returncode:
            self.fail("FFmpeg 合成媒体失败：" + result.stderr.decode())
        self.source.write_bytes(photo(raw.read_bytes()))
        compatible, audio = self.root / "compatible", self.root / "audio"
        self.assertEqual(self.call(self.source, "--video-dir", self.out, "--compatible-dir", compatible,
                                   "--audio-dir", audio, "--require-audio", "--max-width", "80", "--html", self.root / "index.html"), 0)
        def audio_bytes(path):
            return subprocess.check_output(["ffmpeg", "-v", "error", "-i", str(path), "-map", "0:a:0", "-c:a", "copy", "-f", "adts", "-"])
        self.assertEqual(audio_bytes(raw), audio_bytes(compatible / "sample.mp4"))
        self.assertEqual(audio_bytes(raw), audio_bytes(audio / "sample.m4a"))
        info = json.loads(subprocess.check_output(["ffprobe", "-v", "error", "-show_streams", "-of", "json", str(compatible / "sample.mp4")]))
        self.assertEqual(next(s for s in info["streams"] if s["codec_type"] == "video")["width"], 80)
        silent = self.root / "silent.mp4"
        subprocess.run(["ffmpeg", "-v", "error", "-i", str(raw), "-an", "-c:v", "copy", str(silent)], check=True)
        self.source.write_bytes(photo(silent.read_bytes()))
        self.assertEqual(self.call(self.source, "--compatible-dir", self.root / "silent-ok"), 0)
        self.assertEqual(self.call(self.source, "--video-dir", self.root / "silent-error", "--require-audio"), 1)
        self.assertFalse((self.root / "silent-error/sample.mp4").exists())


if __name__ == "__main__":
    unittest.main()
