"""容器入口与显式运维命令。"""
import argparse
import json
import os
from pathlib import Path
import time
import urllib.request

from . import __version__
from .api import Immich, asset_id
from .config import BridgeError, Config
from .engine import Engine
from .journal import Journal
from .maintenance import Maintenance
from .service import Runtime
from .store import ProcessLock, Store


def output(value, path=None):
    text = json.dumps(value, ensure_ascii=False, indent=2) + '\n'
    if path:
        with Path(path).open('x', encoding='utf-8') as file:
            file.write(text)
            file.flush()
            os.fsync(file.fileno())
    else:
        print(text, end='')


def parser():
    p = argparse.ArgumentParser(description='Immich 荣耀动态照片兼容服务与维护工具')
    p.add_argument('--version', action='version', version=__version__)
    sub = p.add_subparsers(dest='command', required=True)
    sub.add_parser('serve', help='运行 Webhook 与持久工作队列')
    sub.add_parser('healthcheck', help='仅检查本容器健康，不访问 Immich')
    sub.add_parser('status', help='查看本地台账状态计数')
    enqueue = sub.add_parser('enqueue', help='导入待检查照片，不立即调用写入 API')
    group = enqueue.add_mutually_exclusive_group(required=True)
    group.add_argument('--photo', nargs='+')
    group.add_argument('--scan', action='store_true', help='扫描当前账号目标机型的未关联图片')
    enqueue.add_argument('--retry-terminal', action='store_true', help='重新检查 observed/skipped/conflict/dead 等终止项')
    work = sub.add_parser('work', help='停止服务后执行本地队列')
    work.add_argument('--once', action='store_true')
    rec = sub.add_parser('reconcile', help='只读远端对账，不自动删除或换绑')
    rec.add_argument('--output', type=Path)
    backup = sub.add_parser('backup', help='SQLite 在线一致性备份，不复制裸 WAL 数据库')
    backup.add_argument('--output', type=Path, required=True)
    sub.add_parser('export-records', help='把已提交事件导出为按天/大小轮转的 JSONL')
    sub.add_parser('rebuild-records', help='停止服务后归档旧记录目录，并从 SQLite 全量重建 JSONL')
    plan = sub.add_parser('plan', help='生成只读维护计划，15 分钟有效')
    plan.add_argument('operation', choices=('cleanup', 'rollback', 'migrate'))
    plan.add_argument('--photo', nargs='+', required=True)
    plan.add_argument('--native-proof-photo', help='迁移要求同机型、未经兼容层处理的原生识别样本')
    plan.add_argument('--output', type=Path, required=True)
    execute = sub.add_parser('execute', help='执行已签名计划，必须停服务并处于维护窗口')
    execute.add_argument('--plan', type=Path, required=True)
    execute.add_argument('--confirm', required=True)
    execute.add_argument('--maintenance-confirmed', action='store_true', required=True)
    restore = sub.add_parser('restore-bridge', help='中断维护后重新排队恢复桥接；不覆盖现有关联')
    restore.add_argument('--photo', required=True)
    restore.add_argument('--maintenance-confirmed', action='store_true', required=True)
    purge = sub.add_parser('purge-recovery', help='显式删除已完成维护的本地恢复副本，不删除审计')
    purge.add_argument('--photo', required=True)
    purge.add_argument('--confirm-video-sha256', required=True)
    return p


def main(argv=None):
    os.umask(0o077)
    args = parser().parse_args(argv)
    if args.command == 'healthcheck':
        try:
            port = int(os.environ.get('PORT', '8080'))
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            with opener.open(f'http://127.0.0.1:{port}/healthz', timeout=3) as response:
                return 0 if response.status == 200 else 1
        except Exception:
            return 1
    try:
        config = Config.load()
        store = Store(config.state_dir)
        journal = Journal(store, config.record_max_bytes, timezone_name=config.record_timezone)
        engine = Engine(config, store, Immich(config), journal)
        maint = Maintenance(engine)
        if args.command == 'status':
            output(store.stats())
        elif args.command == 'backup':
            store.backup(args.output)
            output({'status': 'backed_up', 'records_can_be_rebuilt_from_outbox': True})
        elif args.command == 'enqueue':
            count = 0
            if args.scan:
                engine.identity()
                ids = (p['id'] for p in engine.api.all_photos()
                       if p.get('ownerId') == engine.owner and not p.get('isTrashed') and not p.get('livePhotoVideoId')
                       and (p.get('exifInfo') or {}).get('make', '').upper() == 'HONOR'
                       and (not config.model or (p.get('exifInfo') or {}).get('model') == config.model))
            else:
                ids = (asset_id(p) for p in args.photo)
            for pid in ids:
                count += store.enqueue(pid, config.max_queue, args.retry_terminal)
            # 正在运行的服务负责导出文件，避免另一个进程同时轮转。
            output({'newly_queued': count, 'durable_outbox': True})
        else:
            with ProcessLock(config.state_dir):
                if args.command == 'serve':
                    Runtime(engine, journal).serve()
                elif args.command == 'work':
                    store.recover()
                    journal.flush()
                    while engine.run_one():
                        journal.flush()
                        if args.once:
                            break
                    stats = store.stats()
                    output(stats)
                    if any(stats['states'].get(s, 0) for s in ('retry', 'dead', 'conflict', 'processing')):
                        journal.flush()
                        return 1
                elif args.command == 'reconcile':
                    output(engine.reconcile(), args.output)
                elif args.command == 'plan':
                    plan = maint.plan(args.operation, args.photo, args.native_proof_photo)
                    output(plan, args.output)
                    output({'confirmation': plan['confirmation'], 'expires': plan['plan']['expires'], 'actions': len(plan['plan']['actions'])})
                elif args.command == 'execute':
                    if args.plan.stat().st_size > 2 * 1024 * 1024:
                        raise BridgeError('PLAN_TOO_LARGE')
                    signed = json.loads(args.plan.read_text())
                    output(maint.execute(signed, args.confirm, args.maintenance_confirmed))
                elif args.command == 'restore-bridge':
                    maint.restore_bridge(args.photo, args.maintenance_confirmed)
                    output({'status': 'queued'})
                elif args.command == 'purge-recovery':
                    maint.purge_recovery(args.photo, args.confirm_video_sha256)
                    output({'status': 'local_recovery_purged'})
                elif args.command == 'rebuild-records':
                    directory = journal.directory
                    # 先重置游标；即使随后崩溃，最多重复导出，不会留下空目录却跳过旧事件。
                    with store.connection() as db:
                        db.execute('DELETE FROM meta WHERE key=?', ('record_cursor',))
                    directory.rename(config.state_dir / ('records-archive-' + str(time.time_ns())))
                    directory.mkdir(mode=0o700)
                    output({'exported': journal.flush()})
                elif args.command == 'export-records':
                    output({'exported': journal.flush()})
                journal.flush()
        return 0
    except BridgeError as exc:
        output({'error': exc.code})
        return 1
    except Exception:
        output({'error': 'LOCAL_OR_UNEXPECTED_ERROR'})
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
