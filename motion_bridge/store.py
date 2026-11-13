"""SQLite 持久队列、台账和操作审计。单卷只允许一个执行进程。"""
from contextlib import contextmanager
import fcntl
import json
import os
from pathlib import Path
import secrets
import sqlite3
import time
import uuid

from .config import BridgeError

ACTIVE = ('pending', 'retry', 'processing')


class ProcessLock:
    def __init__(self, directory):
        self.path = Path(directory) / 'process.lock'
        self.file = None

    def __enter__(self):
        self.file = self.path.open('a')
        try:
            fcntl.flock(self.file, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            self.file.close()
            raise BridgeError('EXECUTOR_ALREADY_RUNNING') from None
        return self

    def __exit__(self, *args):
        self.file.close()


class Store:
    def __init__(self, directory):
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.path = self.directory / 'ledger.sqlite3'
        with self.connection() as db:
            version = db.execute('PRAGMA user_version').fetchone()[0]
            if version not in (0, 1, 2):
                raise BridgeError('UNKNOWN_LEDGER_SCHEMA')
            db.execute('PRAGMA journal_mode=WAL')
            db.executescript('''
                CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS jobs (
                    photo_id TEXT PRIMARY KEY, state TEXT NOT NULL,
                    attempts INTEGER NOT NULL DEFAULT 0, due REAL NOT NULL,
                    created REAL NOT NULL, updated REAL NOT NULL, error TEXT,
                    source_sha1 TEXT, source_sha256 TEXT, video_sha1 TEXT, video_sha256 TEXT,
                    video_bytes INTEGER, video_id TEXT, native_video_id TEXT, created_by_us INTEGER NOT NULL DEFAULT 0,
                    upload_intent REAL, original_path TEXT, media TEXT
                );
                CREATE TABLE IF NOT EXISTS audit (
                    sequence INTEGER PRIMARY KEY AUTOINCREMENT, at REAL NOT NULL,
                    photo_id TEXT, event TEXT NOT NULL, detail TEXT NOT NULL
                );
                CREATE INDEX IF NOT EXISTS jobs_due ON jobs(state,due);
            ''')
            db.execute('BEGIN IMMEDIATE')
            columns = {r['name'] for r in db.execute('PRAGMA table_info(jobs)')}
            if 'native_video_id' not in columns:
                db.execute('ALTER TABLE jobs ADD COLUMN native_video_id TEXT')
            db.execute('PRAGMA user_version=2')
            db.execute('INSERT OR IGNORE INTO meta VALUES (?,?)', ('installation', str(uuid.uuid4())))
            db.execute('INSERT OR IGNORE INTO meta VALUES (?,?)', ('plan_key', secrets.token_hex(32)))

    @contextmanager
    def connection(self):
        db = sqlite3.connect(self.path, timeout=10)
        db.row_factory = sqlite3.Row
        db.execute('PRAGMA synchronous=FULL')
        db.execute('PRAGMA foreign_keys=ON')
        try:
            with db:
                yield db
        finally:
            db.close()

    def meta(self, key):
        with self.connection() as db:
            row = db.execute('SELECT value FROM meta WHERE key=?', (key,)).fetchone()
            return row[0] if row else None

    def bind(self, identity):
        text = json.dumps(identity, sort_keys=True)
        with self.connection() as db:
            db.execute('BEGIN IMMEDIATE')
            row = db.execute('SELECT value FROM meta WHERE key=?', ('identity',)).fetchone()
            if row and row[0] != text:
                raise BridgeError('INSTANCE_OR_OWNER_CHANGED')
            db.execute('INSERT OR IGNORE INTO meta VALUES (?,?)', ('identity', text))

    def audit(self, event, photo=None, detail=None):
        with self.connection() as db:
            self._audit(db, event, photo, detail)

    @staticmethod
    def _audit(db, event, photo, detail=None):
        row = db.execute('SELECT * FROM jobs WHERE photo_id=?', (photo,)).fetchone() if photo else None
        keys = ('photo_id', 'video_id', 'native_video_id', 'source_sha1', 'source_sha256', 'video_sha1', 'video_sha256',
                'video_bytes', 'state', 'attempts', 'error', 'created_by_us')
        record = {k: row[k] for k in keys} if row else {'photo_id': photo}
        payload = {'event_uuid': str(uuid.uuid4()), 'record': record, 'data': detail or {}}
        db.execute('INSERT INTO audit(at,photo_id,event,detail) VALUES (?,?,?,?)',
                   (time.time(), photo, event, json.dumps(payload, sort_keys=True)))

    def enqueue(self, photo, maximum=10000, retry_terminal=False):
        now = time.time()
        with self.connection() as db:
            db.execute('BEGIN IMMEDIATE')
            old = db.execute('SELECT state FROM jobs WHERE photo_id=?', (photo,)).fetchone()
            # linked 不能被 Webhook 重置，否则会丢失来源台账。
            if old and (old[0] in ACTIVE or not retry_terminal or old[0] in ('linked', 'retired', 'migrated', 'cleanup_pending', 'migration_wait')):
                return False
            active = db.execute("SELECT count(*) FROM jobs WHERE state IN ('pending','retry','processing')").fetchone()[0]
            if active >= maximum:
                raise BridgeError('QUEUE_FULL')
            if old:
                db.execute("UPDATE jobs SET state='pending',attempts=0,due=?,updated=?,error=NULL WHERE photo_id=?", (now, now, photo))
            else:
                db.execute('INSERT INTO jobs(photo_id,state,due,created,updated) VALUES (?,?,?,?,?)', (photo, 'pending', now, now, now))
            self._audit(db, 'enqueue', photo)
            return True

    def recover(self):
        # 仅持有进程排他锁的执行器在启动时调用，不抢占仍活跃的工作线程。
        with self.connection() as db:
            ids = [r[0] for r in db.execute("SELECT photo_id FROM jobs WHERE state='processing'")]
            db.execute("UPDATE jobs SET state='retry',due=?,error='PROCESS_RESTART' WHERE state='processing'", (time.time(),))
            for pid in ids:
                self._audit(db, 'recovered_after_restart', pid)

    def claim(self):
        with self.connection() as db:
            db.execute('BEGIN IMMEDIATE')
            row = db.execute("SELECT * FROM jobs WHERE state IN ('pending','retry') AND due<=? ORDER BY due,photo_id LIMIT 1", (time.time(),)).fetchone()
            if row is None:
                return None
            db.execute("UPDATE jobs SET state='processing',attempts=attempts+1,updated=? WHERE photo_id=?", (time.time(), row['photo_id']))
            self._audit(db, 'processing', row['photo_id'])
            return dict(db.execute('SELECT * FROM jobs WHERE photo_id=?', (row['photo_id'],)).fetchone())

    def get(self, photo):
        with self.connection() as db:
            row = db.execute('SELECT * FROM jobs WHERE photo_id=?', (photo,)).fetchone()
            return dict(row) if row else None

    def rows(self):
        with self.connection() as db:
            return [dict(r) for r in db.execute('SELECT * FROM jobs ORDER BY photo_id')]

    def update(self, photo, state=None, event=None, **fields):
        allowed = {'attempts', 'due', 'error', 'source_sha1', 'source_sha256', 'video_sha1', 'video_sha256',
                   'video_bytes', 'video_id', 'native_video_id', 'created_by_us', 'upload_intent', 'original_path', 'media'}
        if not set(fields) <= allowed:
            raise ValueError('invalid ledger field')
        if state:
            fields['state'] = state
        fields['updated'] = time.time()
        with self.connection() as db:
            db.execute('UPDATE jobs SET ' + ','.join(k + '=?' for k in fields) + ' WHERE photo_id=?', [*fields.values(), photo])
            if event:
                self._audit(db, event, photo, {'state': state} if state else {})

    def stats(self):
        with self.connection() as db:
            counts = {r[0]: r[1] for r in db.execute('SELECT state,count(*) FROM jobs GROUP BY state')}
            oldest = db.execute("SELECT min(created) FROM jobs WHERE state IN ('pending','retry')").fetchone()[0]
            return {'states': counts, 'oldest_pending_age_seconds': int(time.time() - oldest) if oldest else 0}

    def backup(self, target):
        target = Path(target)
        if target.exists() or target.resolve() == self.path.resolve():
            raise BridgeError('BACKUP_TARGET_EXISTS')
        target.parent.mkdir(parents=True, exist_ok=True)
        # O_EXCL 防止覆盖；SQLite backup API 同时包含 WAL 中已经提交的事务。
        fd = os.open(target, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        os.close(fd)
        try:
            with self.connection() as source:
                dest = sqlite3.connect(target)
                try:
                    source.backup(dest)
                    if dest.execute('PRAGMA integrity_check').fetchone()[0] != 'ok':
                        raise BridgeError('BACKUP_INTEGRITY_FAILED')
                finally:
                    dest.close()
        except Exception:
            target.unlink(missing_ok=True)
            raise
