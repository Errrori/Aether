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

## 7. v2 实现规格

> v2 在 v1 单节点消息推送的基础上，增加动态 Key 管理、多消息源接入、集群扩展、多种消费方式和运维可见性。

### 7.1 v2 实现顺序

```
第1层  地基       Key CRUD (FR-2.1)
第2层  消息入口    Webhook (FR-2.5) + Batch Publish API (FR-2.4 拆分) + MQ 桥接设计文档
第3层  保护       速率限制 (FR-2.8)
第4层  扩展       集群模式 (FR-2.2) + Presence (FR-2.3)
第5层  消费体验    SSE (FR-2.9) + 消息确认 (FR-2.6) + MQ 桥接实现
第6层  运维可见    管理面板 (FR-2.7) + 批量操作 UI (FR-2.4 拆分)
```

依赖关系：
- 第1层（Key CRUD）为所有后续层提供认证和权限基础设施
- 第2层（消息入口）依赖第1层的 Key 管理来验证消息源身份
- 第3层（速率限制）依赖第1层的 Key 标识作为限流维度
- 第4层（集群模式）依赖第1层的 Key 模型可跨节点共享
- MQ 桥接设计文档在第2层交付，实现在第5层：待第4层集群跑稳后，MQ Consumer 直接调用 hub.Publish，LISTEN/NOTIFY 透明生效
- Batch Publish API 端点在第2层（消息入口），管理面板的批量操作 UI 在第6层

### 7.2 第1层：动态 API Key CRUD

#### 7.2.1 模块：`internal/keymgmt`

新增模块，负责 API Key 的存储和生命周期管理。与 `internal/auth` 配合：auth 负责验证，keymgmt 负责管理。

| # | 验收项 |
|---|--------|
| K-1 | `POST /api/v2/keys`：创建 Key，请求体 `{ "name": string, "publish": [string], "subscribe": [string], "admin": bool, "expires_in"?: string }`，返回完整 Key 明文（仅此一次）和元数据 |
| K-2 | `GET /api/v2/keys`：列出所有 Key 的元数据（id、name、key_prefix、permissions、created_at、expires_at、revoked_at），不含 key_hash |
| K-3 | `GET /api/v2/keys/{id}`：返回单个 Key 完整元数据 |
| K-4 | `DELETE /api/v2/keys/{id}`：撤销 Key，设置 `revoked_at`，已撤销 Key 的后续请求拒绝 |
| K-5 | `POST /api/v2/keys/{id}/rotate`：轮换 Key，生成新密钥并返回明文，旧密钥立即失效 |
| K-6 | Key 明文格式：前缀 `aek_` + 32 字节加密随机数 Base64url 编码 = 47 字符，遵循 NFR-9 |
| K-7 | 存储时仅保存 SHA-256 哈希，不保存明文。创建/轮换时返回明文后不可再次获取 |
| K-8 | 创建 Key 时检查 name 唯一性，重复返回 409 |
| K-9 | 管理端点认证：请求者必须持有 `admin: true` 的 Key，否则返回 40302 |
| K-10 | 至少一个管理员 Key 存在：bootstrap 阶段从配置文件迁移已有 Key 到 `api_keys` 表，第一个 Key 标记为 admin |
| K-11 | `auth.ValidateAPIKey` 改为查询 `api_keys` 表（通过 key_hash），结果缓存到内存（sync.Map），撤销/轮换时淘汰缓存 |
| K-12 | `auth.ValidateAPIKey` 签名从 `bool` 升级为 `(KeyValidationResult, error)`，返回 Key ID 和权限集合供后续授权检查使用 |
| K-13 | Key 的权限模型：`publish` 列表控制可发布频道（支持前缀通配符，复用 `prefix.*` 规则），`subscribe` 列表控制可订阅频道，`admin` 标志控制管理端点访问 |
| K-14 | 配置文件中仍可定义静态 Key（向后兼容），启动时自动迁移到 `api_keys` 表（幂等：已存在则跳过） |

#### 7.2.2 关键接口

```go
// KeyPermissions 定义 Key 的权限范围
type KeyPermissions struct {
    Publish   []string `json:"publish"`   // 可发布频道模式，空列表 = 禁止发布
    Subscribe []string `json:"subscribe"` // 可订阅频道模式，空列表 = 禁止订阅
    Admin     bool     `json:"admin"`     // 是否管理员
}

// KeyMeta 是 Key 的公开元数据（不含哈希和明文）
type KeyMeta struct {
    ID          string         `json:"id"`
    Name        string         `json:"name"`
    KeyPrefix   string         `json:"key_prefix"`   // "aek_xxxxxxxx" 用于识别
    Permissions KeyPermissions `json:"permissions"`
    CreatedAt   time.Time      `json:"created_at"`
    ExpiresAt   *time.Time     `json:"expires_at,omitempty"`
    RevokedAt   *time.Time     `json:"revoked_at,omitempty"`
}

// CreatedKey 是创建/轮换 Key 的返回值
type CreatedKey struct {
    Key  string  `json:"key"` // 完整明文，仅此一次
    Meta KeyMeta `json:"meta"`
}

// KeyManager 管理 API Key 的生命周期
type KeyManager interface {
    CreateKey(ctx context.Context, name string, perms KeyPermissions, expiresIn *time.Duration) (*CreatedKey, error)
    ListKeys(ctx context.Context) ([]KeyMeta, error)
    GetKey(ctx context.Context, id string) (*KeyMeta, error)
    RotateKey(ctx context.Context, id string) (*CreatedKey, error)
    RevokeKey(ctx context.Context, id string) error
}

// KeyValidationResult 是 ValidateAPIKey 的返回值（替代原 bool）
type KeyValidationResult struct {
    Valid       bool
    KeyID       string
    Permissions KeyPermissions
}
```

#### 7.2.3 API 端点

```
POST /api/v2/keys
Authorization: Bearer <admin_api_key>
Content-Type: application/json

Request:  { "name": string, "publish": [string], "subscribe": [string], "admin": bool, "expires_in"?: string }
Success:  { "ok": true, "key": string, "meta": { ... } }

GET /api/v2/keys
Authorization: Bearer <admin_api_key>
Success:  { "ok": true, "keys": [ { ... } ] }

GET /api/v2/keys/{id}
Authorization: Bearer <admin_api_key>
Success:  { "ok": true, "meta": { ... } }

DELETE /api/v2/keys/{id}
Authorization: Bearer <admin_api_key>
Success:  { "ok": true }

POST /api/v2/keys/{id}/rotate
Authorization: Bearer <admin_api_key>
Success:  { "ok": true, "key": string, "meta": { ... } }
```

#### 7.2.4 数据模型

```sql
CREATE TABLE IF NOT EXISTS api_keys (
    id          TEXT PRIMARY KEY,            -- UUID v4
    name        TEXT NOT NULL UNIQUE,        -- 人类可读标签
    key_hash    TEXT NOT NULL UNIQUE,        -- SHA-256(key)
    key_prefix  TEXT NOT NULL,               -- key 前 8 字符，用于日志和识别
    permissions JSONB NOT NULL DEFAULT '{}', -- { publish: [...], subscribe: [...], admin: bool }
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ,                 -- NULL = 永不过期
    revoked_at  TIMESTAMPTZ                  -- NULL = 有效
);

CREATE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys (key_hash);
```

迁移版本：v3（v1 = schema_migrations + channels，v2 = messages + 索引，v3 = api_keys）。

#### 7.2.5 技术决策

| 决策 | 选择 | 理由 |
|------|------|------|
| Key 哈希算法 | SHA-256 | API Key 是机器凭证非用户密码，不需要 bcrypt 的慢哈希；SHA-256 查找快速，恒定时间比较 + DB 索引保证安全性 |
| Key 标识方式 | 前缀 `aek_` + Base64url | 前缀便于日志审计和 Key 扫描，Base64url 遵循 NFR-9 |
| 配置文件 Key 迁移 | 启动时幂等迁移到 api_keys 表 | 向后兼容，现有部署无需改配置；幂等保证重复启动安全 |
| ValidateAPIKey 缓存 | sync.Map 内存缓存，撤销/轮换时主动淘汰 | 避免每条消息都查库；Key 变更频率低，主动淘汰保证即时生效 |
| 管理端点路径 | `/api/v2/keys` | v2 API 版本化路径，与 v1 发布端点清晰分离 |
| 管理员区分 | 通过 Key 的 `admin` 权限字段 | 统一权限模型，Key 自身携带角色信息 |
| 权限存储格式 | JSONB，字段为 `["pattern", ...]` | 灵活可扩展，前缀通配符复用 auth 模块 `prefix.*` 匹配逻辑 |

#### 7.2.6 新增错误码

| 代码 | 类别 | 描述 |
|------|------|------|
| 40302 | 授权 | 非管理员尝试访问 Key 管理端点 |
| 40401 | 资源 | 指定 Key 不存在 |
| 40901 | 冲突 | Key name 重复 |

### 7.3 第2层：消息入口

#### 7.3.1 模块：`internal/webhook`

新增模块，负责 Webhook 配置的 CRUD 以及入站 Webhook 的接收和转发。

| # | 验收项 |
|---|--------|
| WH-1 | `POST /api/v2/webhooks`：创建 Webhook，请求体 `{ "name", "channel_template", "key_id" }`，返回 Webhook 元数据和 HMAC secret 明文（仅此一次） |
| WH-2 | `GET /api/v2/webhooks`：列出所有 Webhook 元数据（不含 secret） |
| WH-3 | `GET /api/v2/webhooks/{id}`：返回单个 Webhook 元数据 |
| WH-4 | `DELETE /api/v2/webhooks/{id}`：删除 Webhook 及其投递记录 |
| WH-5 | `POST /api/v2/webhooks/{id}/rotate-secret`：轮换 HMAC secret，返回新 secret 明文，旧 secret 立即失效 |
| WH-6 | `POST /api/v2/webhooks/{url_token}`：入站 Webhook 接收端点（无 auth middleware），通过 HMAC-SHA256 签名验证 |
| WH-7 | HMAC 签名验证：请求头 `X-Signature-256` 携带 hex 编码签名，可选 `sha256=` 前缀 |
| WH-8 | Channel 模板引擎：`{path.to.field}` 点号路径从 JSON payload 提取值，支持多占位符 |
| WH-9 | 签名无效返回 40103，Webhook 不存在返回 40402，模板解析失败返回 40005 |
| WH-10 | Webhook 管理端点使用 admin middleware，需要 admin Key |
| WH-11 | 投递记录：每次入站调用自动记录到 `webhook_deliveries` 表（status、duration、seq_id 等） |
| WH-12 | HMAC secret 以原始形式存储（非哈希），因为 HMAC 验签需要共享密钥 |

#### 7.3.2 模块：Batch Publish API

扩展现有 `internal/api`，新增批量发布端点。

| # | 验收项 |
|---|--------|
| BP-1 | `POST /api/v2/publish/batch`：接受 `{ "messages": [{ "channel", "payload", "idempotency_key"? }, ...] }` |
| BP-2 | 每批最多 100 条消息，超出返回 400 |
| BP-3 | 各消息独立处理：一条失败不影响其他 |
| BP-4 | 响应格式：`{ "ok": true, "results": [{ "index", "status", "seq_id"?, "timestamp"?, "error"? }, ...] }` |
| BP-5 | HTTP 状态码始终为 200（只要请求格式正确），由 `results[].status` 区分成功/失败 |
| BP-6 | 使用已有 auth middleware（API Key 认证） |

#### 7.3.3 模块：MQ 桥接设计文档

仅交付设计文档 `docs/mq-bridge-design.md`，实现在第5层。

| # | 验收项 |
|---|--------|
| MQ-1 | 覆盖 Kafka 消费者组和 RabbitMQ 直接消费两种模型 |
| MQ-2 | Topic/Queue → Channel 映射复用 Webhook 模板引擎 |
| MQ-3 | 定义死信队列/DLX 错误处理策略 |
| MQ-4 | 定义与 `Hub.Publish` 的集成方式和安全性考量 |
| MQ-5 | 草案配置结构（YAML） |

#### 7.3.4 数据模型

```sql
CREATE TABLE IF NOT EXISTS webhooks (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    url_token        TEXT NOT NULL UNIQUE,
    channel_template TEXT NOT NULL,
    secret           TEXT NOT NULL,
    key_id           TEXT NOT NULL REFERENCES api_keys(id),
    active           BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id              BIGSERIAL PRIMARY KEY,
    webhook_id      TEXT NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    status          TEXT NOT NULL,          -- 'success' | 'failed'
    response_code   INTEGER,
    error_message   TEXT,
    duration_ms     INTEGER NOT NULL,
    seq_id          BIGINT,
    channel         TEXT,
    idempotency_key TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

迁移版本：v4。

#### 7.3.5 技术决策

| 决策 | 选择 | 理由 |
|------|------|------|
| HMAC secret 存储方式 | 原始存储（非哈希） | HMAC 验签需要原始密钥；Webhook 是共享密钥认证，非口令认证 |
| 入站端点路径 | `/api/v2/webhooks/{url_token}` | url_token 是 64 字符 hex 随机字符串，防止枚举 |
| 模板引擎 | 点号路径 `{path.to.field}` | 简单够用，Webhook payload 通常扁平结构 |
| Batch publish 原子性 | 各自独立 | 频道间无事务依赖，单条失败不应该回滚已成功的 |
| MQ 桥接实现时机 | 第5层 | 依赖第4层集群模式完成后的 LISTEN/NOTIFY 机制 |

#### 7.3.6 新增错误码

| 代码 | 类别 | 描述 |
|------|------|------|
| 40005 | 请求 | Webhook 频道模板解析失败 |
| 40006 | 请求 | Batch publish 消息列表为空 |
| 40103 | 认证 | Webhook 签名无效 |
| 40402 | 资源 | Webhook 不存在 |
| 40902 | 冲突 | Webhook name 重复 |
|
