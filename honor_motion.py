"""本地提取荣耀 LIVE_ 尾标动态照片；不上传任何媒体。"""
import argparse
import html
import json
import os
from pathlib import Path
import re
import struct
import subprocess
import sys
import tempfile

__version__ = "0.2.0"


class MotionError(ValueError):
    """无效或不支持的动态照片。"""


def parse_motion(data):
    """返回 (视频起点, 视频终点, MP4 顶层盒名称)。不解码媒体。"""
    if not data.startswith(b"\xff\xd8"):
        raise MotionError("不是 JPEG 文件")
    marker = re.fullmatch(rb"LIVE_(\d{1,16}) *", data[-20:])
    if len(data) < 60 or marker is None:
        raise MotionError("未找到支持的荣耀 LIVE_ 尾标")
    start = len(data) - int(marker[1]) - 40
    end = len(data) - 60
    if not 2 <= start < end or data[start + 4:start + 8] != b"ftyp":
        raise MotionError("视频偏移或 MP4 文件头无效")
    pos, boxes = start, []
    while pos < end:
        if pos + 8 > end:
            raise MotionError("MP4 盒头不完整")
        size, kind = struct.unpack_from(">I4s", data, pos)
        header = 8
        if size == 1:
            if pos + 16 > end:
                raise MotionError("MP4 扩展盒头不完整")
            size = struct.unpack_from(">Q", data, pos + 8)[0]
            header = 16
        elif size == 0:
            size = end - pos
        if size < header or pos + size > end or not all(32 <= c < 127 for c in kind):
            raise MotionError("MP4 盒类型或长度无效")
        boxes.append(kind.decode("ascii"))
        pos += size
    if not {"moov", "mdat"} <= set(boxes):
        raise MotionError("缺少 MP4 索引或媒体数据")
    return start, end, boxes


def run(command, timeout):
    try:
        result = subprocess.run(command, capture_output=True, text=True, timeout=timeout, check=True)
    except FileNotFoundError as exc:
        raise RuntimeError(f"找不到程序：{command[0]}") from exc
    except subprocess.TimeoutExpired as exc:
        raise RuntimeError("媒体处理超时") from exc
    except subprocess.CalledProcessError as exc:
        # 不把 ffmpeg 的完整日志（可能含媒体标签或路径）写入报告。
        raise RuntimeError(f"媒体程序执行失败，退出码 {exc.returncode}") from exc
    return result.stdout


def probe(path, args):
    info = json.loads(run([args.ffprobe, "-v", "error", "-show_streams", "-show_format", "-of", "json", str(path)], args.timeout))
    streams = info.get("streams", [])
    if not any(s.get("codec_type") == "video" for s in streams):
        raise MotionError("没有视频轨道")
    if args.require_audio and not any(s.get("codec_type") == "audio" for s in streams):
        raise MotionError("没有音频轨道")
    keys = ("codec_type", "codec_name", "width", "height", "sample_rate", "channels", "duration")
    return {"duration": info.get("format", {}).get("duration"),
            "streams": [{k: s[k] for k in keys if k in s} for s in streams]}


def publish(temp, target, overwrite):
    """同目录临时文件原子提交；默认用硬链接避免覆盖竞争。"""
    if overwrite:
        os.replace(temp, target)
    else:
        os.link(temp, target)
        temp.unlink()


def temporary(target):
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix=".motion-", suffix=target.suffix, dir=target.parent)
    os.close(fd)
    return Path(name)


def write_result(target, content, overwrite):
    temp = temporary(target)
    try:
        temp.write_bytes(content)
        publish(temp, target, overwrite)
    finally:
        temp.unlink(missing_ok=True)


def discover(inputs, recursive):
    found = []
    seen = set()
    for number, source in enumerate(inputs, 1):
        source = source.expanduser().resolve()
        if not source.exists():
            raise MotionError(f"输入不存在：{source.name}")
        candidates = sorted(source.rglob("*") if recursive else source.iterdir()) if source.is_dir() else [source]
        for path in candidates:
            if not path.is_file() or path.suffix.lower() not in (".jpg", ".jpeg"):
                continue
            real = path.resolve()
            if real in seen:
                continue
            seen.add(real)
            relative = path.relative_to(source) if source.is_dir() else Path(source.name)
            if len(inputs) > 1:
                relative = Path(f"input-{number}") / relative
            found.append((real, relative))
    if not found:
        raise MotionError("没有找到 JPEG 输入文件")
    return found


def plan(args):
    files = discover(args.inputs, args.recursive)
    jobs, used = [], set()
    sources = {p for p, _ in files}
    for source, relative in files:
        outputs = {}
        for kind, directory, suffix in (
            ("video", args.video_dir, ".mp4"), ("compatible", args.compatible_dir, ".mp4"),
            ("image", args.image_dir, ".jpg"), ("audio", args.audio_dir, ".m4a")
        ):
            if directory:
                target = (directory.expanduser() / relative.with_suffix(suffix)).resolve()
                if target in sources:
                    raise MotionError("输出不能覆盖任何输入照片")
                if target in used:
                    raise MotionError("输出文件名冲突，请拆分输入或使用不同输出目录")
                used.add(target)
                outputs[kind] = target
        jobs.append((source, relative, outputs))
    for target in (args.report, args.html):
        if target:
            target = target.expanduser().resolve()
            if target in sources or target in used:
                raise MotionError("报告或播放页面路径与其他文件冲突")
            used.add(target)
            if target.exists() and args.existing != "overwrite":
                raise MotionError("报告或播放页面已存在；请更换路径或使用 --existing overwrite")
    return jobs


def process(source, outputs, args):
    data = source.read_bytes()
    start, end, boxes = parse_motion(data)
    result = {"offset": start, "video_bytes": end - start, "boxes": boxes}
    if args.dry_run:
        return result
    staged = []
    # 即使不导出原始视频，也需要临时 MP4 给媒体工具读取。
    with tempfile.TemporaryDirectory(prefix="honor-motion-") as work:
        raw = Path(work) / "source.mp4"
        raw.write_bytes(data[start:end])
        if args.probe or args.require_audio or args.compatible_dir or args.audio_dir:
            result["media"] = probe(raw, args)
        try:
            for kind, target in outputs.items():
                temp = temporary(target)
                staged.append((temp, target))
                if kind == "video":
                    temp.write_bytes(data[start:end])
                elif kind == "image":
                    temp.write_bytes(data[:start])
                else:
                    command = [args.ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-y"]
                    if kind == "compatible" and args.no_autorotate:
                        command += ["-noautorotate"]
                    command += ["-i", str(raw)]
                    if kind == "compatible":
                        command += ["-map", "0:v:0", "-map", "0:a:0?", "-c:v", "libx264",
                                    "-crf", str(args.crf), "-preset", args.preset, "-pix_fmt", "yuv420p",
                                    "-fps_mode", "vfr", "-c:a", "copy"]
                        if args.max_width:
                            command += ["-vf", f"scale=w='min({args.max_width},iw)':h=-2"]
                    else:
                        command += ["-map", "0:a:0", "-vn", "-c:a", "copy"]
                    command += ["-map_metadata", "-1", "-map_chapters", "-1", "-movflags", "+faststart", str(temp)]
                    run(command, args.timeout)
            # 全部生成成功后才逐个提交；不是跨多个目录的整体事务。
            for temp, target in staged:
                publish(temp, target, args.existing == "overwrite")
        finally:
            for temp, _ in staged:
                temp.unlink(missing_ok=True)
    return result


def gallery(path, videos):
    from urllib.parse import quote
    cards = []
    for video in videos:
        url = html.escape(quote(Path(os.path.relpath(video, path.parent)).as_posix()), quote=True)
        name = html.escape(video.name)
        cards.append(f'<article><h2>{name}</h2><video controls playsinline preload="metadata" src="{url}"></video><p><a href="{url}" download>下载视频</a></p></article>')
    return ('<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">'
            '<title>动态照片视频</title><style>body{font-family:system-ui;max-width:960px;margin:24px auto;padding:16px}video{width:100%;max-height:75vh}h2{font-size:16px;overflow-wrap:anywhere}</style>'
            '<h1>动态照片视频</h1><p>点击播放以听取音频。原始 HEVC 视频能否播放取决于浏览器，推荐生成兼容版。此页面不访问网络。</p>'
            + ''.join(cards) + '</html>').encode()


def build_parser():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("inputs", type=Path, nargs="+", help="一个或多个 JPEG 文件或目录")
    p.add_argument("-r", "--recursive", action="store_true", help="递归扫描目录，保留子目录结构")
    p.add_argument("--video-dir", type=Path, help="原始 MP4 输出目录（不转码）")
    p.add_argument("--compatible-dir", type=Path, help="H.264 + 原始音频 MP4 输出目录")
    p.add_argument("--image-dir", type=Path, help="移除内嵌视频后的 JPEG 输出目录")
    p.add_argument("--audio-dir", type=Path, help="原始音频 M4A 输出目录（不转码）")
    p.add_argument("--report", type=Path, help="JSON 报告路径（不包含绝对输入路径或媒体标签）")
    p.add_argument("--html", type=Path, help="离线播放页面路径（需指定视频输出目录）")
    p.add_argument("--existing", choices=("error", "skip", "overwrite"), default="error", help="已有媒体输出处理策略；skip 跳过整张照片")
    p.add_argument("--on-error", choices=("stop", "continue"), default="stop", help="单张处理失败后的策略")
    p.add_argument("--dry-run", action="store_true", help="只检查结构和输出计划；不写文件、不调用媒体工具")
    p.add_argument("--probe", action="store_true", help="用 ffprobe 检查实际媒体轨道")
    p.add_argument("--require-audio", action="store_true", help="缺少音轨视为失败（隐含轨道检查）")
    p.add_argument("--crf", type=int, default=18, help="H.264 质量 0–51，越小画质越高，默认 18")
    p.add_argument("--preset", choices=("ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow"), default="medium")
    p.add_argument("--max-width", type=int, help="兼容版最大宽度（偶数）；不放大小图")
    p.add_argument("--no-autorotate", action="store_true", help="兼容版不自动把方向应用到像素")
    p.add_argument("--ffmpeg", default="ffmpeg", help="ffmpeg 程序路径")
    p.add_argument("--ffprobe", default="ffprobe", help="ffprobe 程序路径")
    p.add_argument("--timeout", type=float, default=300, help="每次媒体子进程超时秒数，默认 300")
    p.add_argument("--version", action="version", version=__version__)
    return p


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    if not 0 <= args.crf <= 51 or args.timeout <= 0:
        parser.error("CRF 必须在 0–51 之间，超时必须大于 0")
    if args.max_width is not None and (args.max_width < 2 or args.max_width % 2):
        parser.error("最大宽度必须为不小于 2 的偶数")
    if args.html and not (args.video_dir or args.compatible_dir):
        parser.error("--html 需要 --video-dir 或 --compatible-dir")
    if not any((args.video_dir, args.compatible_dir, args.image_dir, args.audio_dir, args.probe, args.require_audio, args.dry_run)):
        parser.error("至少指定一个输出目录或检查模式")
    try:
        jobs = plan(args)
        records, videos, failures = [], [], 0
        for number, (source, relative, outputs) in enumerate(jobs, 1):
            # 报告只用序号与基本文件名；不复制 EXIF/GPS、源目录或完整 ffprobe 输出。
            record = {"id": number, "name": source.name}
            try:
                exists = any(t.exists() for t in outputs.values())
                if exists and args.existing == "skip":
                    record["status"] = "skipped"
                elif exists and args.existing == "error":
                    raise FileExistsError("媒体输出已存在")
                else:
                    record.update(process(source, outputs, args))
                    record["status"] = "planned" if args.dry_run else "ok"
                    if not args.dry_run and ("compatible" in outputs or "video" in outputs):
                        videos.append(outputs.get("compatible", outputs.get("video")))
                print(f"[{number}/{len(jobs)}] {record['status']}")
            except (OSError, ValueError, RuntimeError) as exc:
                failures += 1
                record.update(status="error", error=type(exc).__name__)
                print(f"[{number}/{len(jobs)}] 失败：{exc}", file=sys.stderr)
            records.append(record)
            if record["status"] == "error" and args.on_error == "stop":
                break
        if not args.dry_run:
            if args.report:
                write_result(args.report.expanduser().resolve(), json.dumps(records, ensure_ascii=False, indent=2).encode(), args.existing == "overwrite")
            if args.html:
                path = args.html.expanduser().resolve()
                write_result(path, gallery(path, videos), args.existing == "overwrite")
        return 1 if failures else 0
    except (OSError, ValueError, RuntimeError) as exc:
        print(f"错误：{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
