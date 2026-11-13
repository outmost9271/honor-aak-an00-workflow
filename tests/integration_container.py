"""显式运行的全隔离容器集成测试。只创建随机命名资源，不接收外部 Immich 地址。"""
import hashlib
import io
import json
import os
from pathlib import Path
import secrets
import sqlite3
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid
import zipfile

OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def wait(check, seconds=150):
    until = time.monotonic() + seconds
    while time.monotonic() < until:
        try:
            value = check()
            if value:
                return value
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(1)
    raise TimeoutError('isolated integration timeout')


def docker(*args):
    result = subprocess.run(['docker', *map(str, args)], capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError('Docker command failed: ' + result.stdout[-1500:] + result.stderr[-1500:])
    return result.stdout.strip()


def http(base, path, method='GET', body=None, token=None, binary=False):
    headers = {}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    if isinstance(body, dict):
        body = json.dumps(body).encode();headers['Content-Type'] = 'application/json'
    with OPENER.open(urllib.request.Request(base + path, data=body, headers=headers, method=method), timeout=60) as response:
        data = response.read()
        return data if binary else (json.loads(data) if data else None)


def upload(base, token, image):
    boundary = 'test-' + uuid.uuid4().hex
    fields = {'deviceAssetId': str(uuid.uuid4()), 'deviceId': 'isolated-test',
              'fileCreatedAt': '2026-01-01T00:00:00Z', 'fileModifiedAt': '2026-01-01T00:00:00Z'}
    body = b''
    for k, v in fields.items():
        body += f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode()
    body += f'--{boundary}\r\nContent-Disposition: form-data; name="assetData"; filename="synthetic-honor.jpg"\r\nContent-Type: image/jpeg\r\n\r\n'.encode() + image
    body += f'\r\n--{boundary}--\r\n'.encode()
    req = urllib.request.Request(base + '/assets', data=body, method='POST',
                                 headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'multipart/form-data; boundary=' + boundary})
    return json.loads(OPENER.open(req, timeout=60).read())['id']


def synthetic(root):
    jpg, mp4 = root / 'still.jpg', root / 'video.mp4'
    subprocess.run(['ffmpeg', '-v', 'error', '-y', '-f', 'lavfi', '-i', 'color=c=blue:s=320x240', '-frames:v', '1', str(jpg)], check=True)
    subprocess.run(['ffmpeg', '-v', 'error', '-y', '-f', 'lavfi', '-i', 'color=c=green:s=320x240:r=15:d=1',
                    '-f', 'lavfi', '-i', 'sine=frequency=440:duration=1', '-c:v', 'libx264', '-c:a', 'aac', '-shortest', str(mp4)], check=True)
    # 人工 TIFF 只含 make/model，没有真实设备号、拍摄时间、GPS 或个人信息。
    make, model = b'HONOR\x00', b'AAK-AN00\x00'
    strings = 8 + 2 + 2 * 12 + 4
    tiff = b'II*\x00\x08\x00\x00\x00' + struct.pack('<H', 2)
    tiff += struct.pack('<HHII', 0x10F, 2, len(make), strings)
    tiff += struct.pack('<HHII', 0x110, 2, len(model), strings + len(make)) + b'\x00' * 4 + make + model
    exif = b'Exif\x00\x00' + tiff
    image = jpg.read_bytes()
    image = image[:2] + b'\xff\xe1' + struct.pack('>H', len(exif) + 2) + exif + image[2:]
    video = mp4.read_bytes()
    return image + video + b'v2_f0'.ljust(20) + b'0:1000'.ljust(20) + f'LIVE_{len(video)+20}'.encode().ljust(20), video


def main():
    image = os.environ.get('BRIDGE_TEST_IMAGE', 'honor-motion:0.2.0')
    prefix = 'hmtest-' + uuid.uuid4().hex[:10]
    network, db, redis, server, bridge = [prefix + '-' + x for x in ('net', 'db', 'redis', 'server', 'bridge')]
    names = []
    checks = []
    with tempfile.TemporaryDirectory(prefix='honor-motion-integration-') as temp:
        root = Path(temp);state = root / 'state';state.mkdir()
        uid, gid = (os.getuid() or 10001), (os.getgid() or 10001)
        if os.getuid() == 0:
            os.chown(state, uid, gid)
        password = secrets.token_urlsafe(32)
        token = None
        try:
            docker('network', 'create', network)
            docker('run', '-d', '--name', db, '--network', network, '-e', 'POSTGRES_PASSWORD=' + password,
                   '-e', 'POSTGRES_USER=postgres', '-e', 'POSTGRES_DB=immich', '--shm-size', '128m',
                   'ghcr.io/immich-app/postgres:18-vectorchord1.1.1');names.append(db)
            docker('run', '-d', '--name', redis, '--network', network, 'valkey/valkey:9');names.append(redis)
            wait(lambda: subprocess.run(['docker', 'exec', db, 'pg_isready', '-U', 'postgres'], capture_output=True).returncode == 0)
            config = root / 'immich-config.json'
            config.write_text(json.dumps({'machineLearning': {'enabled': False}, 'reverseGeocoding': {'enabled': False}}))
            docker('run', '-d', '--name', server, '--network', network, '-p', '127.0.0.1::2283',
                   '-e', 'DB_HOSTNAME=' + db, '-e', 'DB_USERNAME=postgres', '-e', 'DB_PASSWORD=' + password,
                   '-e', 'DB_DATABASE_NAME=immich', '-e', 'REDIS_HOSTNAME=' + redis,
                   '-e', 'IMMICH_CONFIG_FILE=/lab.json', '-v', str(config) + ':/lab.json:ro',
                   'ghcr.io/immich-app/immich-server:v3.4.0');names.append(server)
            port = docker('port', server, '2283').rsplit(':', 1)[1]
            base = 'http://127.0.0.1:' + port + '/api'
            wait(lambda: http(base, '/server/ping'))
            account = {'email': 'lab@example.invalid', 'name': 'Synthetic integration', 'password': password}
            http(base, '/auth/admin-sign-up', 'POST', account)
            token = http(base, '/auth/login', 'POST', {'email': account['email'], 'password': password})['accessToken']
            perms = ['asset.read', 'asset.download', 'asset.upload', 'asset.update', 'user.read']
            key = http(base, '/api-keys', 'POST', {'name': 'Synthetic bridge', 'permissions': perms}, token)['secret']
            maintenance_key = http(base, '/api-keys', 'POST', {'name': 'Synthetic maintenance', 'permissions': perms + ['asset.delete', 'job.create']}, token)['secret']
            key_file, maint_file, hook_file = root / 'api-key', root / 'maintenance-key', root / 'webhook-key'
            scan_file = root / 'scan-session'
            pin = ''.join(str(secrets.randbelow(10)) for _ in range(6))
            http(base, '/auth/pin-code', 'POST', {'pinCode': pin}, token)
            http(base, '/auth/session/unlock', 'POST', {'pinCode': pin}, token)
            hook = secrets.token_urlsafe(32)
            for file, value in ((key_file, key), (maint_file, maintenance_key), (hook_file, hook), (scan_file, token)):
                file.write_text(value);file.chmod(0o600)
                if os.getuid() == 0:os.chown(file, uid, gid)
            def options(mode='apply', maintenance=False):
                extra = ['-v', str(scan_file) + ':/run/secrets/scan_session:ro', '-e', 'IMMICH_SCAN_SESSION_FILE=/run/secrets/scan_session'] if maintenance else []
                return extra + ['--network', network, '--user', f'{uid}:{gid}', '--read-only', '--cap-drop', 'ALL',
                        '--security-opt', 'no-new-privileges:true', '--pids-limit', '128', '--memory', '1g', '--cpus', '2',
                        '--tmpfs', '/tmp:rw,noexec,nosuid,nodev,size=384m,mode=1777',
                        '-v', str(state) + ':/state', '-v', str(maint_file if maintenance else key_file) + ':/run/secrets/immich_api_key:ro',
                        '-v', str(hook_file) + ':/run/secrets/webhook_secret:ro',
                        '-e', 'IMMICH_URL=http://' + server + ':2283', '-e', 'BRIDGE_MODE=' + mode,
                        '-e', 'IMMICH_WRITE_ENABLED=' + ('true' if mode == 'apply' else 'false'),
                        '-e', 'RECORD_MAX_BYTES=4096', '-e', 'CLEANUP_GRACE_SECONDS=0']
            def cli(*args, maintenance=False):
                return json.loads(docker('run', '--rm', *options(maintenance=maintenance), image, *args))
            def start_bridge(mode):
                docker('run', '-d', '--name', bridge, *options(mode), '-p', '127.0.0.1::8080', image)
                if bridge not in names:names.append(bridge)
                p = docker('port', bridge, '8080').rsplit(':', 1)[1]
                target = 'http://127.0.0.1:' + p
                wait(lambda: http(target, '/readyz'))
                return target
            target = start_bridge('observe')
            steps = [
                {'method': 'immich-plugin-core#assetTypeFilter', 'config': {'allowedTypes': ['IMAGE']}},
                {'method': 'immich-plugin-core#assetExifFilter', 'config': {'property': 'make', 'matchType': 'exact', 'pattern': 'HONOR'}},
                {'method': 'immich-plugin-core#assetExifFilter', 'config': {'property': 'model', 'matchType': 'exact', 'pattern': 'AAK-AN00'}},
                {'method': 'immich-plugin-core#webhook', 'config': {'url': 'http://' + bridge + ':8080/webhook', 'method': 'POST', 'headerName': 'X-Motion-Secret', 'headerValue': hook}},
            ]
            wait(lambda: 'assetTypeFilter' in json.dumps(http(base, '/plugins/methods', token=token)))
            try:
                http(base, '/workflows', 'POST', {'trigger': 'AssetMetadataExtraction', 'name': 'Synthetic test', 'enabled': True, 'steps': steps}, token)
            except urllib.error.HTTPError as exc:
                raise RuntimeError('Synthetic workflow setup: ' + exc.read().decode()) from None
            source, raw = synthetic(root)
            photo = upload(base, token, source)
            def row():
                dbh = sqlite3.connect('file:' + str(state / 'ledger.sqlite3') + '?mode=ro', uri=True)
                dbh.row_factory = sqlite3.Row
                try:
                    r = dbh.execute('SELECT * FROM jobs WHERE photo_id=?', (photo,)).fetchone()
                    return dict(r) if r else {}
                finally:dbh.close()
            wait(lambda: row().get('state') == 'observed')
            assert http(base, '/assets/' + photo, token=token)['livePhotoVideoId'] is None
            checks.append('observe performs no Immich writes; standard workflow delivers event')
            try:
                http(target, '/webhook', 'POST', {'type': 'AssetV1'})
                raise AssertionError('unauthenticated webhook accepted')
            except urllib.error.HTTPError as error:
                assert error.code == 401
            checks.append('webhook rejects unauthenticated requests')
            docker('stop', bridge);docker('rm', bridge)
            assert cli('enqueue', '--photo', photo, '--retry-terminal')['newly_queued'] == 1
            target = start_bridge('apply')
            def linked():
                r = row()
                if r.get('state') in ('dead', 'conflict'):
                    raise AssertionError('job failed: ' + str(r.get('error')))
                return r if r.get('state') == 'linked' else None
            record = wait(linked)
            video = record['video_id']
            assert http(base, '/assets/' + video, token=token)['visibility'] == 'hidden'
            assert http(base, '/assets/' + video + '/original', token=token, binary=True) == raw
            assert http(base, '/assets/' + photo + '/original', token=token, binary=True) == source
            checks.append('apply links lossless video; source unchanged; non-root hardened container')
            http(base, '/assets/jobs', 'POST', {'assetIds': [photo], 'name': 'refresh-metadata'}, token)
            time.sleep(3)
            assert http(base, '/assets/' + photo, token=token)['livePhotoVideoId'] == video
            checks.append('repeat metadata event preserves association')
            wait(lambda: http(base, '/assets/' + video + '/video/playback', token=token, binary=True))
            checks.append('Immich playback API returns video')
            assert cli('backup', '--output', '/state/backup.sqlite3')['status'] == 'backed_up'
            checks.append('online SQLite backup with service running')
            docker('stop', bridge)
            report = cli('reconcile')
            assert report[0]['status'] == 'linked', report
            checks.append('reference scan and reconciliation through official API')
            plan = cli('plan', 'rollback', '--photo', photo, '--output', '/state/rollback-plan.json', maintenance=True)
            # 低权限常驻密钥不具备删除权限；必须在取消关联之前失败。
            denied = subprocess.run(['docker', 'run', '--rm', *options(), image, 'execute', '--plan', '/state/rollback-plan.json',
                                      '--confirm', plan['confirmation'], '--maintenance-confirmed'], capture_output=True, text=True)
            assert denied.returncode != 0 and 'API_KEY_PERMISSIONS_INSUFFICIENT' in denied.stdout
            assert http(base, '/assets/' + photo, token=token)['livePhotoVideoId'] == video
            checks.append('maintenance requires separate delete permission before unlink')
            try:
                outcome = cli('execute', '--plan', '/state/rollback-plan.json', '--confirm', plan['confirmation'], '--maintenance-confirmed', maintenance=True)
            except RuntimeError:
                print('Synthetic maintenance state:', row().get('state'))
                logs = subprocess.run(['docker', 'logs', '--tail', '80', server], capture_output=True, text=True)
                print('\n'.join(line for line in (logs.stdout + logs.stderr).splitlines() if any(word in line for word in ('Exception', 'Error', 'error', 'Cannot', 'Skipping', 'invalid'))))
                raise
            assert outcome['status'] == 'completed'
            assert http(base, '/assets/' + photo, token=token)['livePhotoVideoId'] is None
            assert http(base, '/assets/' + photo + '/original', token=token, binary=True) == source
            checks.append('signed rollback preserves original; deletes only proven-owned video')
            records = [json.loads(line) for file in sorted((state / 'records').glob('*.jsonl')) for line in file.read_text().splitlines()]
            assert len(list((state / 'records').glob('*.jsonl'))) > 1
            assert all(file.stat().st_size <= 4096 for file in (state / 'records').glob('*.jsonl'))
            assert records[-1]['state'] == 'retired'
            assert any(r.get('video_id') == video and r.get('video_sha256') == hashlib.sha256(raw).hexdigest() for r in records)
            assert key not in json.dumps(records) and hook not in json.dumps(records)
            checks.append('bind-mounted JSONL records contain IDs/checksums/states and rotate by size')
            print(json.dumps({'passed': checks, 'count': len(checks)}, ensure_ascii=False, indent=2))
        finally:
            for name in reversed(names):
                subprocess.run(['docker', 'rm', '-f', '-v', name], capture_output=True)
            subprocess.run(['docker', 'network', 'rm', network], capture_output=True)


if __name__ == '__main__':main()
