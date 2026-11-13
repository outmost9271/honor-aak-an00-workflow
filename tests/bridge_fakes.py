"""人工夹具；不读取网络、真实账号或媒体。"""
from copy import deepcopy
from dataclasses import replace
import hashlib
from pathlib import Path
import struct
import tempfile
import uuid

from motion_bridge.api import APIError, sha1_base64
from motion_bridge.config import BridgeError, Config
from motion_bridge.engine import Engine
from motion_bridge.journal import Journal
from motion_bridge.store import Store

OWNER = '11111111-1111-4111-8111-111111111111'
PHOTO = '22222222-2222-4222-8222-222222222222'


def box(name, data=b''):
    return struct.pack('>I4s', len(data) + 8, name) + data


VIDEO = box(b'ftyp', b'isom0000') + box(b'moov') + box(b'mdat', b'synthetic')
SOURCE = b'\xff\xd8synthetic\xff\xd9' + VIDEO + b'v2_f0'.ljust(20) + b'0:1000'.ljust(20) + f'LIVE_{len(VIDEO)+20}'.encode().ljust(20)


class FakeAPI:
    def __init__(self):
        self.assets = {PHOTO: {'id': PHOTO, 'ownerId': OWNER, 'type': 'IMAGE', 'checksum': sha1_base64(SOURCE),
                              'exifInfo': {'make': 'HONOR', 'model': 'AAK-AN00'},
                              'fileCreatedAt': '2026-01-01T00:00:00Z', 'fileModifiedAt': '2026-01-01T00:00:00Z',
                              'livePhotoVideoId': None, 'isTrashed': False, 'visibility': 'timeline',
                              'originalPath': '/data/upload/synthetic.jpg'}}
        self.data = {PHOTO: SOURCE}
        self.uploads = self.links = self.deletes = 0
        self.upload_error_after_commit = False
        self.link_error_after_commit = False
        self.race_duplicate = False
        self.version = '3.4.0'
        self.permissions = ['asset.read', 'asset.download', 'asset.upload', 'asset.update', 'user.read', 'asset.delete', 'job.create']
        self.on_upload = None
        self.refresh_callback = None

    def request(self, method, path, body=None):
        if path == '/server/version':
            major, minor, patch = map(int, self.version.split('.'))
            return {'major': major, 'minor': minor, 'patch': patch, 'prerelease': None}
        if path == '/users/me':
            return {'id': OWNER}
        if path == '/api-keys/me':
            return {'permissions': self.permissions}
        raise AssertionError(path)

    def get(self, id):
        return deepcopy(self.assets.get(id))

    def original(self, id):
        if id not in self.data:
            raise APIError(404)
        return self.data[id]

    def duplicate(self, sha1):
        return next((id for id, a in self.assets.items() if a['type'] == 'VIDEO' and a['checksum'] == sha1), None)

    def upload(self, data, photo, installation):
        id = str(uuid.uuid4())
        self.assets[id] = {'id': id, 'ownerId': OWNER, 'type': 'VIDEO', 'checksum': sha1_base64(data),
                           'isTrashed': False, 'visibility': 'timeline', 'thumbhash': 'synthetic-ready', 'originalPath': '/data/upload/' + id + '.mp4'}
        self.data[id] = data
        self.uploads += 1
        if self.on_upload:
            self.on_upload()
        if self.upload_error_after_commit:
            self.upload_error_after_commit = False
            raise BridgeError('API_NETWORK_ERROR')
        return {'id': id, 'status': 'duplicate' if self.race_duplicate else 'created'}

    def link(self, photo, video):
        previous = self.assets[photo].get('livePhotoVideoId')
        self.assets[photo]['livePhotoVideoId'] = video
        if video:
            self.assets[video]['visibility'] = 'hidden'
        elif previous and previous in self.assets:
            self.assets[previous]['visibility'] = 'timeline'
        self.links += 1
        if self.link_error_after_commit:
            self.link_error_after_commit = False
            raise BridgeError('API_NETWORK_ERROR')
        return self.get(photo)

    def delete(self, id):
        self.assets.pop(id, None)
        self.data.pop(id, None)
        self.deletes += 1

    def all_photos(self, full=False):
        return [deepcopy(a) for a in self.assets.values() if a['type'] == 'IMAGE']

    def refresh(self, photo):
        if self.refresh_callback:
            self.refresh_callback(photo)


class Fixture:
    def __init__(self, root, **kwargs):
        self.config = replace(Config('http://immich.invalid/api', 'a' * 32, 'b' * 32, Path(root),
                                     mode='apply', writes=True, grace_seconds=0, media_timeout=1), **kwargs)
        self.store = Store(root)
        self.api = FakeAPI()
        self.journal = Journal(self.store, 4096)
        self.engine = Engine(self.config, self.store, self.api, self.journal)
        self.engine.inspect = lambda source: (VIDEO, {'duration': 1, 'streams': [{'codec_type': 'video', 'codec_name': 'hevc'}]})

    def run(self):
        self.store.enqueue(PHOTO)
        self.engine.run_one()
        self.journal.flush()
        return self.store.get(PHOTO)
