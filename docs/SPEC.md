# Aether — 实现规格书 (SPEC)

> 本文档是 `docs/prd.md` 的实现伴生文档，不重复 PRD 中的背景、术语和产品描述。

## 1. 实现顺序

```
Phase 1  config ─────────────────────────────────────────┐
                                                          │
Phase 2  store (依赖 config) ────────────────────────────┤
                                                          │
Phase 3  auth (依赖 config) ─────────────────────────────┤
                                                          │
Phase 4  hub (依赖 store, auth) ─────────────────────────┤
                                                          │
Phase 5  api (依赖 hub, auth) ───────────────────────────┤
                                                          │
Phase 6  ws  (依赖 hub, auth) ───────────────────────────┘
```

每个 Phase 完成后必须通过该模块的验收测试才能进入下一阶段。主分支持续迭代。

## 2. 模块验收标准

### 2.1 `internal/config`

| # | 验收项 |
|---|--------|
| C-1 | 加载 YAML 配置文件并解析为结构体，缺失必填字段（`auth.jwt_signing_key`、`database.dsn`）返回错误 |
| C-2 | 环境变量覆盖标量值：`AETHER_<SECTION>_<KEY>` 覆盖对应配置项（仅字符串、数字、布尔、时长，不覆盖列表） |
| C-3 | 所有配置项提供 PRD 8.3 节定义的默认值，零配置启动仅缺 dsn 和 jwt_signing_key 报错 |
| C-4 | 配置结构体包含完整字段和验证方法（频道名正则、payload 大小上限 > 0 等） |

### 2.2 `internal/store`

| # | 验收项 |
|---|--------|
| S-1 | `RunMigrations`：空库启动后自动创建 `schema_migrations`、`channels`、`messages` 表及索引；重复调用幂等 |
| S-2 | 迁移版本：v1 = `schema_migrations` + `channels`，v2 = `messages` + 索引 |
| S-3 | `WriteMessage`：单事务写入，`FOR UPDATE` 行锁保证并发安全，seq_id 每频道单调递增 |
| S-4 | `WriteMessage` 幂等：相同 `(channel, idempotency_key)` 返回首次 seq_id，不重复写入，不递增 seq |
| S-5 | `WriteMessage`：idempotency_key 为空时不触发去重逻辑，正常写入 |
| S-6 | `ReadHistory`：返回 `seq_id > afterSeq` 的消息，按 `seq_id ASC` 排序，limit 上限 1000 |
| S-7 | `ReadHistory`：频道不存在时返回空切片，不返回错误 |
| S-8 | `EvictExpiredMessages`：按频道逐个清理 TTL 和 max_count，清理空频道，返回清理统计 |
| S-9 | `Ping`：验证 PG 连接可用 |
| S-10 | 集成测试需真实 PostgreSQL（Docker），不使用 mock |

### 2.3 `internal/auth`

| # | 验收项 |
|---|--------|
| A-1 | API Key 验证使用 `crypto/subtle.ConstantTimeCompare`，防计时攻击 |
| A-2 | JWT 仅接受 HS256 算法，拒绝 `none` 和其他算法 |
| A-3 | JWT 过期验证：拒绝过期 Token，允许 30s 时钟偏移 |
| A-4 | JWT channels 授权：精确匹配优先，`prefix.*` 匹配所有以 `prefix.` 开头的频道（递归不限层级），`["*"]` 授权所有 |
| A-5 | WebSocket 连接建立后 Token 过期不断开已有连接 |

### 2.4 `internal/hub`

| # | 验收项 |
|---|--------|
| H-1 | Hub 持有 Store 引用，`Publish` 方法内同步调用 `Store.WriteMessage`，成功后再遍历订阅者分发 |
| H-2 | 持久化失败时 Publish 返回错误，不推送任何消息 |
| H-3 | 频道注册表和订阅者映射使用 `sync.RWMutex` 保护（读多写少） |
| H-4 | 每个 WebSocket 连接有独立写 goroutine 和出站 channel（容量可配，默认 256） |
| H-5 | 出站缓冲区满时关闭连接（close 帧代码 1012） |
| H-6 | subscribe 重复订阅同一频道：首次生效（含 after_seq 回放），后续忽略，不报错 |
| H-7 | subscribe 带 `after_seq` 时：先完成全部历史回放，再将连接注册到实时订阅者集合 |
| H-8 | Gap 检测：`ReadHistory` 返回结果附带 `minSeq` 信息，由 Hub 判断是否需要发 gap 帧 |
| H-9 | Gap 判断逻辑：若 `after_seq < minSeq - 1`，说明存在不可覆盖的间隔，发送 gap 帧 |
| H-10 | 单次 subscribe 最多 100 个频道，单连接累计最多 1000 个频道，超出返回 error 帧 |
| H-11 | 未授权频道请求返回 error 帧，代码 40301 |

### 2.5 `internal/api`

| # | 验收项 |
|---|--------|
| API-1 | `POST /api/v1/publish`：完整实现 PRD 5.1 节请求/响应格式 |
| API-2 | `GET /api/v1/history`：完整实现 PRD 5.1 节请求/响应格式 |
| API-3 | 频道名称验证：1-128 字符，仅 `[a-zA-Z0-9_./-]`，不允许多个连续点，禁止以点开头或结尾 |
| API-4 | Payload 大小限制：可配置最大值（默认 64KB），超出返回 413 |
| API-5 | 错误响应统一格式：`{ "ok": false, "error": { "code": <int>, "message": "<string>" } }` |
| API-6 | `GET /healthz`：存储可写时返回 200 |
| API-7 | `GET /readyz`：服务器完全初始化后返回 200 |
| API-8 | `GET /metricsz`：Prometheus text 格式 |

### 2.6 `internal/ws`

| # | 验收项 |
|---|--------|
| WS-1 | WebSocket 升级在 `/ws?token=<jwt>`，无效 Token 返回 HTTP 401 |
| WS-2 | Origin 验证：默认拒绝所有跨源，`["*"]` 接受任意源，指定源列表精确匹配 |
| WS-3 | 帧 JSON 文本协议：subscribe / unsubscribe / message / subscribed / unsubscribed / gap / error |
| WS-4 | 心跳：仅 WebSocket 标准 ping/pong，服务器每 30s（可配）发 ping，2 个周期无 pong 断开 |
| WS-5 | 入站帧最大字节数可配（默认 64KB） |
| WS-6 | 优雅关闭：停止接受新连接，排空现有连接（发 close 帧代码 1001），超时后强制断开 |
| WS-7 | 未知帧类型返回 error 帧，代码 40004 |

## 3. 关键接口定义

### 3.1 Store 接口

```go
type Message struct {
    SeqID     int64
    Payload   json.RawMessage
    CreatedAt time.Time
}

type HistoryResult struct {
    Messages []Message
    MinSeq   int64 // 该频道最早可用消息的 seq_id，供 Hub 做 gap 判断
}

type Store interface {
    RunMigrations(ctx context.Context) error
    Ping(ctx context.Context) error
    WriteMessage(ctx context.Context, channel string, payload json.RawMessage, idempotencyKey *string) (seqID int64, timestamp time.Time, err error)
    ReadHistory(ctx context.Context, channel string, afterSeq int64, limit int) (*HistoryResult, error)
    EvictExpiredMessages(ctx context.Context) (channelsCleaned int, messagesEvicted int, err error)
}
```

### 3.2 Auth 接口

```go
type Claims struct {
    Subject  string   // sub
    Channels []string // channels 声明
}

type Auth interface {
    ValidateAPIKey(key string) bool
    ParseAndValidateToken(tokenString string) (*Claims, error)
    IsChannelAuthorized(claims *Claims, channel string) bool
}
```

### 3.3 Hub 接口

```go
type Hub interface {
    Publish(ctx context.Context, channel string, payload json.RawMessage, idempotencyKey *string) (seqID int64, timestamp time.Time, err error)
    Subscribe(conn *Connection, channels []string, afterSeq map[string]int64) error
    Unsubscribe(conn *Connection, channels []string)
    RemoveConnection(conn *Connection)
}
```

## 4. API 接口约定

> 完整定义见 `docs/prd.md` 第 5 节。此处仅列要点和错误码汇总。

### 4.1 发布消息

```
POST /api/v1/publish
Authorization: Bearer <api_key>
Content-Type: application/json

Request:  { "channel": string, "payload": any, "idempotency_key"?: string }
Success:  { "ok": true, "seq_id": int64, "timestamp": RFC3339 }
Error:    { "ok": false, "error": { "code": int, "message": string } }
```

### 4.2 检索历史

```
GET /api/v1/history?channel=<name>&after_seq=<n>&limit=<m>
Authorization: Bearer <api_key>

Success:  { "ok": true, "channel": string, "messages": [...], "has_more": bool }
Error:    { "ok": false, "error": { "code": int, "message": string } }
```

### 4.3 运维端点

```
GET /healthz  -> 200 "ok"
GET /readyz   -> 200 "ok" | 503 "not ready"
GET /metricsz -> Prometheus text format
```

### 4.4 错误码

| 代码 | 类别 | 描述 |
|------|------|------|
| 40001 | 请求 | 无效频道名称 |
| 40002 | 请求 | 缺少必填字段 |
| 40003 | 请求 | 无效 JSON |
| 40004 | 请求 | 未知帧类型 |
| 40101 | 认证 | 无效或缺失 API Key |
| 40102 | 认证 | 无效或过期 JWT Token |
| 40301 | 授权 | 未授权订阅该频道 |
| 41301 | 限制 | Payload 超过最大大小 |
| 42901 | 限流 | 请求过多（v2 预留） |
| 50301 | 存储 | 无法持久化消息 |

### 4.5 WebSocket 帧

客户端 → 服务器：

| 帧类型 | 字段 |
|--------|------|
| subscribe | `{ "type": "subscribe", "channels": [string], "after_seq"?: {string: int64} }` |
| unsubscribe | `{ "type": "unsubscribe", "channels": [string] }` |

服务器 → 客户端：

| 帧类型 | 字段 |
|--------|------|
| message | `{ "type": "message", "channel": string, "seq_id": int64, "timestamp": RFC3339, "payload": any }` |
| subscribed | `{ "type": "subscribed", "channels": [string] }` |
| unsubscribed | `{ "type": "unsubscribed", "channels": [string] }` |
| gap | `{ "type": "gap", "channel": string, "available_from_seq": int64, "requested_from_seq": int64, "message": string }` |
| error | `{ "type": "error", "code": int, "message": string }` |

## 5. 数据模型与关键 SQL

> 完整 Schema 见 `docs/prd.md` 第 8.2 节。

### 5.1 表结构

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS channels (
    name        TEXT PRIMARY KEY,
    current_seq BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS messages (
    id              BIGSERIAL PRIMARY KEY,
    channel         TEXT NOT NULL REFERENCES channels(name),
    seq_id          BIGINT NOT NULL,
    payload         JSONB NOT NULL,
    idempotency_key TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (channel, seq_id),
    UNIQUE (channel, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_messages_channel_seq ON messages (channel, seq_id);
CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages (created_at);
```

### 5.2 WriteMessage 事务

```sql
-- Step 1: 确保频道存在
INSERT INTO channels (name) VALUES ($1) ON CONFLICT (name) DO NOTHING;

-- Step 2: 锁定频道行并读取当前序列号
SELECT current_seq FROM channels WHERE name = $1 FOR UPDATE;

-- Step 3: 插入消息（Go 侧计算 newSeq = current_seq + 1）
INSERT INTO messages (channel, seq_id, payload, idempotency_key)
VALUES ($1, $2, $3, $4)
ON CONFLICT (channel, idempotency_key) DO NOTHING
RETURNING seq_id, created_at;

-- Step 3b: 若 RETURNING 为空（幂等冲突），查询已有消息
SELECT seq_id, created_at FROM messages WHERE channel = $1 AND idempotency_key = $2;

-- Step 4: 推进序列号（幂等冲突时跳过）
UPDATE channels SET current_seq = current_seq + 1, updated_at = now() WHERE name = $1;
```

### 5.3 ReadHistory

```sql
SELECT seq_id, payload, created_at FROM messages
WHERE channel = $1 AND seq_id > $2
ORDER BY seq_id ASC LIMIT $3;
```

### 5.4 EvictExpiredMessages

```sql
-- 按 TTL 驱逐
DELETE FROM messages WHERE channel = $1 AND created_at < now() - $2::interval;

-- 按最大计数驱逐
DELETE FROM messages WHERE channel = $1 AND seq_id <= ($2 - $3);

-- 清理空频道
DELETE FROM channels WHERE NOT EXISTS (SELECT 1 FROM messages WHERE messages.channel = channels.name);
```

## 6. 技术决策摘要

| 决策 | 选择 | 理由 |
|------|------|------|
| Hub 与 Store 耦合方式 | Hub 直接持有 Store 引用 | v1 简单优先，PRD 未要求解耦 |
| Publish 分发时机 | 同步写 Store → 成功后异步推订阅者 | PRD 7.3：先持久化再分发 |
| Hub 并发模型 | `sync.RWMutex` + 每连接独立写 goroutine | 读多写少场景，简单有效 |
| WriteMessage 并发控制 | `SELECT ... FOR UPDATE` 行锁 | v1 有意简化，v2 可优化 |
| 重复订阅行为 | 首次生效（含 after_seq），后续忽略 | PRD "忽略"原意，实现最简 |
| Gap 检测 | ReadHistory 返回 MinSeq，Hub 判断 | 职责分离，Store 不做业务判断 |
| 迁移版本拆分 | v1: schema_migrations + channels；v2: messages + 索引 | 逐步构建，便于回滚 |
| 环境变量覆盖 | 仅标量（字符串/数字/布尔/时长），不覆盖列表 | 列表覆盖语义不明确，v1 不做 |
| Prometheus histogram | v1 使用默认桶，v2 根据实际延迟分布调整 | 先跑起来再优化 |
| 集成测试 | 真实 PostgreSQL（Docker） | mock 无法验证 SQL 正确性 |
| WebSocket close 代码 | 缓冲区满: 1012，优雅关闭: 1001，Token 无效: HTTP 401（非 WS close） | PRD 已定义 |
