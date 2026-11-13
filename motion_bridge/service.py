"""鉴权 Webhook、持久队列执行与健康检查。"""
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import signal
import threading
import time

from .api import asset_id
from .config import BridgeError


class LimitedServer(ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 32

    def __init__(self, address, handler):
        self.slots = threading.BoundedSemaphore(32)
        super().__init__(address, handler)

    def process_request(self, request, address):
        if not self.slots.acquire(blocking=False):
            try:
                request.sendall(b'HTTP/1.0 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n')
            finally:
                self.shutdown_request(request)
            return
        try:
            super().process_request(request, address)
        except Exception:
            self.slots.release()
            raise

    def process_request_thread(self, request, address):
        try:
            super().process_request_thread(request, address)
        finally:
            self.slots.release()


class Runtime:
    def __init__(self, engine, journal):
        self.engine, self.journal = engine, journal
        self.stop = threading.Event()
        self.ready = False
        self.last_check = 0
        self.error = None
        self.workers = []

    def handler(self):
        runtime = self
        class Handler(BaseHTTPRequestHandler):
            server_version = 'honor-motion'
            sys_version = ''

            def setup(self):
                super().setup()
                self.connection.settimeout(10)

            def reply(self, status, body):
                data = json.dumps(body, separators=(',', ':')).encode()
                self.send_response(status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(data)))
                self.send_header('Cache-Control', 'no-store')
                self.end_headers()
                self.wfile.write(data)

            def do_GET(self):
                if self.path == '/healthz':
                    alive = not runtime.stop.is_set() and all(w.is_alive() for w in runtime.workers)
                    self.reply(200 if alive else 503, {'alive': alive})
                elif self.path == '/readyz':
                    ready = runtime.ready and runtime.journal.healthy and time.monotonic() - runtime.last_check < 90
                    self.reply(200 if ready else 503, {'ready': ready})
                else:
                    self.reply(404, {'error': 'NOT_FOUND'})

            def do_POST(self):
                if self.path != '/webhook':
                    self.reply(404, {'error': 'NOT_FOUND'})
                    return
                if not hmac.compare_digest(self.headers.get('X-Motion-Secret', '').encode('utf-8'), runtime.engine.config.webhook_secret.encode('utf-8')):
                    self.reply(401, {'error': 'UNAUTHORIZED'})
                    return
                if self.headers.get('Transfer-Encoding') or not self.headers.get('Content-Length'):
                    self.reply(411, {'error': 'CONTENT_LENGTH_REQUIRED'})
                    return
                try:
                    length = int(self.headers['Content-Length'])
                    if not 0 < length <= 65536:
                        self.reply(413, {'error': 'BODY_TOO_LARGE'})
                        return
                    event = json.loads(self.rfile.read(length))
                    if event.get('type') != 'AssetV1' or event.get('trigger') != 'AssetMetadataExtraction':
                        raise ValueError
                    pid = asset_id(event['data']['asset']['id'])
                    added = runtime.engine.store.enqueue(pid, runtime.engine.config.max_queue)
                    runtime.journal.flush()
                    self.reply(202, {'accepted': True, 'new': added})
                except BridgeError as exc:
                    self.reply(503 if exc.code == 'QUEUE_FULL' else 400, {'error': exc.code})
                except (ValueError, TypeError, KeyError, AttributeError):
                    self.reply(400, {'error': 'INVALID_EVENT'})
                except Exception:
                    runtime.ready = False
                    self.reply(503, {'error': 'PERSISTENCE_UNAVAILABLE'})

            def log_message(self, *args):
                # 不记录 URL、请求体、密钥或私人文件名。
                pass
        return Handler

    def worker(self):
        engine = self.engine
        while not self.stop.is_set():
            try:
                if time.monotonic() - self.last_check > 30:
                    engine.identity()
                    self.last_check = time.monotonic()
                self.journal.flush()
                self.ready = True
                worked = engine.run_one()
                self.journal.flush()
                self.error = None
                if not worked:
                    self.stop.wait(1)
            except Exception as exc:
                self.ready = False
                code = exc.code if isinstance(exc, BridgeError) else 'LOCAL_PERSISTENCE_OR_WORKER_ERROR'
                if self.error != code:
                    print(json.dumps({'event': 'worker_paused', 'code': code}), flush=True)
                    self.error = code
                self.stop.wait(5)

    def serve(self):
        self.engine.store.recover()
        self.journal.flush()
        self.workers = [threading.Thread(target=self.worker, name=f'worker-{i}') for i in range(self.engine.config.workers)]
        server = LimitedServer(('0.0.0.0', self.engine.config.port), self.handler())
        for worker in self.workers:
            worker.start()
        def stop(*args):
            self.stop.set()
            threading.Thread(target=server.shutdown, daemon=True).start()
        old = {}
        for sig in (signal.SIGTERM, signal.SIGINT):
            old[sig] = signal.signal(sig, stop)
        try:
            server.serve_forever(poll_interval=0.5)
        finally:
            self.stop.set()
            server.server_close()
            for worker in self.workers:
                worker.join()
            self.journal.flush()
            for sig, handler in old.items():
                signal.signal(sig, handler)
