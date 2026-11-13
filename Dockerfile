# syntax=docker/dockerfile:1
FROM ghcr.io/astral-sh/uv:0.12.3@sha256:2d890623d310b57771ce840f0da5eed5fc6d657da05ffaa45d82797b53fa3abc AS uv
FROM python:3.13-slim-bookworm@sha256:a5a7017efc5969e00ae2b91c8eb0631049a569652aa4ffc4501769049ae45c72
LABEL org.opencontainers.image.source="https://github.com/outmost9271/honor-motion" \
      org.opencontainers.image.title="honor-motion" \
      org.opencontainers.image.licenses="MIT"
COPY --from=uv /uv /usr/local/bin/uv
RUN sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ffmpeg ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 motion \
    && useradd --uid 10001 --gid motion --no-create-home --home-dir /tmp motion \
    && mkdir -p /state /app \
    && chown 10001:10001 /state
WORKDIR /app
ENV UV_PYTHON_DOWNLOADS=never UV_NO_CACHE=1 UV_PROJECT_ENVIRONMENT=/app/.venv \
    PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 HOME=/tmp STATE_DIR=/state
COPY pyproject.toml uv.lock README.md LICENSE NOTICE.md honor_motion.py ./
COPY motion_bridge/ ./motion_bridge/
RUN uv sync --frozen --no-dev --no-managed-python
LABEL org.opencontainers.image.version="0.2.0"
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["uv", "run", "--frozen", "--no-sync", "python", "-m", "motion_bridge", "healthcheck"]
ENTRYPOINT ["uv", "run", "--frozen", "--no-sync", "python", "-m", "motion_bridge"]
CMD ["serve"]
