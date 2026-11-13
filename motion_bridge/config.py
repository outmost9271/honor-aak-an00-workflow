"""配置和密钥文件；绝不将密钥写入台账或日志。"""
from dataclasses import dataclass
import os
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError


class BridgeError(Exception):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


class Conflict(BridgeError):
    pass


class Skip(BridgeError):
    pass


def secret(path):
    value = Path(path).read_text().strip()
    if len(value) < 24 or any(not 33 <= ord(c) <= 126 for c in value):
        raise BridgeError('INVALID_SECRET_FILE')
    return value


def canonical_url(value):
    u = urlsplit(value)
    if u.scheme not in ('http', 'https') or not u.hostname or u.username or u.password or u.query or u.fragment:
        raise BridgeError('INVALID_IMMICH_URL')
    path = u.path.rstrip('/')
    if not path.endswith('/api'):
        path += '/api'
    return urlunsplit((u.scheme, u.netloc, path, '', ''))


@dataclass(frozen=True)
class Config:
    url: str
    api_key: str
    webhook_secret: str
    state_dir: Path
    mode: str = 'observe'
    writes: bool = False
    model: str = 'AAK-AN00'
    versions: tuple = ('3.4.0',)
    timeout: int = 30
    media_timeout: int = 90
    max_bytes: int = 128 * 1024 * 1024
    max_seconds: int = 30
    max_queue: int = 10000
    max_attempts: int = 8
    workers: int = 1
    port: int = 8080
    grace_seconds: int = 86400
    require_audio: bool = False
    decode: bool = True
    record_max_bytes: int = 16 * 1024 * 1024
    record_timezone: str = 'UTC'
    scan_session: str = ''

    @classmethod
    def load(cls):
        e = os.environ
        def number(name, default, low, high):
            try:
                n = int(e.get(name, str(default)))
                if not low <= n <= high:
                    raise ValueError
                return n
            except ValueError as exc:
                raise BridgeError('INVALID_' + name) from exc
        record_timezone = e.get('RECORD_TIMEZONE', 'UTC')
        try:
            ZoneInfo(record_timezone)
        except (ZoneInfoNotFoundError, ValueError):
            raise BridgeError('INVALID_RECORD_TIMEZONE') from None
        mode = e.get('BRIDGE_MODE', 'observe')
        writes = e.get('IMMICH_WRITE_ENABLED', 'false') == 'true'
        if mode not in ('observe', 'apply') or (mode == 'apply' and not writes):
            raise BridgeError('APPLY_REQUIRES_WRITE_ACK')
        return cls(
            url=canonical_url(e.get('IMMICH_URL', '')),
            api_key=secret(e.get('IMMICH_API_KEY_FILE', '/run/secrets/immich_api_key')),
            webhook_secret=secret(e.get('WEBHOOK_SECRET_FILE', '/run/secrets/webhook_secret')),
            state_dir=Path(e.get('STATE_DIR', '/state')),
            mode=mode, writes=writes, model=e.get('HONOR_MODEL', 'AAK-AN00'),
            versions=tuple(e.get('ALLOWED_IMMICH_VERSIONS', '3.4.0').split(',')),
            timeout=number('HTTP_TIMEOUT', 30, 1, 300),
            media_timeout=number('MEDIA_TIMEOUT', 90, 1, 300),
            max_bytes=number('MAX_IMAGE_BYTES', 128 * 1024 * 1024, 1024, 512 * 1024 * 1024),
            max_seconds=number('MAX_VIDEO_SECONDS', 30, 1, 120),
            max_queue=number('MAX_QUEUE', 10000, 1, 1000000),
            max_attempts=number('MAX_ATTEMPTS', 8, 1, 100),
            workers=number('WORKERS', 1, 1, 4), port=number('PORT', 8080, 1024, 65535),
            grace_seconds=number('CLEANUP_GRACE_SECONDS', 86400, 0, 2592000),
            require_audio=e.get('REQUIRE_AUDIO', 'false') == 'true',
            decode=e.get('VERIFY_DECODE', 'true') == 'true',
            record_max_bytes=number('RECORD_MAX_BYTES', 16 * 1024 * 1024, 4096, 1024 * 1024 * 1024),
            record_timezone=record_timezone,
            scan_session=secret(e['IMMICH_SCAN_SESSION_FILE']) if e.get('IMMICH_SCAN_SESSION_FILE') else '',
        )
