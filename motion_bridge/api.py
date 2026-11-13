"""Immich v3.4 API 适配。拒绝重定向，限制下载，不自动重试写请求。"""
import base64
import hashlib
import json
import urllib.error
import urllib.request
import uuid

from .config import BridgeError


def asset_id(value):
    try:
        return str(uuid.UUID(str(value)))
    except (ValueError, TypeError, AttributeError) as exc:
        raise BridgeError('INVALID_ASSET_ID') from exc


class APIError(BridgeError):
    def __init__(self, status, missing_or_denied=False):
        self.status = status
        self.missing_or_denied = missing_or_denied
        super().__init__('API_' + str(status))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Immich:
    def __init__(self, config):
        self.config = config
        # 不继承进程的 HTTP_PROXY，避免私有媒体和密钥意外经过外部代理。
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def request(self, method, path, body=None, binary=False, headers=None, limit=None, scan=False):
        if not path.startswith('/') or path.startswith('//'):
            raise BridgeError('INVALID_API_PATH')
        h = {'Accept': 'application/json'}
        if scan:
            if not self.config.scan_session:
                raise BridgeError('FULL_SCAN_REQUIRES_ELEVATED_SESSION')
            if (method, path) not in (('GET', '/users/me'), ('GET', '/auth/status'), ('POST', '/search/metadata')):
                raise BridgeError('SCAN_SESSION_READ_ONLY')
            h['Authorization'] = 'Bearer ' + self.config.scan_session
        else:
            h['x-api-key'] = self.config.api_key
        if isinstance(body, (dict, list)):
            body = json.dumps(body).encode()
            h['Content-Type'] = 'application/json'
        h.update(headers or {})
        req = urllib.request.Request(self.config.url + path, data=body, headers=h, method=method)
        maximum = limit if limit is not None else (self.config.max_bytes if binary else 16 * 1024 * 1024)
        try:
            with self.opener.open(req, timeout=self.config.timeout) as response:
                if int(response.headers.get('Content-Length', '0')) > maximum:
                    raise BridgeError('RESPONSE_TOO_LARGE')
                data = response.read(maximum + 1)
                if len(data) > maximum:
                    raise BridgeError('RESPONSE_TOO_LARGE')
                return data if binary else (json.loads(data) if data else None)
        except urllib.error.HTTPError as exc:
            # 只识别上游固定错误消息，不把响应体、路径或元数据输出到日志。
            try:
                message = json.loads(exc.read(16384)).get('message')
            except (ValueError, AttributeError):
                message = None
            missing = exc.code == 400 and message in ('Asset not found', 'Not found or no asset.read access')
            raise APIError(exc.code, missing) from None
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise BridgeError('API_NETWORK_ERROR') from None
        except (ValueError, TypeError) as exc:
            raise BridgeError('API_INVALID_RESPONSE') from None

    def get(self, id):
        try:
            return self.request('GET', '/assets/' + asset_id(id))
        except APIError as exc:
            if exc.status == 404 or exc.missing_or_denied:
                if self.config.scan_session:
                    self.require_full_scan()
                    items = list(self.search({'id': {'eq': asset_id(id)},
                                              'visibility': {'in': ['timeline', 'archive', 'hidden', 'locked']},
                                              'or': [{'trashedAt': {'eq': None}}, {'trashedAt': {'ne': None}}]}, scan=True))
                    return items[0] if items else None
                # 未提供提升会话时只能表示“不存在或不可见”；维护删除另要求完整扫描。
                return None
            raise

    def original(self, id):
        return self.request('GET', '/assets/' + asset_id(id) + '/original', binary=True)

    def link(self, photo, video):
        return self.request('PUT', '/assets/' + asset_id(photo), {'livePhotoVideoId': asset_id(video) if video else None})

    def delete(self, video):
        return self.request('DELETE', '/assets', {'ids': [asset_id(video)], 'force': True})

    def refresh(self, photo):
        return self.request('POST', '/assets/jobs', {'assetIds': [asset_id(photo)], 'name': 'refresh-metadata'})

    def duplicate(self, sha1):
        response = self.request('POST', '/assets/bulk-upload-check', {'assets': [{'id': 'motion', 'checksum': sha1}]})
        entries = response.get('results')
        if not isinstance(entries, list) or len(entries) != 1:
            raise BridgeError('INVALID_DUPLICATE_RESPONSE')
        entry = entries[0]
        if entry.get('action') == 'accept':
            return None
        if entry.get('reason') == 'duplicate' and entry.get('assetId'):
            return asset_id(entry['assetId'])
        raise BridgeError('DUPLICATE_CHECK_REJECTED')

    def upload(self, data, photo, installation):
        boundary = 'motion-' + uuid.uuid4().hex
        name = 'honor-motion-' + asset_id(photo['id']) + '-' + hashlib.sha256(data).hexdigest()[:16] + '.mp4'
        fields = {'deviceAssetId': name, 'deviceId': 'honor-motion-' + installation,
                  'fileCreatedAt': photo['fileCreatedAt'], 'fileModifiedAt': photo['fileModifiedAt']}
        chunks = []
        for key, value in fields.items():
            # API 字段仍须防止 multipart 头/边界注入。
            if not isinstance(value, str) or '\r' in value or '\n' in value:
                raise BridgeError('INVALID_UPLOAD_METADATA')
            chunks.append(f'--{boundary}\r\nContent-Disposition: form-data; name="{key}"\r\n\r\n{value}\r\n'.encode())
        chunks += [f'--{boundary}\r\nContent-Disposition: form-data; name="assetData"; filename="{name}"\r\nContent-Type: video/mp4\r\n\r\n'.encode(),
                   data, f'\r\n--{boundary}--\r\n'.encode()]
        return self.request('POST', '/assets', b''.join(chunks), headers={'Content-Type': 'multipart/form-data; boundary=' + boundary})

    def search(self, filter, scan=False):
        cursor = None
        seen = set()
        for _ in range(100000):
            payload = {'filter': filter, 'withExif': True, 'withStacked': True, 'size': 250}
            if cursor:
                payload['cursor'] = cursor
            response = self.request('POST', '/search/metadata', payload, scan=scan)
            assets = response.get('assets', {})
            items = assets.get('items')
            if not isinstance(items, list):
                raise BridgeError('SEARCH_INCOMPLETE')
            yield from items
            cursor = assets.get('nextCursor')
            if not cursor:
                if assets.get('nextPage'):
                    raise BridgeError('SEARCH_CURSOR_UNSUPPORTED')
                return
            if cursor in seen:
                raise BridgeError('SEARCH_CURSOR_LOOP')
            seen.add(cursor)
        raise BridgeError('SEARCH_TOO_LARGE')

    def require_full_scan(self):
        status = self.request('GET', '/auth/status', scan=True)
        owner = self.request('GET', '/users/me')['id']
        scanner = self.request('GET', '/users/me', scan=True)['id']
        if not status.get('isElevated') or owner != scanner:
            raise BridgeError('FULL_SCAN_SESSION_NOT_ELEVATED_OR_WRONG_OWNER')

    def all_photos(self, full=False):
        visibility = ['timeline', 'archive', 'hidden']
        if full:
            self.require_full_scan()
            visibility.append('locked')
        # 普通 API 密钥不能访问锁定资源。维护删除必须显式使用完整扫描，不能假装无引用。
        return self.search({'type': {'eq': 'IMAGE'}, 'visibility': {'in': visibility},
                            'or': [{'trashedAt': {'eq': None}}, {'trashedAt': {'ne': None}}]}, scan=full)


def sha1_base64(data):
    return base64.b64encode(hashlib.sha1(data).digest()).decode()
