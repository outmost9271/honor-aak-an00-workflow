"""只读对账、签名计划与显式维护。没有定时删除、直接数据库或文件系统操作。"""
import hashlib
import hmac
import json
import os
from pathlib import Path
import time

from .api import asset_id, sha1_base64
from .config import BridgeError, Conflict


def encoded_path(asset):
    # v3.4 以存储路径区分内嵌提取视频；这只是已验证版本的分类，不用于移动文件。
    return '/encoded-video/' in str((asset or {}).get('originalPath', ''))


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def snapshot(asset):
    if asset is None:
        return None
    keys = ('id', 'ownerId', 'type', 'checksum', 'livePhotoVideoId', 'isTrashed', 'originalPath', 'visibility')
    return {k: asset.get(k) for k in keys}


class Maintenance:
    def __init__(self, engine):
        self.engine, self.api, self.store, self.config = engine, engine.api, engine.store, engine.config

    def references(self, vid):
        return sorted(p['id'] for p in self.api.all_photos(full=True) if p.get('livePhotoVideoId') == vid)

    def verify_owned(self, row, video):
        if row and row['state'] in ('retired', 'migrated'):
            raise Conflict('MAINTENANCE_ALREADY_COMPLETED')
        if not row or not row['created_by_us'] or not row['video_id']:
            raise Conflict('VIDEO_PROVENANCE_UNPROVEN')
        if time.time() - (row['upload_intent'] or row['created']) < self.config.grace_seconds:
            raise Conflict('CLEANUP_GRACE_NOT_ELAPSED')
        if video is not None:
            self.engine.validate_video(row['video_id'], row['video_sha1'])
            if encoded_path(video):
                raise Conflict('NATIVE_VIDEO_MUST_NOT_BE_DELETED')
            raw = self.api.original(row['video_id'])
            if hashlib.sha256(raw).hexdigest() != row['video_sha256']:
                raise Conflict('VIDEO_CONTENT_CHANGED')
            return raw
        return None

    def native_proof(self, pid):
        pid = asset_id(pid)
        if self.store.get(pid):
            raise Conflict('PROOF_MUST_BE_UNTOUCHED_PHOTO')
        photo = self.api.get(pid)
        self.engine.check_photo(photo)
        vid = photo.get('livePhotoVideoId')
        if not vid:
            raise Conflict('NATIVE_SUPPORT_NOT_PROVEN')
        video = self.api.get(vid)
        self.engine.owned_asset(video, 'VIDEO')
        if not encoded_path(video):
            raise Conflict('PROOF_NOT_NATIVE_STORAGE')
        original = self.api.original(pid)
        raw, _ = self.engine.inspect(original)
        if self.api.original(vid) != raw:
            raise Conflict('PROOF_VIDEO_NOT_LOSSLESS')
        return {'photo_id': pid, 'checksum': photo['checksum'], 'video_id': vid, 'video_sha256': hashlib.sha256(raw).hexdigest()}

    def plan(self, operation, ids, proof=None):
        identity = self.engine.identity()
        if operation not in ('cleanup', 'rollback', 'migrate') or not 1 <= len(ids) <= 100:
            raise BridgeError('INVALID_PLAN_REQUEST')
        if len(set(ids)) != len(ids):
            raise BridgeError('DUPLICATE_PLAN_PHOTO')
        native = self.native_proof(proof) if operation == 'migrate' and proof else None
        if operation == 'migrate' and not native:
            raise Conflict('NATIVE_PROOF_REQUIRED')
        actions = []
        for pid in ids:
            pid = asset_id(pid)
            row = self.store.get(pid)
            video = self.api.get(row['video_id']) if row and row['video_id'] else None
            self.verify_owned(row, video)
            photo = self.api.get(pid)
            if photo:
                self.engine.owned_asset(photo, 'IMAGE')
                if photo.get('isTrashed') or photo.get('checksum') != row['source_sha1']:
                    raise Conflict('PHOTO_CHANGED_OR_TRASHED')
                if hashlib.sha256(self.api.original(pid)).hexdigest() != row['source_sha256']:
                    raise Conflict('PHOTO_CONTENT_CHANGED')
            elif operation == 'migrate':
                raise Conflict('MIGRATION_PHOTO_MISSING')
            refs = self.references(row['video_id'])
            allowed = [pid] if operation in ('rollback', 'migrate') else []
            if any(r not in allowed for r in refs):
                raise Conflict('VIDEO_STILL_REFERENCED')
            if operation == 'cleanup' and refs:
                raise Conflict('VIDEO_STILL_REFERENCED')
            if operation == 'migrate' and photo.get('livePhotoVideoId') not in (None, row['video_id']):
                other = self.api.get(photo['livePhotoVideoId'])
                if not encoded_path(other):
                    raise Conflict('ASSOCIATION_CHANGED')
            actions.append({'photo_id': pid, 'row_digest': digest(row), 'photo': snapshot(photo),
                            'video': snapshot(video), 'video_id': row['video_id'], 'references': refs})
        content = {'schema': 1, 'operation': operation, 'identity': identity,
                   'installation': self.store.meta('installation'), 'created': time.time(),
                   'expires': time.time() + 900, 'native_proof': native, 'actions': actions}
        signature = hmac.new(bytes.fromhex(self.store.meta('plan_key')), json.dumps(content, sort_keys=True).encode(), hashlib.sha256).hexdigest()
        return {'plan': content, 'confirmation': signature}

    def recovery_path(self, row):
        return self.store.directory / 'recovery' / (row['photo_id'] + '-' + row['video_sha256'] + '.mp4')

    def spool(self, row, raw):
        path = self.recovery_path(row)
        path.parent.mkdir(mode=0o700, exist_ok=True)
        if path.exists():
            if hashlib.sha256(path.read_bytes()).hexdigest() != row['video_sha256']:
                raise Conflict('RECOVERY_FILE_CHANGED')
            return
        if raw is None:
            raise Conflict('RECOVERY_FILE_MISSING')
        # 同目录临时文件 + fsync + 原子替换，避免崩溃留下看似完整的恢复副本。
        import tempfile
        fd, name = tempfile.mkstemp(prefix='.recovery-', dir=path.parent)
        temp = Path(name)
        try:
            with os.fdopen(fd, 'wb') as file:
                file.write(raw)
                file.flush()
                os.fsync(file.fileno())
            os.replace(temp, path)
            directory = os.open(path.parent, os.O_RDONLY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            temp.unlink(missing_ok=True)

    def execute(self, signed, confirmation, maintenance_confirmed=False):
        if not maintenance_confirmed or self.config.mode != 'apply':
            raise BridgeError('MAINTENANCE_WINDOW_REQUIRED')
        self.engine.writing()
        content = signed.get('plan', {})
        expected = hmac.new(bytes.fromhex(self.store.meta('plan_key')), json.dumps(content, sort_keys=True).encode(), hashlib.sha256).hexdigest()
        if not hmac.compare_digest(str(confirmation), expected) or signed.get('confirmation') != expected:
            raise BridgeError('PLAN_SIGNATURE_INVALID')
        if content.get('schema') != 1 or content.get('expires', 0) < time.time():
            raise BridgeError('PLAN_EXPIRED_OR_UNSUPPORTED')
        if content.get('identity') != self.engine.identity() or content.get('installation') != self.store.meta('installation'):
            raise BridgeError('PLAN_INSTANCE_CHANGED')
        self.engine.require_permissions({'asset.delete', 'job.create'} if content['operation'] == 'migrate' else {'asset.delete'})
        if content['operation'] == 'migrate':
            proof = content.get('native_proof')
            if not proof or self.native_proof(proof['photo_id']) != proof:
                raise Conflict('NATIVE_PROOF_CHANGED')
        # 一次性消费；执行中断必须依据现状重新生成计划，不能重放陈旧快照。
        with self.store.connection() as db:
            try:
                db.execute('INSERT INTO meta VALUES (?,?)', ('used_plan:' + expected, str(time.time())))
            except Exception:
                raise BridgeError('PLAN_ALREADY_USED') from None
        for action in content['actions']:
            self.execute_action(content['operation'], action)
        return {'status': 'completed', 'physical_file_cleanup': 'not_observable_via_api',
                'recovery_copies': 'retained_until_explicit_purge'}

    def execute_action(self, operation, action):
        pid, vid = action['photo_id'], action['video_id']
        row = self.store.get(pid)
        if digest(row) != action['row_digest']:
            raise Conflict('LEDGER_CHANGED_REPLAN_REQUIRED')
        photo, video = self.api.get(pid), self.api.get(vid)
        if snapshot(photo) != action['photo'] or snapshot(video) != action['video']:
            raise Conflict('REMOTE_CHANGED_REPLAN_REQUIRED')
        raw = self.verify_owned(row, video)
        if photo and hashlib.sha256(self.api.original(pid)).hexdigest() != row['source_sha256']:
            raise Conflict('PHOTO_CONTENT_CHANGED')
        if self.references(vid) != action['references']:
            raise Conflict('REFERENCES_CHANGED_REPLAN_REQUIRED')
        self.spool(row, raw)
        self.store.audit('maintenance_before', pid, {'operation': operation, 'ledger': row})
        self.store.update(pid, 'migration_wait' if operation == 'migrate' else 'cleanup_pending', event='maintenance_begin')
        if photo and photo.get('livePhotoVideoId') == vid:
            # 在维护窗口中执行。Immich 没有跨客户端 CAS，不能把此检查称作绝对竞态消除。
            if operation == 'cleanup':
                raise Conflict('VIDEO_STILL_REFERENCED')
            self.engine.writing()
            self.api.link(pid, None)
        if self.references(vid):
            raise Conflict('VIDEO_STILL_REFERENCED')
        if video is not None:
            self.engine.writing()
            self.api.delete(vid)
            deadline = time.monotonic() + 60
            while self.api.get(vid) is not None:
                if time.monotonic() > deadline:
                    raise BridgeError('DELETE_PENDING_REPLAN_REQUIRED')
                time.sleep(1)
        self.store.audit('api_delete_confirmed_files_unverified', pid, {'video_id': vid, 'last_known_path': row['original_path']})
        if operation == 'migrate':
            self.engine.writing()
            self.api.refresh(pid)
            self.finish_migration(pid, row)
        else:
            self.store.update(pid, 'retired', error=None, event='retired')

    def finish_migration(self, pid, row):
        deadline = time.monotonic() + self.config.media_timeout
        while time.monotonic() < deadline:
            photo = self.api.get(pid)
            if not photo or photo.get('checksum') != row['source_sha1']:
                raise Conflict('MIGRATION_PHOTO_CHANGED')
            vid = photo.get('livePhotoVideoId')
            if vid:
                video = self.api.get(vid)
                self.engine.owned_asset(video, 'VIDEO')
                if not encoded_path(video) or sha1_base64(self.api.original(vid)) != row['video_sha1']:
                    raise Conflict('NATIVE_RESULT_NOT_CANONICAL')
                self.store.audit('native_migration_complete', pid, {'new_video_id': vid})
                self.store.update(pid, 'migrated', native_video_id=vid, error=None, event='migrated')
                return
            time.sleep(1)
        self.store.update(pid, 'migration_wait', error='NATIVE_EXTRACTION_PENDING', event='migration_pending')
        raise BridgeError('NATIVE_EXTRACTION_PENDING')

    def restore_bridge(self, pid, maintenance_confirmed=False):
        if not maintenance_confirmed or self.config.mode != 'apply':
            raise BridgeError('MAINTENANCE_WINDOW_REQUIRED')
        self.engine.writing()
        self.engine.identity()
        row = self.store.get(asset_id(pid))
        if not row or row['state'] not in ('migration_wait', 'cleanup_pending'):
            raise Conflict('NOT_IN_INTERRUPTED_MAINTENANCE')
        photo = self.api.get(pid)
        self.engine.check_photo(photo)
        if photo.get('checksum') != row['source_sha1'] or photo.get('livePhotoVideoId'):
            raise Conflict('PHOTO_CHANGED_OR_ALREADY_LINKED')
        path = self.recovery_path(row)
        if not path.exists() or hashlib.sha256(path.read_bytes()).hexdigest() != row['video_sha256']:
            raise Conflict('RECOVERY_FILE_MISSING_OR_CHANGED')
        if row['video_id'] and self.api.get(row['video_id']) is not None:
            raise Conflict('OLD_VIDEO_STILL_EXISTS_REPLAN_REQUIRED')
        self.store.audit('restore_bridge_requested', pid, {'previous_video_id': row['video_id']})
        self.store.update(pid, 'pending', video_id=None, native_video_id=None, created_by_us=0, upload_intent=None,
                          attempts=0, due=time.time(), error=None, event='restore_queued')

    def purge_recovery(self, pid, confirmation):
        row = self.store.get(asset_id(pid))
        if not row or row['state'] not in ('retired', 'migrated') or confirmation != row['video_sha256']:
            raise Conflict('RECOVERY_PURGE_NOT_CONFIRMED')
        self.recovery_path(row).unlink(missing_ok=True)
        self.store.audit('recovery_purged', pid)
