"""只新增关联，不覆盖关联，不自动删除。跨系统失败按保守状态恢复。"""
import hashlib
import json
import math
from pathlib import Path
import random
import subprocess
import tempfile
import time

from honor_motion import MotionError, parse_motion
from .api import APIError, asset_id, sha1_base64
from .config import BridgeError, Conflict, Skip


class Engine:
    def __init__(self, config, store, api, journal=None):
        self.config, self.store, self.api = config, store, api
        self.journal = journal
        self.owner = None

    def identity(self):
        version = self.api.request('GET', '/server/version')
        text = '.'.join(str(version[k]) for k in ('major', 'minor', 'patch'))
        if version.get('prerelease') or text not in self.config.versions:
            raise BridgeError('UNAPPROVED_IMMICH_VERSION')
        owner = asset_id(self.api.request('GET', '/users/me')['id'])
        self.store.bind({'url': self.config.url, 'owner': owner})
        self.owner = owner
        required = {'asset.read', 'asset.download', 'user.read'}
        if self.config.mode == 'apply':
            required |= {'asset.upload', 'asset.update'}
        self.require_permissions(required)
        return {'url': self.config.url, 'owner': owner, 'version': text}

    def require_permissions(self, required):
        permissions = set(self.api.request('GET', '/api-keys/me')['permissions'])
        if 'all' not in permissions and not set(required) <= permissions:
            raise BridgeError('API_KEY_PERMISSIONS_INSUFFICIENT')

    def writing(self):
        if not self.config.writes:
            raise BridgeError('WRITE_DISABLED')
        if self.journal:
            self.journal.flush()

    def owned_asset(self, asset, type):
        if not asset or asset.get('ownerId') != self.owner or asset.get('type') != type:
            raise Conflict('ASSET_OWNER_OR_TYPE_MISMATCH')
        if asset.get('isExternal') or asset.get('libraryId'):
            raise Conflict('EXTERNAL_LIBRARY_UNSUPPORTED')

    def check_photo(self, photo):
        self.owned_asset(photo, 'IMAGE')
        if photo.get('isTrashed') or photo.get('isOffline') or photo.get('visibility') == 'locked':
            raise Skip('PHOTO_UNAVAILABLE')
        exif = photo.get('exifInfo') or {}
        if str(exif.get('make', '')).upper() != 'HONOR' or (self.config.model and exif.get('model') != self.config.model):
            raise Skip('NOT_TARGET_CAMERA')

    def inspect(self, data):
        try:
            start, end, _ = parse_motion(data)
        except MotionError:
            raise Skip('NO_SUPPORTED_MOTION_TRAILER') from None
        video = data[start:end]
        with tempfile.TemporaryDirectory(prefix='motion-') as directory:
            source = Path(directory) / 'source.mp4'
            source.write_bytes(video)
            try:
                result = subprocess.run(['ffprobe', '-v', 'error', '-protocol_whitelist', 'file,pipe',
                                         '-show_streams', '-show_format', '-of', 'json', str(source)],
                                        capture_output=True, timeout=self.config.media_timeout, check=True)
                info = json.loads(result.stdout)
                streams = info['streams']
                videos = [s for s in streams if s.get('codec_type') == 'video']
                duration = float(info['format']['duration'])
                if not videos or not math.isfinite(duration) or not 0 < duration <= self.config.max_seconds:
                    raise Conflict('UNSAFE_VIDEO_DURATION')
                if any(not 0 < s.get('width', 0) <= 8192 or not 0 < s.get('height', 0) <= 8192 for s in videos):
                    raise Conflict('UNSAFE_VIDEO_DIMENSIONS')
                if self.config.require_audio and not any(s.get('codec_type') == 'audio' for s in streams):
                    raise Conflict('AUDIO_REQUIRED')
                if self.config.decode:
                    subprocess.run(['ffmpeg', '-v', 'error', '-nostdin', '-xerror', '-protocol_whitelist', 'file,pipe',
                                    '-threads', '1', '-i', str(source), '-map', '0:v:0', '-map', '0:a:0?',
                                    '-fps_mode', 'passthrough', '-f', 'null', '-'],
                                   capture_output=True, timeout=self.config.media_timeout, check=True)
            except (subprocess.SubprocessError, OSError, ValueError, KeyError):
                raise Conflict('MEDIA_VALIDATION_FAILED') from None
        keys = ('codec_type', 'codec_name', 'width', 'height', 'sample_rate', 'channels')
        media = {'duration': duration, 'streams': [{k: s[k] for k in keys if k in s} for s in streams]}
        return video, media

    def assert_photo_unchanged(self, photo_id, checksum, expected_link=None):
        current = self.api.get(photo_id)
        self.check_photo(current)
        if current.get('checksum') != checksum:
            raise Conflict('PHOTO_CHANGED')
        if current.get('livePhotoVideoId') != expected_link:
            raise Conflict('ASSOCIATION_CHANGED')
        return current

    def validate_video(self, video_id, checksum):
        asset = self.api.get(video_id)
        if not asset:
            raise Conflict('RECORDED_VIDEO_MISSING')
        self.owned_asset(asset, 'VIDEO')
        if asset.get('isTrashed') or asset.get('visibility') == 'locked' or asset.get('checksum') != checksum:
            raise Conflict('RECORDED_VIDEO_CHANGED')
        return asset

    def process(self, row):
        self.identity()  # 每项开始检查版本和实例，升级后默认停止写入。
        pid = row['photo_id']
        photo = self.api.get(pid)
        if photo is None:
            raise Skip('PHOTO_MISSING')
        self.check_photo(photo)
        current = photo.get('livePhotoVideoId')
        if current:
            if row['source_sha1'] and photo.get('checksum') != row['source_sha1']:
                raise Conflict('PHOTO_CHANGED')
            if row['video_id'] == current and row['created_by_us']:
                self.validate_video(current, row['video_sha1'])
                self.store.update(pid, 'linked', error=None, event='already_linked')
                return
            if row['video_id']:
                raise Conflict('ASSOCIATION_CHANGED')
            raise Skip('ALREADY_HAS_MOTION')
        source = self.api.original(pid)
        source_sha1 = sha1_base64(source)
        source_sha256 = hashlib.sha256(source).hexdigest()
        if source_sha1 != photo.get('checksum'):
            raise Conflict('PHOTO_CHECKSUM_MISMATCH')
        if row['source_sha256'] and row['source_sha256'] != source_sha256:
            raise Conflict('PHOTO_CHANGED')
        video, media = self.inspect(source)
        vsha1, vsha256 = sha1_base64(video), hashlib.sha256(video).hexdigest()
        if row['video_sha256'] and row['video_sha256'] != vsha256:
            raise Conflict('EXTRACTED_VIDEO_CHANGED')
        self.store.update(pid, source_sha1=source_sha1, source_sha256=source_sha256,
                          video_sha1=vsha1, video_sha256=vsha256, video_bytes=len(video),
                          media=json.dumps(media, sort_keys=True), event='validated')
        if self.config.mode == 'observe':
            self.store.update(pid, 'observed', error=None, event='observe_complete')
            return
        self.writing()
        photo = self.assert_photo_unchanged(pid, source_sha1)
        vid = row['video_id']
        if vid:
            if not row['created_by_us']:
                raise Conflict('VIDEO_PROVENANCE_UNPROVEN')
            self.validate_video(vid, vsha1)
        else:
            duplicate = self.api.duplicate(vsha1)
            if duplicate:
                self.store.update(pid, video_id=duplicate, created_by_us=0, event='duplicate_unclaimed')
                raise Conflict('UPLOAD_OUTCOME_AMBIGUOUS' if row['upload_intent'] else 'PREEXISTING_VIDEO')
            # 在网络写入前落盘；超时或崩溃不能因为文件名相似就冒认所有权。
            self.store.update(pid, upload_intent=time.time(), event='upload_intent')
            self.writing()
            response = self.api.upload(video, photo, self.store.meta('installation'))
            vid = asset_id(response['id'])
            created = response.get('status') == 'created'
            self.store.update(pid, video_id=vid, created_by_us=int(created), event='upload_result')
            if not created:
                raise Conflict('UPLOAD_DUPLICATE_NOT_OWNED')
            remote = self.validate_video(vid, vsha1)
            self.store.update(pid, original_path=remote.get('originalPath'))
        self.assert_photo_unchanged(pid, source_sha1)
        # Immich 没有 compare-and-swap 接口。保守地再次读取，不承诺跨外部客户端的原子性。
        self.writing()
        self.api.link(pid, vid)
        linked = self.api.get(pid)
        if not linked or linked.get('livePhotoVideoId') != vid:
            raise Conflict('LINK_NOT_CONFIRMED')
        remote = self.validate_video(vid, vsha1)
        if remote.get('visibility') != 'hidden':
            raise Conflict('VIDEO_NOT_HIDDEN')
        self.store.update(pid, 'linked', error=None, original_path=remote.get('originalPath'), event='link_confirmed')

    def run_one(self):
        row = self.store.claim()
        if row is None:
            return False
        pid = row['photo_id']
        try:
            self.process(row)
        except Skip as exc:
            self.store.update(pid, 'skipped', error=exc.code, event='skipped')
        except Conflict as exc:
            self.store.update(pid, 'conflict', error=exc.code, event='conflict')
        except APIError as exc:
            terminal = exc.status in (400, 401, 403, 404, 409, 413, 422)
            self.failure(row, exc.code, terminal)
        except BridgeError as exc:
            self.failure(row, exc.code)
        except Exception:
            # 保留上传意图与已确认的视频 ID；不泄露异常中的媒体路径或请求头。
            self.failure(row, 'UNEXPECTED_ERROR')
        return True

    def failure(self, row, code, terminal=False):
        terminal = terminal or row['attempts'] >= self.config.max_attempts
        delay = min(3600, 5 * 2 ** min(row['attempts'], 10)) + random.uniform(0, 3)
        self.store.update(row['photo_id'], 'dead' if terminal else 'retry', error=code,
                          due=time.time() + delay, event='failure')

    def reconcile(self):
        """只读取远端，不换绑、不删除；异常保留原始来源记录。"""
        self.identity()
        refs = {}
        full = bool(self.config.scan_session)
        for photo in self.api.all_photos(full=full):
            if photo.get('ownerId') != self.owner:
                continue
            vid = photo.get('livePhotoVideoId')
            if vid:
                refs.setdefault(vid, []).append(photo['id'])
        report = []
        for row in self.store.rows():
            if row['state'] == 'migrated':
                photo = self.api.get(row['photo_id'])
                native = self.api.get(row['native_video_id']) if row['native_video_id'] else None
                good = (photo and native and photo.get('livePhotoVideoId') == row['native_video_id']
                        and '/encoded-video/' in native.get('originalPath', '') and native.get('checksum') == row['video_sha1'])
                report.append({'photo_id': row['photo_id'], 'video_id': row['video_id'],
                               'native_video_id': row['native_video_id'], 'status': 'migrated_native' if good else 'migration_state_changed'})
                continue
            if row['state'] == 'retired':
                visible = self.api.get(row['video_id']) if row['video_id'] else None
                report.append({'photo_id': row['photo_id'], 'video_id': row['video_id'],
                               'status': 'retired_video_still_visible' if visible else 'retired_api_unavailable_files_unverified'})
                continue
            if not row['video_id']:
                report.append({'photo_id': row['photo_id'], 'status': row['state'], 'error': row['error']})
                continue
            photo, video = self.api.get(row['photo_id']), self.api.get(row['video_id'])
            if video and video.get('ownerId') == self.owner:
                self.store.update(row['photo_id'], original_path=video.get('originalPath'))
            if video is None:
                status = 'remote_video_unavailable_possible_disk_residue'
            elif photo and row['source_sha1'] and photo.get('checksum') != row['source_sha1']:
                status = 'source_checksum_changed'
            elif video.get('checksum') != row['video_sha1']:
                status = 'video_checksum_changed'
            elif not row['created_by_us']:
                status = 'unproven_provenance_manual_review'
            elif photo and photo.get('livePhotoVideoId') == row['video_id']:
                status = 'linked'
            elif refs.get(row['video_id']):
                status = 'referenced_elsewhere'
            else:
                status = 'owned_unreferenced_candidate' if full else 'unreferenced_in_non_locked_scope_only'
            report.append({'photo_id': row['photo_id'], 'video_id': row['video_id'], 'status': status,
                           'reference_scope': 'all_including_locked' if full else 'non_locked_only',
                           'references': refs.get(row['video_id'], [])})
        return report
