# syntax=docker/dockerfile:1
# honor-motion 单容器自包含镜像：Go 静态服务 + 裁剪静态 ffmpeg + scratch。
# ffmpeg 仅用于 MP4 轨道探测与全片解码验证（不转码），只保留所需组件。

# ---- 阶段 1：Go 静态构建 ----
FROM golang:1.25-alpine3.23 AS go
ARG GOPROXY=https://goproxy.cn,direct
RUN sed -i 's|https://mirrors.aliyun.com|https://dl-cdn.alpinelinux.org|g' /etc/apk/repositories \
    && apk add --no-cache tzdata ca-certificates \
    && mkdir -p /state /backup-export /tmp \
    && chown 10001:10001 /state /backup-export /tmp
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/honor-aak-an00-workflow ./cmd/honor-aak-an00-workflow

# ---- 阶段 2：裁剪静态 ffmpeg（MP4 解封装 + HEVC/H.264/AAC 解码 + null 输出）----
FROM alpine:3.23 AS ffmpeg
ARG FFMPEG_VERSION=8.0.1
ARG http_proxy
ARG https_proxy
RUN apk add --no-cache build-base pkgconf xz
WORKDIR /build
COPY build/ffmpeg-${FFMPEG_VERSION}.tar.xz /build/
RUN tar xf ffmpeg-${FFMPEG_VERSION}.tar.xz
WORKDIR /build/ffmpeg-${FFMPEG_VERSION}
RUN ./configure \
    --disable-everything --disable-doc --disable-debug --disable-network \
    --disable-autodetect --disable-x86asm --disable-iconv --disable-zlib \
    --enable-small --enable-static --disable-shared --extra-ldflags="-static" \
    --enable-ffmpeg --enable-ffprobe \
    --enable-demuxer=mov --enable-protocol=file,pipe \
    --enable-decoder=hevc,h264,aac --enable-parser=hevc,h264,aac \
    --enable-encoder=wrapped_avframe,pcm_s16le --enable-muxer=null \
    --enable-filter=aresample,anull,null \
    && make -j"$(nproc)" ffmpeg ffprobe \
    && strip ffmpeg ffprobe \
    && mkdir -p /out && cp ffmpeg ffprobe /out/

# ---- 阶段 3：scratch 运行 ----
FROM scratch
LABEL org.opencontainers.image.source="https://github.com/outmost9271/honor-aak-an00-workflow" \
      org.opencontainers.image.title="honor-aak-an00-workflow" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="0.3.0"
COPY --from=go /out/honor-aak-an00-workflow /usr/local/bin/honor-aak-an00-workflow
COPY --from=ffmpeg /out/ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg /out/ffprobe /usr/local/bin/ffprobe
COPY --from=go /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=go /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=go /state /state
COPY --from=go /backup-export /backup-export
COPY --from=go /tmp /tmp
ENV HOME=/tmp STATE_DIR=/state BACKUP_EXPORT_DIR=/backup-export PATH=/usr/local/bin
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/usr/local/bin/honor-aak-an00-workflow", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/honor-aak-an00-workflow"]
CMD ["serve"]
