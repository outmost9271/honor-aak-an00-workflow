# Immich 容器服务与运维说明

> 本版本提供可构建的容器与保守的生产运维工具，但不会自动部署、创建工作流、修改数据库或挂载 Immich 媒体目录。已验证 Immich v3.4.0。没有跨客户端原子关联接口，不能承诺绝对消除所有并发竞态，也不能通过公开 API 证明磁盘文件已经物理清除。

## 1. 功能与默认安全边界

- 标准工作流通过鉴权 Webhook 提交照片 ID，持久排队后立即返回 202。
- 始终重新从固定 Immich 地址读取资源；不信任事件中的 URL、路径、所有者或 EXIF。
- 校验账号、机型、源校验值、荣耀尾标、视频轨道、时长、尺寸，并默认完整解码检查。
- 只无损提取并上传原始 MP4，保留音轨，不预先有损转码。播放兼容性由 Immich 自己处理。
- 默认 `observe`，不会上传或关联。正式写入必须同时设置 `BRIDGE_MODE=apply` 和 `IMMICH_WRITE_ENABLED=true`。
- 不覆盖已有动态关联；不认领已有或去重返回的视频；无法证明上传结果时转人工复核，不冒认所有权。
- 自动工作线程绝不删除远端资源。清理由独立、限时、签名、一次性的维护计划执行。
- 每个账号单独运行一份服务，使用独立状态目录与密钥；不支持外部图库、锁定照片和跨账号处理。
- 绑定首次使用的 API 地址和账号；更换实例/账号会拒绝执行，不能把旧台账直接指向另一个服务器。
- 默认只批准 v3.4.0。升级后未经显式批准不写入；增加允许版本之前应重新验证接口和原生支持情况。

## 2. 容器、宿主目录与配置

在可信的本地克隆仓库构建：

```bash
docker build -t honor-motion:0.2.0 .
```

镜像固定基础镜像摘要，运行时为非 root 用户 10001，使用 uv 和独立 Python 环境；不需要第三方 Python 包。镜像含 FFmpeg，源码采用 MIT，依赖软件分别遵守自己的许可证；发行包附带依赖版权说明。

准备目录和示例配置，**下面是待人工执行的部署说明，不是工具自动执行的操作**：

```bash
cp deployment.env.example deployment.env
sudo install -d -m 0700 -o 10001 -g 10001 state secrets
```

将自己在 Immich 中创建的 API 密钥写入 `secrets/immich_api_key`，将随机至少 24 位 ASCII 共享密钥写入 `secrets/webhook_secret`。推荐 32 字节以上随机值。不要把密钥写入命令行参数、环境示例、工作流分享文件或 Git。

两个文件需要让容器 UID 10001 可读，例如所有者 10001、权限 0400。目录和文件只挂载到当前服务，不能挂 Docker socket、SSH 密钥、生产数据库或整个媒体目录。

修改 `deployment.env` 中的 API 地址、已有 Docker 网络名称和宿主目录。示例明确使用 bind mount，而不是匿名卷：

```yaml
volumes:
  - type: bind
    source: '${BRIDGE_STATE_DIR:-./state}'
    target: /state
    bind:
      create_host_path: false
```

所有持久状态都在同一个宿主目录：

```text
state/
├── ledger.sqlite3          # 事务台账、队列、审计 outbox、计划密钥
├── ledger.sqlite3-wal      # 运行时可能存在，不能随意删
├── ledger.sqlite3-shm
├── process.lock            # 执行器排他锁
├── records/
│   ├── events-2026-01-01-000001.jsonl
│   ├── events-2026-01-01-000002.jsonl
│   └── events-2026-01-02-000001.jsonl
└── recovery/               # 显式维护时保存的原视频恢复副本
```

状态目录应使用支持 POSIX 锁和 fsync 的本地文件系统，例如 ext4/XFS。不要将 SQLite 活跃目录放在 NFS、CIFS、对象存储挂载或未经验证的分布式文件系统上。只允许一个执行器进程使用同一目录；可在该进程中配置 1–4 个线程，但不能多副本部署或跨主机共享状态卷。

示例容器根文件系统只读、丢弃全部 capabilities、限制内存/CPU/PID，并只为临时解码提供受限 tmpfs。默认 1 个工作线程和 1 GiB 内存；提高并发或图片大小上限时应相应提高资源预算。

## 3. 文件记录、字段和轮转规则

`/state/records/` 中的 JSONL 是正式持久记录，不是仅供查看的 Docker stdout。宿主机可直接读取 `state/records/`，每行一个完整 JSON 对象：

```json
{
  "schema_version": 1,
  "event_id": "<installation-uuid>:<event-uuid>",
  "sequence": 42,
  "timestamp": "2026-01-01T12:00:00+00:00",
  "event": "link_confirmed",
  "photo_id": "<photo-uuid>",
  "video_id": "<bridge-video-uuid>",
  "native_video_id": null,
  "source_sha1": "<base64-sha1>",
  "source_sha256": "<hex-sha256>",
  "video_sha1": "<base64-sha1>",
  "video_sha256": "<hex-sha256>",
  "video_bytes": 1234567,
  "state": "linked",
  "attempts": 1,
  "error": null,
  "created_by_us": 1,
  "context": {}
}
```

未提取/未上传时相关字段为 null。迁移后 video_id 保留原桥接视频来源，native_video_id 保存新的原生视频 ID，避免把已删除的旧视频误当成当前关联。SHA-1 与 Immich 的校验值格式一致；SHA-256 用于额外内容校验。记录不包含 API 密钥、Webhook 密钥、提升会话、GPS、EXIF、原始文件名或媒体路径。SQLite 内部台账会保存必要的最近已知路径和维护快照，整个目录仍然属于敏感数据。

轮转规则：

1. **日期变化**：下一条记录写入新日期的文件，默认 UTC；可用 `RECORD_TIMEZONE=Asia/Shanghai` 等 IANA 时区修改。事件内 timestamp 始终为 UTC。没有新事件时不为了跨日制造空记录。
2. **大小阈值**：默认 `RECORD_MAX_BYTES=16777216`，即 16 MiB；允许 4 KiB–1 GiB。
3. 一条记录会使文件超过阈值时，先轮转再写，保持单行完整；精确达到阈值时立即创建下一分卷。
4. 每条写入后 flush + fsync，再提交 SQLite 导出游标；文件写入失败会暂停工作线程、使就绪检查失败，不静默丢弃事件。
5. 事务状态和事件在 SQLite 中一起提交，随后导出 JSONL。崩溃可能发生在文件落盘和游标提交之间，因此承诺**至少一次导出**而不是跨两个存储系统的 exactly-once。后续程序用 `event_id` 去重；同一事件重放保留相同 ID。`sequence` 用于当前台账内排序，不能单独当作跨备份恢复的全局唯一标识。
6. 不完整尾行会在下一次追加前修复，然后从事务 outbox 重放。不要对活跃文件使用外部 copytruncate/logrotate。

按照片取得当前状态：按 sequence 读取去重事件，取该 photo_id 的最后一条状态记录。事件字段保留发生时的事务快照，不会用当前状态冒充过去的状态。

SQLite 是当前状态和事务恢复源，不按天或大小拆库；按天/大小轮转的是可直接消费的 JSONL 文件。JSONL 是审计投影，不能单独代替完整事务台账恢复任务。

**默认不自动删除任何历史记录或审计数据。** 轮转不等于保留策略。应监控状态卷空间；SQLite outbox 本身也会增长。归档/删除旧 JSONL 前先备份台账，尽量停服务操作，不要删除活跃分卷或 WAL。需要时可从完整 outbox 重建 JSONL。

Docker 自己的 stdout 日志另设 10 MiB × 3 轮转，不能替代上述持久业务记录。

## 4. API 密钥与工作流

常驻服务使用当前照片所属账号创建的 API 密钥，不需要管理员权限：

- observe：`asset.read`、`asset.download`、`user.read`。
- apply：再加 `asset.upload`、`asset.update`。
- 清理/回退的独立维护密钥：再加 `asset.delete`。
- 原生迁移：另需 `job.create`。

服务会读取当前密钥权限并在写入前检查，不自动扩大权限。建议常驻密钥**不包含删除权限**。密钥文件轮换后重新创建容器以重新读取配置，账号和实例绑定不变。

在照片所属账号下手动创建标准工作流：

1. 触发器：`AssetMetadataExtraction`。
2. 图片类型筛选：`IMAGE`。
3. EXIF 筛选：`make` 精确匹配 `HONOR`。
4. EXIF 筛选：`model` 精确匹配目标型号。
5. `Trigger Webhook`：POST 到共同 Docker 网络中的 `http://honor-motion:8080/webhook`；Header name 为 `X-Motion-Secret`，值与秘密文件一致。

容器自己的 `localhost` 不是另一容器。Compose 示例加入一个**已经存在**的 Immich Docker 网络。跨主机时使用可信 TLS 反向代理，不能将明文 Webhook 暴露到公网。镜像不自动创建工作流，也不把真实工作流配置提交仓库。

观察模式检查通过后，人工调整两个写入开关，再重新创建服务。之前的 observed 项不会被普通重复事件自动重置；可使用 `enqueue --retry-terminal` 显式重新排队。

## 5. 运维命令

以下用 `docker compose -f compose.example.yaml --env-file deployment.env` 作为命令前缀，示例省略实际账号和资源 ID。

### 在线可用

```bash
# 只查看本地计数
docker compose -f compose.example.yaml --env-file deployment.env run --rm honor-motion status

# 提交历史照片 ID，或扫描本账号目标机型未关联图片
docker compose -f compose.example.yaml --env-file deployment.env run --rm honor-motion enqueue --photo <UUID>
docker compose -f compose.example.yaml --env-file deployment.env run --rm honor-motion enqueue --scan --retry-terminal

# SQLite 一致性在线备份；输出路径必须尚不存在
docker compose -f compose.example.yaml --env-file deployment.env run --rm honor-motion backup --output /state/ledger-backup.sqlite3
```

在线 enqueue 只写事务队列，正在运行的服务负责按序导出文件，避免两个进程同时轮转。

### 停服务后执行

```bash
# 先停止服务，不删除宿主状态目录
docker compose -f compose.example.yaml --env-file deployment.env stop honor-motion

# 手动处理当前到期队列；失败项遵循退避，不忙等到重试时间
docker compose -f compose.example.yaml --env-file deployment.env run --rm honor-motion work

# 只读远端对账；默认不读取锁定照片，不自动删除
docker compose -f compose.example.yaml --env-file deployment.env run --rm honor-motion reconcile

# 导出积压事件
docker compose -f compose.example.yaml --env-file deployment.env run --rm honor-motion export-records
```

冲突/失败不会被普通 Webhook 自动重置。审核问题后对目标照片显式 `enqueue --retry-terminal`。linked、retired、migrated、中断维护项不会被此命令重新激活，避免误恢复用户主动取消的关联。

上传请求超时或进程崩溃时，远端可能实际已创建视频，但本地尚未确认。若只收到去重结果，本工具不会认领、关联或删除该视频，状态为 conflict，需人工核对。不要因为文件名看起来相同就修改 created_by_us。

## 6. 维护、孤儿清理与退出

### 完整引用检查需要短期提升会话

实测 v3.4.0 的普通 API 密钥无法搜索锁定照片，且删除后的 GET 可能返回“没有访问权限或不存在”的 400，不能简单当作 404。为避免漏掉锁定照片的引用：

- 普通 reconcile 明确标记 `non_locked_only`，不把该范围内零引用当作全局零引用。
- **生成和执行删除计划必须提供同账号、已通过 PIN 临时解锁的短期会话令牌**，通过只读文件 `IMMICH_SCAN_SESSION_FILE` 传入。工具只允许该会话调用用户身份、解锁状态和元数据搜索接口，不允许用它上传、换绑、删除或改变 PIN。
- 令牌必须已由用户在 Immich 正常登录并临时解锁；工具不保存账号密码、不设置 PIN、不代用户解锁。没有 PIN/提升会话时，维护计划会安全拒绝；可继续正常处理新照片，但不能声称能够完整检查所有引用。
- 提升权限通常 15 分钟有效；过期、账号不同、分页失败都会中止。不要把它挂到长期运行服务或加入默认 Compose。

可以为临时维护容器增加单独只读挂载：

```text
--volume <local-session-file>:/run/secrets/scan_session:ro
--env IMMICH_SCAN_SESSION_FILE=/run/secrets/scan_session
```

同时将临时容器的 `IMMICH_API_KEY_FILE` 指向独立维护密钥。真实令牌只保存在受限秘密文件，不写入部署示例或审计文件。

### 计划与执行

必须停兼容层、暂停涉及目标资源的上传/客户端编辑/其他自动化，并人工确认维护窗口。进程锁只能阻止本工具另一执行器，不能锁住其他客户端；公开 API 没有 CAS 或全局事务。

```text
plan cleanup --photo <UUID> --output /state/cleanup-plan.json
plan rollback --photo <UUID> --output /state/rollback-plan.json
execute --plan /state/rollback-plan.json --confirm <plan-confirmation> --maintenance-confirmed
```

- cleanup：只清理已证明由本工具新建且无任何照片引用的视频，不解除照片关联。
- rollback：必要时取消本工具关联，再清理其视频，保留原照片。
- 每个计划最多 100 张，15 分钟过期，签名绑定实例、账号、台账、资源快照和引用列表；执行一次即失效。
- 默认清理宽限期 24 小时。`CLEANUP_GRACE_SECONDS=0` 仅用于隔离测试或明确评估过的紧急维护，不建议生产常态关闭。
- 删除前再次核对所有者、来源、源图实际 SHA-256、视频实际 SHA-256、是否已变成原生提取路径、是否还有锁定/归档/回收站等照片引用。
- 在状态卷保存 fsync 的恢复副本后才修改关联。计划陈旧、来源不明、已有去重视频、其他引用、API 错误均拒绝继续。
- 原生提取视频绝不由此清理路径删除。不直接改 Immich 数据库，不扫描或删除宿主媒体路径。
- API 删除确认只能证明资源记录不可再找到，**不能证明后台文件删除已经物理完成**。工具保留最近已知路径和审计信息供人工检查，不伪造“文件已清除”的结论。

若执行中断：重新根据当前状态生成计划，不重放旧计划。cleanup_pending/migration_wait 不会被工作线程自动接管。磁盘中恢复副本保留，状态和操作审计也保留。

### 将来原生支持后迁移

当前 v3.4.0 不识别普通荣耀尾标，不能删掉桥接视频指望它自动恢复。升级前应暂停服务，验证新版本，再显式更新允许版本。

迁移必须提供一张同机型、未被本工具处理、刚刚被目标版本原生识别的照片作为证据。工具检查其存储分类和视频与原图内嵌部分的字节一致性：

```text
plan migrate --photo <UUID> --native-proof-photo <untouched-native-photo-UUID> --output /state/migrate-plan.json
execute --plan /state/migrate-plan.json --confirm <confirmation> --maintenance-confirmed
```

迁移显式清除旧的普通上传视频，再重新提取并验证原生存储和视频字节；照片 ID 保留，视频 ID 会变化。原生识别未及时完成时进入 migration_wait，恢复副本不删，不能承诺未来版本实现与当前相同。

中断后可在确认原照片仍未关联、旧视频记录确实不在时，显式重新排队恢复兼容处理：

```text
restore-bridge --photo <UUID> --maintenance-confirmed
```

原生提取仍在运行时不要抢先恢复桥接，否则可能再次竞争关联。已出现任何其他关联时命令会拒绝覆盖。

退出/迁移完成且已确认备份后，可显式删除本工具状态卷中的恢复视频，原始审计仍保留：

```text
purge-recovery --photo <UUID> --confirm-video-sha256 <ledger-video-sha256>
```

“完全一致”仅指经过验证的原生业务行为：不是恢复所有原视频 ID、数据库审计和时间戳，更不是数据库逐字节回滚。

## 7. 备份、恢复与记录重建

需要一起考虑：

1. Immich 数据库、原始照片、普通上传视频和相应必要密钥/配置。
2. 本工具的完整状态卷、独立密钥文件以及部署参数。
3. 使用 SQLite backup API 得到一致性副本，不能只复制运行中的 ledger.sqlite3 而忽略 WAL。

JSONL 可以从完整审计 outbox 重建，所以在线 SQLite 备份是可靠的事务恢复源，但不是 Immich 本身的数据库/媒体一致性备份。要获得两个系统一致时间点，应暂停处理和业务写入，再分别备份；跨系统没有分布式事务。

恢复步骤：

1. 停止兼容层，恢复正确的 Immich 数据库/原始媒体和实例身份。
2. 将 SQLite 备份恢复为状态目录中的 ledger.sqlite3，移走旧 WAL/SHM；切勿在服务运行时替换。
3. 恢复独立秘密文件，校验目录归属和权限，不把台账指向其他账号或服务器。
4. 运行 `rebuild-records`：将旧 records 目录归档，重置导出游标，从 SQLite outbox 重建全部 JSONL。事件 UUID 保持不变，离线历史归档与重建结果可按 event_id 去重。
5. 先 observe/对账，确认来源及关联，再恢复 apply。不要把崩溃窗口里的未知去重结果强行认领。

只有 JPG、没有数据库时，荣耀内嵌视频内容仍可提取，但当前 Immich 不会自动恢复关联，旧照片 ID、相册等也不能仅靠 JPG 恢复。不要为了伪装原生而把桥接原视频搬到 encoded-video；该目录常被当缓存排除，当前版本又无法原生重建荣耀视频。

## 8. 参数与状态

| 环境变量 | 默认值 / 作用 |
| --- | --- |
| IMMICH_URL | 必填，可信固定 HTTP(S) 地址；不允许 URL 凭据、查询串或片段 |
| IMMICH_API_KEY_FILE | `/run/secrets/immich_api_key` |
| WEBHOOK_SECRET_FILE | `/run/secrets/webhook_secret` |
| STATE_DIR | `/state`，绑定宿主目录 |
| BRIDGE_MODE | `observe`；另可选 apply |
| IMMICH_WRITE_ENABLED | `false`；写入还需 apply |
| HONOR_MODEL | `AAK-AN00`；空值允许 HONOR 的其他机型 |
| ALLOWED_IMMICH_VERSIONS | `3.4.0`，逗号分隔精确版本，不接受预发布 |
| RECORD_MAX_BYTES | `16777216`，按日期或大小轮转 JSONL |
| RECORD_TIMEZONE | `UTC`，文件分日时区；例如 Asia/Shanghai |
| WORKERS | 1，允许 1–4；不能多进程共享同一卷 |
| MAX_QUEUE | 10000 个活跃任务，满时 Webhook 返回 503 |
| MAX_ATTEMPTS | 8，指数退避，达到后 dead |
| HTTP_TIMEOUT | 30 秒，单次请求 |
| MEDIA_TIMEOUT | 90 秒，单次媒体检查/原生迁移等待 |
| MAX_IMAGE_BYTES | 128 MiB，最高允许 512 MiB |
| MAX_VIDEO_SECONDS | 30 秒，最高允许 120 秒 |
| VERIFY_DECODE | true，完整解码检查 |
| REQUIRE_AUDIO | false；存在音轨时始终保留，true 时缺少音轨拒绝 |
| CLEANUP_GRACE_SECONDS | 86400，维护宽限期 |
| IMMICH_SCAN_SESSION_FILE | 默认不配置，仅临时完整引用扫描使用 |
| PORT | 8080，允许 1024–65535 |

主要状态：pending、processing、retry、observed、linked、skipped、conflict、dead、cleanup_pending、migration_wait、retired、migrated。

健康检查：`GET /healthz` 表示执行进程存活，`GET /readyz` 表示版本/实例检查与文件持久化正常。版本不批准、远端故障、导出失败会使就绪检查失败。应监控就绪状态、dead/conflict 数量、最老待处理时间、磁盘空间、恢复副本和记录积压；不要仅凭 Docker healthy 判断所有任务成功。

## 9. 已验证与未承诺

单元测试覆盖队列竞争、重启、失败重试、上传/关联响应丢失、去重所有权、版本/账号门禁、维护签名/过期/重放、原生迁移状态机、恢复副本、按天/大小轮转、尾行修复、重复事件 ID、鉴权与磁盘错误。

隔离容器测试使用新账号、合成 EXIF、纯色画面和正弦音，实际验证标准工作流、observe/apply、原图/视频字节、播放接口、在线备份、引用扫描、独立维护权限与签名回退。测试不会接受外部生产 URL，也不会使用私人照片。

```bash
uv run python -m unittest discover -s tests -v
# 需要 Docker、FFmpeg 和本地已构建的镜像；会创建并清理随机命名的隔离实例
uv run python tests/integration_container.py
```

没有承诺所有手机格式、所有客户端保存菜单、未来 Immich 版本、存储后端物理删除或跨账号可见性完全一致。未经独立安全审计；请保持基础镜像与 FFmpeg 安全更新，在获得授权后从少量照片开始验证，不直接全库启用。
