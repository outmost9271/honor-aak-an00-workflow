# honor-motion

**v0.2.1：提供可选的 Immich 容器兼容服务，修复存储模板开启时过早隐藏上传视频的时序问题。** 支持持久队列、来源台账、鉴权 Webhook、失败对账、显式退出/迁移、备份与宿主目录绑定；结构化 JSONL 记录按 UTC 日期/大小轮转。默认只观察，不写入 Immich。详见 **[容器与生产运维说明](docs/bridge.md)**、[Compose 示例](compose.example.yaml)。

以下是仍可单独使用的离线脚本：

完全离线的荣耀动态照片提取与转换工具。将带 `LIVE_` 尾标的单文件 JPEG 动态照片拆成原始 MP4、静态 JPEG 或音频，并可生成更容易播放的 H.264 视频和离线播放页面。

- Python 标准库实现，无第三方 Python 依赖。
- 原始视频直接按字节提取，不降低画质、不丢音轨。
- 多输入、递归扫描、保留子目录结构、独立输出目录。
- 默认不覆盖已有文件，保护输入照片。
- 离线提取脚本不调用网络、不上传文件、不加载远程脚本。可选容器服务仅访问显式配置的 Immich API。
- MIT 许可证。FFmpeg、libx264 等外部程序遵循各自许可证，未随仓库分发。

## 环境与运行

需要 Python 3.10+；推荐使用 [uv](https://docs.astral.sh/uv/)。仅提取原始视频、静态图片或检查文件结构时，不需要 FFmpeg。

生成兼容视频、导出音频、检查音视频轨道时，需要 PATH 中存在 `ffmpeg` 和 `ffprobe`。兼容转码要求 FFmpeg 支持 `libx264` 和 `-fps_mode`（建议 FFmpeg 6+）。

```bash
git clone https://github.com/outmost9271/honor-motion.git
cd honor-motion
uv sync
uv run python honor_motion.py --help
```

这是单文件脚本项目，不需要安装 Python 软件包。

## 使用示例

### 只无损提取视频

```bash
uv run python honor_motion.py ./photos --video-dir ./output/original
```

### 分别导出多种类型，并生成播放页面和报告

```bash
uv run python honor_motion.py ./photos -r \
  --video-dir ./output/original \
  --compatible-dir ./output/compatible \
  --image-dir ./output/images \
  --audio-dir ./output/audio \
  --html ./output/index.html \
  --report ./output/report.json \
  --require-audio --on-error continue
```

打开 `output/index.html` 即可播放；复制页面时应一起复制对应视频，保持相对目录结构。页面优先使用兼容版。原始 HEVC 视频推荐用 VLC 等支持其编码的播放器播放。

### 只生成兼容视频，调整画质、速度与尺寸

```bash
uv run python honor_motion.py ./photo.jpg \
  --compatible-dir ./output/compatible \
  --crf 20 --preset fast --max-width 1280
```

### 多个输入与试运行

```bash
uv run python honor_motion.py ./album-a ./album-b ./single.jpg -r \
  --video-dir ./output/original --dry-run
```

多个输入会分别加上 `input-1/`、`input-2/` 等前缀；重复出现的同一真实输入路径只处理一次。递归模式保留原有子目录。`.jpg` 和 `.jpeg` 同名输出发生冲突时会直接报错，不会静默覆盖。

### 只检查结构或轨道

```bash
# 不生成文件、不调用 FFmpeg；只校验尾标与 MP4 顶层盒
uv run python honor_motion.py ./photos -r --dry-run

# 使用 ffprobe 检查实际轨道，并要求存在音频
uv run python honor_motion.py ./photos -r --probe --require-audio \
  --report ./inspection.json --on-error continue
```

目录直接扫描，不自动解压 ZIP；请先自行解压。目录仅扫描 JPEG；显式指定的其他扩展名文件也不会处理。普通静态 JPEG 会报不支持，可用 `--on-error continue` 继续处理目录中的其他文件。

## 参数

| 参数 | 含义 |
| --- | --- |
| `inputs ...` | 一个或多个输入文件或目录 |
| `-r, --recursive` | 递归目录扫描 |
| `--video-dir DIR` | 无损原始 MP4 输出目录 |
| `--compatible-dir DIR` | H.264 + 原始音轨 MP4 输出目录 |
| `--image-dir DIR` | 去掉内嵌视频后的 JPEG 输出目录 |
| `--audio-dir DIR` | 原始音轨 M4A 输出目录 |
| `--html FILE` | 离线播放页面，需同时输出视频 |
| `--report FILE` | JSON 处理报告 |
| `--existing error\|skip\|overwrite` | 默认 `error`；`skip` 在任一媒体目标已存在时跳过整张照片 |
| `--on-error stop\|continue` | 默认 `stop`；继续模式最终仍返回失败状态码 |
| `--dry-run` | 校验结构和计划，不写任何文件，也不生成报告或页面 |
| `--probe` | 使用 ffprobe 校验媒体轨道 |
| `--require-audio` | 要求存在音频，隐含轨道检查 |
| `--crf 0..51` | 兼容版视频质量，默认 18，越小画质越高、文件通常越大 |
| `--preset NAME` | libx264 编码速度，默认 `medium` |
| `--max-width N` | 最大输出宽度，必须为不小于 2 的偶数；不放大 |
| `--no-autorotate` | 兼容版不把方向信息自动应用到像素 |
| `--ffmpeg PATH` | 自定义 FFmpeg 程序路径 |
| `--ffprobe PATH` | 自定义 ffprobe 程序路径 |
| `--timeout SECONDS` | 每次媒体子进程超时，默认 300 秒 |
| `--version` | 显示版本 |

至少指定一个输出目录或检查模式。默认允许无音频的视频，兼容版有音轨时原样复制；音频导出必然要求存在音轨。主要验证过 AAC 音轨，其他编码如果不能封装进 M4A/MP4，将报错而不是偷偷丢弃音频。不会自动调高音量或转成无声 GIF。

报告和播放页面是汇总文件：如果已存在，只有 `--existing overwrite` 允许覆盖；`skip` 只适用于单张照片的媒体输出。页面只收录本次成功生成的视频，不收录跳过或失败的文件。

退出码：`0` 成功（包括按配置跳过），`1` 处理或文件错误，`2` 参数错误。

## 原理与边界

支持的荣耀封装结构：

```text
JPEG 与厂商附加数据 | MP4 | 20 字节版本字段 | 20 字节时间字段 | 20 字节 LIVE_长度字段
```

```text
视频起点 = 文件总长度 - LIVE_后的数字 - 40
视频终点 = 文件总长度 - 60
```

解析器检查 JPEG 文件头、尾标、偏移边界、MP4 顶层盒长度，以及 `ftyp`、`moov`、`mdat`，支持扩展 64 位盒长度和延伸至文件末尾的零长度盒。结构检查不等于实际解码检查；可用 `--probe` 检查媒体轨道，或另用 FFmpeg 全片解码。

原始 MP4 按字节复制。兼容版通过 FFmpeg 将视频转为 H.264/yuv420p，默认应用旋转方向；通过 `-c:a copy` 复制音轨。转码画面有损，音轨不重新编码。静态图片输出是 MP4 之前的原始字节，可能仍有厂商附加数据或过时的动态照片元数据，不是重新编码或清洗后的 JPEG。

这不是所有品牌动态照片的通用解析器，也不保证支持每一个荣耀系统版本。不支持 Apple 双文件 Live Photo、HEIC 或仅使用 Google Motion Photo XMP 的其他格式。

工具按文件整体读取内存，内存需求与输入照片大小同量级，单张顺序处理。输出使用临时文件生成并逐个原子提交，**多个输出之间不是整体事务**；极端的磁盘错误可能造成该张照片部分结果已提交。应保留原始照片和备份。默认原子提交依赖输出文件系统的硬链接支持；不支持时会安全失败，可以在确认允许覆盖时使用 `--existing overwrite`（原子替换）。

## 隐私与安全

- 本仓库不包含任何真实照片、视频、手机截图、定位信息、设备序列号或本地媒体检查报告。
- 测试只在临时目录人工构造二进制夹具，集成测试用 FFmpeg 生成纯色画面与合成正弦音。
- JSON 报告仅保留基本文件名、处理状态、视频偏移与白名单轨道信息，不复制绝对源路径、EXIF/GPS 或媒体标签。**文件名本身也可能敏感，请在分享报告前检查。**
- 原始视频和静态照片输出会保留其原有数据，**可能包含拍摄时间、GPS 或设备信息**。兼容版不复制全局元数据/章节，但这不是可保证匿名化的媒体清洗工具。发布任何输出前请自行审查。
- 播放页面含输出文件名和相对视频路径，不能当作匿名化报告分享。
- `.gitignore` 防止常见媒体、压缩包、报告和密钥误提交，但不能替代人工审查，也不能阻止 `git add -f`。
- 子进程通过参数列表调用，不使用 shell 拼接；不应以高权限处理来源不明的媒体，建议保持 FFmpeg 更新。

## 测试

```bash
uv run python -m unittest discover -s tests -v
```

没有 FFmpeg 时会跳过媒体集成测试；结构解析、冲突保护、覆盖策略、试运行、递归、多输入、HTML 转义等单元测试仍可运行。

## 参考

- [荣耀官方 LivePhotoUtils 接口](https://developer.honor.com/cn/docs/11032/reference/com.hihonor.mcs.media.datacenter.livephoto/LivePhotoUtils)（仅供背景参考，本工具没有调用该 SDK）
- [第三方荣耀动态照片格式分析](https://blog.ylin314.cn/ed03bbfe818d/)（本工具为独立实现）
- [Android 动态照片格式](https://developer.android.com/media/platform/motion-photo-format?hl=zh-cn)（用于对比，不代表本工具支持该规范的所有格式）
