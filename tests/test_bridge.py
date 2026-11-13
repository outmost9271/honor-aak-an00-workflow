import concurrent.futures
from dataclasses import replace
import hashlib
import json
from pathlib import Path
import sqlite3
import tempfile
import time
import unittest
from unittest.mock import patch
import uuid

from bridge_fakes import Fixture, OWNER, PHOTO, SOURCE, VIDEO
from motion_bridge.api import APIError, asset_id
from motion_bridge.config import BridgeError, Conflict, Config, canonical_url, secret
from motion_bridge.journal import Journal
from motion_bridge.maintenance import Maintenance, digest
from motion_bridge.store import ProcessLock, Store


class CoreTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.f = Fixture(self.root)

    def tearDown(self):
        self.temp.cleanup()

    def test_apply_and_repeat(self):
        row = self.f.run()
        self.assertEqual(row['state'], 'linked')
        self.assertTrue(row['created_by_us'])
        self.assertEqual(row['video_sha256'], hashlib.sha256(VIDEO).hexdigest())
        self.assertEqual(self.f.api.original(PHOTO), SOURCE)
        self.assertFalse(self.f.store.enqueue(PHOTO))
        self.assertEqual(self.f.api.uploads, 1)
        self.assertEqual(self.f.api.deletes, 0)

    def test_observe_never_writes(self):
        f = Fixture(self.root / 'observe', mode='observe', writes=False)
        self.assertEqual(f.run()['state'], 'observed')
        self.assertEqual((f.api.uploads, f.api.links, f.api.deletes), (0, 0, 0))
        self.assertTrue(f.store.enqueue(PHOTO, retry_terminal=True))

    def test_existing_link_never_overwritten(self):
        self.f.api.assets[PHOTO]['livePhotoVideoId'] = str(uuid.uuid4())
        row = self.f.run()
        self.assertEqual((row['state'], row['error']), ('skipped', 'ALREADY_HAS_MOTION'))
        self.assertEqual(self.f.api.uploads, 0)

    def test_duplicate_not_claimed(self):
        self.f.api.upload(VIDEO, self.f.api.get(PHOTO), 'existing')
        row = self.f.run()
        self.assertEqual(row['error'], 'PREEXISTING_VIDEO')
        self.assertFalse(row['created_by_us'])
        self.assertEqual(self.f.api.links, 0)

    def test_duplicate_race_not_claimed(self):
        self.f.api.race_duplicate = True
        row = self.f.run()
        self.assertEqual(row['error'], 'UPLOAD_DUPLICATE_NOT_OWNED')
        self.assertFalse(row['created_by_us'])

    def test_ambiguous_upload_is_not_deleted_or_claimed(self):
        self.f.api.upload_error_after_commit = True
        row = self.f.run()
        self.assertEqual(row['state'], 'retry')
        self.f.store.update(PHOTO, due=0)
        self.f.engine.run_one()
        row = self.f.store.get(PHOTO)
        self.assertEqual(row['error'], 'UPLOAD_OUTCOME_AMBIGUOUS')
        self.assertFalse(row['created_by_us'])
        self.assertEqual((self.f.api.uploads, self.f.api.deletes, self.f.api.links), (1, 0, 0))

    def test_lost_link_response_recovers_idempotently(self):
        self.f.api.link_error_after_commit = True
        row = self.f.run()
        self.assertEqual(row['state'], 'retry')
        self.f.store.update(PHOTO, due=0)
        self.f.engine.run_one()
        self.assertEqual(self.f.store.get(PHOTO)['state'], 'linked')
        self.assertEqual(self.f.api.uploads, 1)
        self.assertEqual(self.f.api.links, 1)

    def test_foreign_owner_and_wrong_camera(self):
        self.f.api.assets[PHOTO]['ownerId'] = str(uuid.uuid4())
        self.assertEqual(self.f.run()['error'], 'ASSET_OWNER_OR_TYPE_MISMATCH')
        f = Fixture(self.root / 'wrong')
        f.api.assets[PHOTO]['exifInfo']['make'] = 'Other'
        self.assertEqual(f.run()['state'], 'skipped')

    def test_changed_during_upload_does_not_link(self):
        def change():
            self.f.api.assets[PHOTO]['checksum'] = 'changed'
        self.f.api.on_upload = change
        row = self.f.run()
        self.assertEqual(row['error'], 'PHOTO_CHANGED')
        self.assertTrue(row['created_by_us'])
        self.assertEqual(self.f.api.links, 0)

    def test_photo_changed_after_link_conflicts(self):
        row = self.f.run()
        self.f.api.assets[PHOTO]['checksum'] = 'changed'
        self.f.store.update(PHOTO, 'pending', due=0)
        self.f.engine.run_one()
        self.assertEqual(self.f.store.get(PHOTO)['error'], 'PHOTO_CHANGED')

    def test_upgrade_gate(self):
        self.f.api.version = '3.5.0'
        row = self.f.run()
        self.assertEqual(row['error'], 'UNAPPROVED_IMMICH_VERSION')
        self.assertEqual(self.f.api.uploads, 0)

    def test_instance_owner_binding(self):
        self.f.engine.identity()
        with self.assertRaises(BridgeError):
            self.f.store.bind({'url': 'http://elsewhere.invalid/api', 'owner': OWNER})

    def test_permanent_auth_error_and_retry_limit(self):
        with patch.object(self.f.api, 'original', side_effect=APIError(403)):
            self.assertEqual(self.f.run()['state'], 'dead')
        f = Fixture(self.root / 'retry', max_attempts=1)
        with patch.object(f.api, 'original', side_effect=BridgeError('API_NETWORK_ERROR')):
            self.assertEqual(f.run()['state'], 'dead')

    def test_journal_failure_prevents_api_writes(self):
        with patch.object(self.f.journal, 'flush', side_effect=OSError('disk full')):
            self.f.store.enqueue(PHOTO)
            self.f.engine.run_one()
        self.assertEqual(self.f.api.uploads, 0)
        self.assertEqual(self.f.store.get(PHOTO)['state'], 'retry')

    def test_claim_is_atomic_and_queue_is_bounded(self):
        self.f.store.enqueue(PHOTO, maximum=1)
        with self.assertRaises(BridgeError):
            self.f.store.enqueue(str(uuid.uuid4()), maximum=1)
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            claims = list(pool.map(lambda _: self.f.store.claim(), range(8)))
        self.assertEqual(sum(x is not None for x in claims), 1)
        self.f.store.recover()
        self.assertEqual(self.f.store.get(PHOTO)['state'], 'retry')
        self.assertEqual(self.f.store.claim()['attempts'], 2)

    def test_process_lock(self):
        with ProcessLock(self.root):
            with self.assertRaises(BridgeError):
                with ProcessLock(self.root):
                    pass

    def test_schema_upgrade_preserves_queue(self):
        self.f.store.enqueue(PHOTO)
        with self.f.store.connection() as db:
            db.execute('ALTER TABLE jobs DROP COLUMN native_video_id')
            db.execute('PRAGMA user_version=1')
        upgraded = Store(self.root)
        self.assertEqual(upgraded.get(PHOTO)['state'], 'pending')
        self.assertIsNone(upgraded.get(PHOTO)['native_video_id'])
        with upgraded.connection() as db:
            self.assertEqual(db.execute('PRAGMA user_version').fetchone()[0], 2)

    def test_backup_is_consistent_and_exclusive(self):
        row = self.f.run()
        backup = self.root / 'snapshot.sqlite3'
        self.f.store.backup(backup)
        db = sqlite3.connect(backup)
        try:
            self.assertEqual(db.execute('PRAGMA integrity_check').fetchone()[0], 'ok')
            self.assertEqual(db.execute('SELECT video_id FROM jobs').fetchone()[0], row['video_id'])
        finally:
            db.close()
        with self.assertRaises(BridgeError):
            self.f.store.backup(backup)


class MaintenanceTests(unittest.TestCase):
    setUp = CoreTests.setUp
    tearDown = CoreTests.tearDown

    def test_rollback_and_recovery_purge(self):
        row = self.f.run()
        m = Maintenance(self.f.engine)
        plan = m.plan('rollback', [PHOTO])
        response = m.execute(plan, plan['confirmation'], True)
        self.assertEqual(response['physical_file_cleanup'], 'not_observable_via_api')
        self.assertIsNone(self.f.api.get(PHOTO)['livePhotoVideoId'])
        self.assertIsNone(self.f.api.get(row['video_id']))
        self.assertEqual(self.f.store.get(PHOTO)['state'], 'retired')
        self.assertTrue(m.recovery_path(row).exists())
        m.purge_recovery(PHOTO, row['video_sha256'])
        self.assertFalse(m.recovery_path(row).exists())

    def test_cleanup_rejects_referenced_reused_native_and_recent(self):
        row = self.f.run()
        m = Maintenance(self.f.engine)
        with self.assertRaises(Conflict):m.plan('cleanup', [PHOTO])
        self.f.api.link(PHOTO, None)
        self.f.store.update(PHOTO, created_by_us=0)
        with self.assertRaises(Conflict):m.plan('cleanup', [PHOTO])
        self.f.store.update(PHOTO, created_by_us=1)
        self.f.api.assets[row['video_id']]['originalPath'] = '/data/encoded-video/native.mp4'
        with self.assertRaises(Conflict):m.plan('cleanup', [PHOTO])
        self.f.api.assets[row['video_id']]['originalPath'] = '/data/upload/raw.mp4'
        m.config = replace(m.config, grace_seconds=86400)
        with self.assertRaises(Conflict):m.plan('cleanup', [PHOTO])

    def test_tamper_stale_replay_and_window(self):
        self.f.run()
        m = Maintenance(self.f.engine)
        signed = m.plan('rollback', [PHOTO])
        with self.assertRaises(BridgeError):m.execute(signed, signed['confirmation'], False)
        with self.assertRaises(BridgeError):m.execute(signed, '0' * 64, True)
        tampered = json.loads(json.dumps(signed));tampered['plan']['operation'] = 'cleanup'
        with self.assertRaises(BridgeError):m.execute(tampered, signed['confirmation'], True)
        self.f.api.assets[PHOTO]['visibility'] = 'archive'
        with self.assertRaises(Conflict):m.execute(signed, signed['confirmation'], True)
        with self.assertRaises(BridgeError):m.execute(signed, signed['confirmation'], True)

    def test_other_photo_reference_blocks(self):
        row = self.f.run()
        other = dict(self.f.api.assets[PHOTO]);other['id'] = str(uuid.uuid4());other['isTrashed'] = True
        self.f.api.assets[other['id']] = other
        with self.assertRaises(Conflict):Maintenance(self.f.engine).plan('rollback', [PHOTO])

    def test_missing_record_retains_last_known_path_for_manual_review(self):
        row = self.f.run()
        self.f.api.delete(row['video_id'])
        result = self.f.engine.reconcile()
        self.assertEqual(result[0]['status'], 'remote_video_unavailable_possible_disk_residue')
        self.assertIsNotNone(self.f.store.get(PHOTO)['original_path'])

    def test_migration_success_keeps_photo_and_changes_video(self):
        row = self.f.run();m = Maintenance(self.f.engine)
        proof = str(uuid.uuid4())
        source = dict(self.f.api.assets[PHOTO]);source['id'] = proof
        proof_video = self.f.api.upload(VIDEO, source, 'native-proof')['id']
        self.f.api.assets[proof_video]['originalPath'] = '/data/encoded-video/proof-MP.mp4'
        source['livePhotoVideoId'] = proof_video
        self.f.api.assets[proof] = source;self.f.api.data[proof] = SOURCE
        def native_refresh(pid):
            new = self.f.api.upload(VIDEO, self.f.api.get(pid), 'native')['id']
            self.f.api.assets[new]['originalPath'] = '/data/encoded-video/new-MP.mp4'
            self.f.api.link(pid, new)
        self.f.api.refresh_callback = native_refresh
        signed = m.plan('migrate', [PHOTO], proof)
        m.execute(signed, signed['confirmation'], True)
        self.assertEqual(self.f.store.get(PHOTO)['state'], 'migrated')
        self.assertEqual(self.f.store.get(PHOTO)['native_video_id'], self.f.api.get(PHOTO)['livePhotoVideoId'])
        self.assertEqual(self.f.engine.reconcile()[0]['status'], 'migrated_native')
        self.assertNotEqual(self.f.api.get(PHOTO)['livePhotoVideoId'], row['video_id'])
        self.assertEqual(self.f.api.original(PHOTO), SOURCE)
        self.assertIsNone(self.f.api.get(row['video_id']))

    def test_missing_delete_permission_fails_before_unlink(self):
        self.f.run();m = Maintenance(self.f.engine)
        signed = m.plan('rollback', [PHOTO])
        self.f.api.permissions.remove('asset.delete')
        links = self.f.api.links
        with self.assertRaises(BridgeError):m.execute(signed, signed['confirmation'], True)
        self.assertEqual(self.f.api.links, links)
        self.assertEqual(self.f.api.deletes, 0)

    def test_plan_expiry(self):
        self.f.run();m = Maintenance(self.f.engine)
        signed = m.plan('rollback', [PHOTO])
        with patch('motion_bridge.maintenance.time.time', return_value=signed['plan']['expires'] + 1):
            with self.assertRaises(BridgeError):m.execute(signed, signed['confirmation'], True)
        self.assertEqual(self.f.api.deletes, 0)

    def test_migration_requires_proof(self):
        self.f.run()
        with self.assertRaises(Conflict):Maintenance(self.f.engine).plan('migrate', [PHOTO])
        with self.assertRaises(Conflict):Maintenance(self.f.engine).native_proof(PHOTO)

    def test_interrupted_maintenance_can_replan(self):
        row = self.f.run();m = Maintenance(self.f.engine)
        signed = m.plan('rollback', [PHOTO])
        with patch.object(self.f.api, 'delete', side_effect=BridgeError('API_NETWORK_ERROR')):
            with self.assertRaises(BridgeError):m.execute(signed, signed['confirmation'], True)
        self.assertEqual(self.f.store.get(PHOTO)['state'], 'cleanup_pending')
        self.assertIsNone(self.f.api.get(PHOTO)['livePhotoVideoId'])
        retry = m.plan('rollback', [PHOTO]);m.execute(retry, retry['confirmation'], True)
        self.assertEqual(self.f.store.get(PHOTO)['state'], 'retired')

    def test_restore_after_interrupted_migration(self):
        row = self.f.run();m = Maintenance(self.f.engine)
        m.spool(row, VIDEO)
        self.f.api.link(PHOTO, None);self.f.api.delete(row['video_id'])
        self.f.store.update(PHOTO, 'migration_wait')
        m.restore_bridge(PHOTO, True)
        self.f.engine.run_one()
        self.assertEqual(self.f.store.get(PHOTO)['state'], 'linked')
        self.assertNotEqual(self.f.store.get(PHOTO)['video_id'], row['video_id'])


class JournalTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory();self.root = Path(self.temp.name)
        self.store = Store(self.root)

    def tearDown(self):self.temp.cleanup()

    def records(self):
        return [json.loads(line) for path in sorted((self.root / 'records').glob('*.jsonl')) for line in path.read_text().splitlines()]

    def test_daily_and_size_rotation_with_structured_snapshots(self):
        day = [1767225600.0]
        journal = Journal(self.store, 4096, clock=lambda: day[0])
        self.store.enqueue(PHOTO)
        for i in range(35):
            self.store.update(PHOTO, 'linked', video_id=str(uuid.uuid4()), video_sha256='a' * 64, event='test_event')
        journal.flush()
        files = list(journal.directory.glob('events-2026-01-01-*.jsonl'))
        self.assertGreater(len(files), 1)
        self.assertTrue(all(p.stat().st_size <= 4096 for p in files))
        day[0] += 86400
        self.store.update(PHOTO, 'retired', event='test_event');journal.flush()
        self.assertEqual(len(list(journal.directory.glob('events-2026-01-02-*.jsonl'))), 1)
        rows = self.records()
        self.assertEqual(len(rows), 37)
        self.assertEqual(rows[0]['state'], 'pending')
        self.assertEqual(rows[-1]['state'], 'retired')
        self.assertIn('video_sha256', rows[-1])
        self.assertEqual(len({r['event_id'] for r in rows}), len(rows))
        self.assertEqual(journal.flush(), 0)

    def test_configurable_daily_timezone(self):
        from datetime import datetime, timezone
        clock = datetime(2026, 1, 1, 16, 0, tzinfo=timezone.utc).timestamp()
        journal = Journal(self.store, 4096, clock=lambda: clock, timezone_name='Asia/Shanghai')
        self.store.enqueue(PHOTO);journal.flush()
        self.assertEqual(len(list(journal.directory.glob('events-2026-01-02-*.jsonl'))), 1)

    def test_exact_size_rotates_immediately(self):
        journal = Journal(self.store, 4096, clock=lambda: 1767225600)
        # {"x":""}\n 占 9 字节。
        record = {'x': 'a' * (4096 - len(b'{"x":""}\n'))}
        journal._append(record)
        files = sorted(journal.directory.glob('*.jsonl'))
        self.assertEqual([f.stat().st_size for f in files], [4096, 0])

    def test_incomplete_tail_recovery(self):
        journal = Journal(self.store, 4096)
        self.store.enqueue(PHOTO);journal.flush()
        file = next(journal.directory.glob('*.jsonl'))
        with file.open('ab') as f:f.write(b'{"incomplete":')
        self.store.audit('next', PHOTO);journal.flush()
        self.assertEqual(len(self.records()), 2)

    def test_crash_after_fsync_may_repeat_but_keeps_event_id(self):
        journal = Journal(self.store, 4096)
        self.store.enqueue(PHOTO)
        original = journal._append
        def crash(record):
            original(record)
            raise OSError('crash before cursor commit')
        with patch.object(journal, '_append', side_effect=crash):
            with self.assertRaises(OSError):journal.flush()
        self.assertFalse(journal.healthy)
        journal.flush()
        rows = self.records()
        self.assertEqual(len(rows), 2)
        self.assertEqual(rows[0]['event_id'], rows[1]['event_id'])
        self.assertTrue(journal.healthy)

    def test_private_paths_and_secrets_not_exported(self):
        journal = Journal(self.store, 4096)
        self.store.enqueue(PHOTO)
        self.store.update(PHOTO, original_path='/private/sensitive-file.mp4')
        self.store.audit('maintenance_before', PHOTO, {'ledger': self.store.get(PHOTO)})
        journal.flush()
        text = ''.join(p.read_text() for p in journal.directory.glob('*.jsonl'))
        self.assertNotIn('/private/', text)
        self.assertNotIn('plan_key', text)


class ConfigTests(unittest.TestCase):
    def test_url_rejects_credential_and_non_http(self):
        self.assertEqual(canonical_url('https://example.invalid/immich'), 'https://example.invalid/immich/api')
        for url in ('file:///tmp/x', 'https://user:pass@example.invalid', 'https://example.invalid/?token=x'):
            with self.assertRaises(BridgeError):canonical_url(url)

    def test_asset_id(self):
        self.assertEqual(asset_id(PHOTO), PHOTO)
        with self.assertRaises(BridgeError):asset_id('../../etc/passwd')

    def test_secret_file_validation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'secret';path.write_text('short')
            with self.assertRaises(BridgeError):secret(path)
            path.write_text('a' * 32 + '\n');self.assertEqual(secret(path), 'a' * 32)


if __name__ == '__main__':unittest.main()
