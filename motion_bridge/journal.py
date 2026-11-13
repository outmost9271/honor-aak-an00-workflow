"""事务 outbox → 按 UTC 日期/大小轮转的持久 JSONL。"""
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import threading
import time
from zoneinfo import ZoneInfo

from .config import BridgeError


class Journal:
    def __init__(self, store, max_bytes=16 * 1024 * 1024, clock=time.time, timezone_name='UTC'):
        if max_bytes < 4096:
            raise BridgeError('RECORD_SIZE_LIMIT_TOO_SMALL')
        self.store, self.max_bytes, self.clock = store, max_bytes, clock
        self.timezone = ZoneInfo(timezone_name)
        self.directory = store.directory / 'records'
        self.directory.mkdir(exist_ok=True, mode=0o700)
        self.lock = threading.Lock()
        self.healthy = True

    def _target(self, day):
        files = sorted(self.directory.glob('events-' + day + '-*.jsonl'))
        if files:
            path = files[-1]
            # 写入中被强制终止可能留下不完整尾行；SQLite 仍保留完整事务事件。
            with path.open('r+b') as file:
                file.seek(0, 2)
                size = file.tell()
                if size:
                    file.seek(size - 1)
                    if file.read(1) != b'\n':
                        position = size
                        end = 0
                        while position:
                            start = max(0, position - 8192)
                            file.seek(start)
                            tail = file.read(position - start)
                            newline = tail.rfind(b'\n')
                            if newline >= 0:
                                end = start + newline + 1
                                break
                            position = start
                        file.truncate(end)
                        file.flush()
                        os.fsync(file.fileno())
            if path.stat().st_size < self.max_bytes:
                return path
            index = int(path.stem.rsplit('-', 1)[1]) + 1
        else:
            index = 1
        path = self.directory / f'events-{day}-{index:06d}.jsonl'
        with path.open('xb') as file:
            file.flush()
            os.fsync(file.fileno())
        self._sync_directory()
        return path

    def _sync_directory(self):
        fd = os.open(self.directory, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)

    def _append(self, record):
        line = json.dumps(record, ensure_ascii=False, separators=(',', ':')).encode() + b'\n'
        if len(line) > self.max_bytes:
            raise BridgeError('RECORD_EXCEEDS_ROTATION_LIMIT')
        day = datetime.fromtimestamp(self.clock(), self.timezone).strftime('%Y-%m-%d')
        path = self._target(day)
        if path.stat().st_size + len(line) > self.max_bytes:
            index = int(path.stem.rsplit('-', 1)[1]) + 1
            path = self.directory / f'events-{day}-{index:06d}.jsonl'
            path.touch(mode=0o600, exist_ok=False)
            self._sync_directory()
        with path.open('ab') as file:
            file.write(line)
            file.flush()
            os.fsync(file.fileno())
        if path.stat().st_size >= self.max_bytes:
            self._target(day)  # 精确达到上限立即创建下一分卷，不等待下一次事件。

    def flush(self):
        """返回导出事件数。文件 fsync 先于 SQLite 游标；崩溃可能重复，不会静默漏记。"""
        with self.lock:
            total = 0
            try:
                while True:
                    cursor = int(self.store.meta('record_cursor') or '0')
                    with self.store.connection() as db:
                        rows = db.execute('SELECT * FROM audit WHERE sequence>? ORDER BY sequence LIMIT 100', (cursor,)).fetchall()
                    if not rows:
                        break
                    for row in rows:
                        detail = json.loads(row['detail'])
                        record = {'schema_version': 1,
                                  'event_id': self.store.meta('installation') + ':' + detail['event_uuid'],
                                  'sequence': row['sequence'],
                                  'timestamp': datetime.fromtimestamp(row['at'], timezone.utc).isoformat(),
                                  'event': row['event'], **detail['record']}
                        # 导出必要操作上下文，不复制原始路径、EXIF 或整个内部台账。
                        safe = ('operation', 'new_video_id', 'previous_video_id')
                        record['context'] = {k: v for k, v in detail.get('data', {}).items() if k in safe}
                        self._append(record)
                        with self.store.connection() as db:
                            db.execute('INSERT INTO meta VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value',
                                       ('record_cursor', str(row['sequence'])))
                        total += 1
                self.healthy = True
                return total
            except Exception:
                self.healthy = False
                raise
