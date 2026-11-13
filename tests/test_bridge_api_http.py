import io
import json
import threading
import time
import unittest
from urllib.error import HTTPError
from urllib.request import ProxyHandler, Request, build_opener
from unittest.mock import patch
import tempfile

from bridge_fakes import Fixture, OWNER, PHOTO
from motion_bridge.api import APIError, Immich, NoRedirect
from motion_bridge.config import BridgeError
from motion_bridge.service import LimitedServer, Runtime


class APITests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory();self.f = Fixture(self.temp.name)
        self.client = Immich(self.f.config)

    def tearDown(self):self.temp.cleanup()

    def test_full_scan_requires_elevated_session(self):
        with self.assertRaises(BridgeError):list(self.client.all_photos(full=True))

    def test_missing_and_denied_cannot_be_assumed_physical_deletion(self):
        error = APIError(400, missing_or_denied=True)
        with patch.object(self.client, 'request', side_effect=error):
            self.assertIsNone(self.client.get(PHOTO))

    def test_other_bad_request_does_not_mean_missing(self):
        with patch.object(self.client, 'request', side_effect=APIError(400)):
            with self.assertRaises(APIError):self.client.get(PHOTO)

    def test_search_cursor_pagination(self):
        responses = [{'assets': {'items': [{'id': PHOTO}], 'nextCursor': 'next'}},
                     {'assets': {'items': [], 'nextCursor': None, 'nextPage': None}}]
        with patch.object(self.client, 'request', side_effect=responses) as call:
            self.assertEqual(list(self.client.search({'type': {'eq': 'IMAGE'}})), [{'id': PHOTO}])
            self.assertEqual(call.call_args_list[1].args[2]['cursor'], 'next')

    def test_search_incomplete_and_loop_fail_closed(self):
        for response in ({'assets': {'items': [], 'nextPage': '2'}},
                         {'assets': {'items': [], 'nextCursor': 'same'}}, {'assets': {}}):
            with patch.object(self.client, 'request', return_value=response):
                with self.assertRaises(BridgeError):list(self.client.search({}))

    def test_redirect_disallowed(self):
        self.assertIsNone(NoRedirect().redirect_request(None, None, 302, '', {}, 'https://evil.invalid'))

    def test_scan_session_not_allowed_for_writes(self):
        from dataclasses import replace
        client = Immich(replace(self.f.config, scan_session='s' * 32))
        with self.assertRaises(BridgeError):client.request('DELETE', '/assets', scan=True)


class HTTPTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory();self.f = Fixture(self.temp.name)
        self.runtime = Runtime(self.f.engine, self.f.journal)
        self.server = LimitedServer(('127.0.0.1', 0), self.runtime.handler())
        self.thread = threading.Thread(target=self.server.serve_forever);self.thread.start()
        self.url = 'http://127.0.0.1:' + str(self.server.server_address[1])
        self.opener = build_opener(ProxyHandler({}))

    def tearDown(self):
        self.server.shutdown();self.server.server_close();self.thread.join();self.temp.cleanup()

    def request(self, path, body=None, secret=True):
        headers = {'X-Motion-Secret': self.f.config.webhook_secret} if secret else {}
        data = json.dumps(body).encode() if body is not None else None
        request = Request(self.url + path, data=data, headers=headers)
        try:
            with self.opener.open(request, timeout=3) as r:return r.status, json.loads(r.read())
        except HTTPError as exc:return exc.code, json.loads(exc.read())

    def test_authorized_queue_is_persistent_and_deduplicated(self):
        event = {'type': 'AssetV1', 'trigger': 'AssetMetadataExtraction', 'data': {'asset': {'id': PHOTO, 'originalPath': 'https://evil.invalid'}}}
        self.assertEqual(self.request('/webhook', event)[0], 202)
        self.assertFalse(self.request('/webhook', event)[1]['new'])
        self.assertEqual(len(self.f.store.rows()), 1)
        self.assertTrue(list(self.f.journal.directory.glob('*.jsonl')))
        self.assertNotIn('evil.invalid', self.f.store.get(PHOTO).values())

    def test_unauthorized_invalid_and_oversize(self):
        self.assertEqual(self.request('/webhook', {}, secret=False)[0], 401)
        self.assertEqual(self.request('/webhook', {})[0], 400)
        self.assertEqual(self.request('/webhook', {'padding': 'x' * 70000})[0], 413)
        self.assertEqual(self.f.store.rows(), [])

    def test_readiness_is_not_liveness(self):
        self.assertEqual(self.request('/healthz')[0], 200)
        self.assertEqual(self.request('/readyz')[0], 503)
        self.runtime.ready = True;self.runtime.last_check = time.monotonic()
        self.assertEqual(self.request('/readyz')[0], 200)
        self.f.journal.healthy = False
        self.assertEqual(self.request('/readyz')[0], 503)

    def test_outbox_survives_file_export_failure(self):
        event = {'type': 'AssetV1', 'trigger': 'AssetMetadataExtraction', 'data': {'asset': {'id': PHOTO}}}
        with patch.object(self.f.journal, 'flush', side_effect=OSError('disk full')):
            self.assertEqual(self.request('/webhook', event)[0], 503)
        self.assertIsNotNone(self.f.store.get(PHOTO))
        self.f.journal.flush()
        self.assertTrue(list(self.f.journal.directory.glob('*.jsonl')))


if __name__ == '__main__':unittest.main()
