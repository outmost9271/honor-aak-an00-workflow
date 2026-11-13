# 第三方软件说明

本项目自行编写的 Python 源码和文档采用 MIT 许可证。该许可证不覆盖基础镜像、Python、uv、FFmpeg 及其链接库。

容器使用固定摘要的官方 Python 和 uv 基础组件，并从 Debian 的官方软件仓库安装 FFmpeg、证书和时区数据。镜像保留 Debian 软件包的版权/许可证文件，可在 `/usr/share/doc/<package>/copyright` 查看；实际安装版本可使用 `dpkg-query -W` 查询。

- Python：Python Software Foundation License，https://www.python.org/downloads/source/
- uv：MIT / Apache-2.0，https://github.com/astral-sh/uv
- FFmpeg：许可证随编译选项及链接库变化，Debian 包可能包含 GPL 组件。https://ffmpeg.org/legal.html
- Debian 对应源包与补丁：https://sources.debian.org/ 、https://deb.debian.org/debian/pool/
- IANA 时区数据库：https://www.iana.org/time-zones

如果自行再分发构建后的镜像或二进制，应审查实际构建的组件及许可证，并履行相应版权声明、对应源码提供等要求。不能将整个镜像误称为 MIT 许可证的软件。当前仓库的自动化只构建和测试镜像，不会自动发布二进制镜像或部署服务。
