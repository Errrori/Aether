# Aether —— 产品需求文档 (PRD)

## 1. 产品概述

Aether 是一个轻量级的实时消息推送中间件，用 Go 语言编写，依赖 PostgreSQL 作为持久化存储。消息发布者通过 HTTP API 将消息发布到指定频道；Aether 将每条消息实时推送给所有通过 WebSocket 订阅该频道的客户端。系统遵循严格的发布-订阅模型，发布者和订阅者完全解耦，通过频道名称作为唯一的路由键。

核心设计原则：

- **严格单向推送**：普通客户端只能接收消息，不能发送。发布权限仅限持有 API Key 的消息源。
- **开闭原则**：内核路由消息而不解析业务内容，扩展新消息类型时内核无需修改。
- **拼好码原则**：优先复用成熟方案，不自行实现密码学原语和网络协议。
- **做减法**：不引入评论、点赞等社交功能，保持内核极简。

## 2. 术语表

| 术语 | 定义 |
|---|---|
| **频道 (Channel)** | 命名的逻辑路由主题。消息发布到频道；订阅者接收该频道的消息。频道名称是分层字符串，如 `order.1234`、`system.alerts`。 |
| **消息 (Message)** | 从发布者发送到频道的单个原子有效载荷。包含服务器分配的序列 ID、时间戳、频道名称和不透明的 JSON payload（存储为 JSONB）。Aether 内核不解释 payload。 |
| **发布者 (Publisher)** | 持有有效 API Key 的外部系统，通过 HTTP 发布 API 将消息推送到频道。 |
| **订阅者 (Subscriber)** | 通过 WebSocket 连接、出示认证 Token 并接收实时消息的客户端。订阅者不能发布消息。 |
| **API Key** | 服务端密钥（v1 通过配置文件管理），授予发布者向频道发布消息的权限。 |
| **Token** | JWT (JSON Web Token)，颁发给订阅者，编码了身份和可订阅频道列表。WebSocket 连接时出示。 |
| **序列 ID (Seq ID)** | 每频道单调递增的 64 位整数，由服务器分配。用于历史回放和断线追赶。 |
| **游标 (Cursor)** | 订阅者已确认最新消息的 (频道, 序列 ID) 对。用于重连后请求追赶消息。 |
| **Hub** | 管理 WebSocket 连接、频道订阅和消息分发的核心 Go 运行时组件。 |

## 3. 功能需求

### 3.1 v1 — 最小可行产品 (MVP)

#### 3.1.1 消息发布 (HTTP API)

**FR-1.1** 暴露 HTTP POST 端点 `/api/v1/publish`，供持有有效 API Key 的发布者将消息推送到频道。

**FR-1.2** 发布请求体为 JSON，包含 `channel` (string, 必填) 和 `payload` (任意合法 JSON 值，必填。可以是对象、数组、字符串、数字、布尔值或 null)。可选字段 `idempotency_key` (string) 用于幂等性——相同 `(channel, idempotency_key)` 的重复发布返回首次的 `seq_id`，不重复写入或推送。去重为永久性（数据库唯一约束），不设时间窗口。

**FR-1.3** API Key 通过 `Authorization: Bearer <key>` 头传递。无效或缺失的 Key 返回 401。

**FR-1.4** 成功发布返回 200，包含服务器分配的 `seq_id` (int64) 和 `timestamp` (RFC 3339)。

**FR-1.5** 消息必须先持久化再分发给订阅者。持久化失败返回 503，且不推送任何消息。

**FR-1.6** 频道名称限制：1-128 字符，仅允许 `[a-zA-Z0-9_./-]`（不允许 `*`），不允许多个连续点，禁止以点开头或结尾。

#### 3.1.2 订阅 (WebSocket)

**FR-1.7** 在 `/ws` 接受 WebSocket 连接升级。

**FR-1.8** 客户端连接时通过查询参数 `?token=<jwt>` 传递 JWT Token。服务器验证 Token，无效则返回 HTTP 401。

**FR-1.9** 连接后，客户端发送 `subscribe` 帧加入频道。客户端可多次发送 subscribe 帧追加订阅频道（重复订阅同一频道忽略，不报错）。单次 subscribe 请求最多包含 100 个频道；单个连接累计最多订阅 1000 个频道，超出返回 `error` 帧。订阅必须在 Token 授权频道列表中验证。未授权频道请求返回 `error` 帧，代码 `40301`。

**FR-1.10** 消息发布到已订阅频道时，服务器推送 `message` 帧，包含 `channel`、`seq_id`、`timestamp` 和 `payload`。

**FR-1.11** 客户端可发送 `unsubscribe` 帧离开频道。

**FR-1.12** 心跳机制：仅使用 WebSocket 标准 ping/pong 帧，不设应用层心跳。服务器每 30 秒（可配置）发送 ping 帧，2 个 ping 周期内无 pong 响应则关闭连接。

**FR-1.13** 客户端可在 `subscribe` 帧中包含 `after_seq` 字段，请求从指定序列 ID 之后追赶消息。服务器从历史中回放所有 seq_id 严格大于请求值的消息。

**FR-1.14** 若历史不足以覆盖完整间隔（客户端离线时间超过历史保留窗口），服务器发送 `gap` 帧指示可用历史范围，然后继续实时推送。

**FR-1.15** WebSocket 出站缓冲区：每个连接的出站消息 channel 容量上限为可配置值（默认 256）。当缓冲区满时，服务器关闭该连接（发送 close 帧代码 1012 "service restart"，然后断开），防止假死客户端撑爆内存。

**FR-1.16** 历史回放与实时推送的顺序保证：订阅带有 `after_seq` 的频道时，服务器必须先完成全部历史消息回放，再将连接注册到频道的实时订阅者集合。这保证客户端不会先收到实时消息再收到旧历史消息。

#### 3.1.3 认证

**FR-1.17** API Key 在 YAML 配置文件中定义。每个 Key 条目包含 `key` (string) 和 `description` (string)。v1 所有 Key 授予对所有频道的发布权限。

**FR-1.18** 订阅者 JWT Token 使用 HMAC-SHA256 (HS256) 和服务器密钥签名。Token 包含标准声明 (`exp`, `iat`) 和自定义声明：`sub` (订阅者 ID)、`channels` (授权频道数组，支持通配符模式 `prefix.*`)。

**FR-1.19** 服务器拒绝过期 JWT Token，返回 401。WebSocket 连接在 Token 生命周期内保持有效；Token 过期不断开已有连接。

#### 3.1.4 持久化

**FR-1.20** 所有发布的消息必须持久化到磁盘。消息不能仅存在于内存。

**FR-1.21** 消息按频道存储，按序列 ID 排序，支持有序历史回放。

**FR-1.22** 历史保留策略：按频道前缀模式配置 TTL 和最大计数。配置中定义 `rules` 列表，每条规则包含 `pattern`（频道前缀通配符，如 `alerts.*`）、`ttl` 和 `max_count`。频道匹配第一个命中的规则；未匹配任何规则的频道使用 `default_ttl`（默认 30 天）和 `default_max_count`（默认 10,000 条）。超出限制的消息由后台清理任务（每 5 分钟运行一次）驱逐。

**FR-1.23** 历史检索 API：`GET /api/v1/history?channel=<name>&after_seq=<n>&limit=<m>` 返回指定频道中 seq_id 严格大于 `after_seq` 的消息，最多 `limit` 条（默认 100，最大 1000）。需 API Key 认证。频道不存在时返回空列表（非 404），与隐式频道模型一致。

#### 3.1.5 频道管理

**FR-1.24** 频道隐式创建：第一条消息发布到某频道名称时即存在。无显式创建端点。

**FR-1.25** 频道生命周期：当所有保留消息因 TTL/最大计数驱逐而过期且无活跃订阅者时，频道不再存在。服务器可内部清理空频道。

#### 3.1.6 运维与监控

**FR-1.26** 健康检查：`GET /healthz` — 存储可写时返回 200。

**FR-1.27** 就绪检查：`GET /readyz` — 服务器完全初始化后返回 200。

**FR-1.28** 指标：`GET /metricsz` 以 Prometheus 格式暴露：

- `aether_connections_active` (gauge)
- `aether_channels_active` (gauge)
- `aether_messages_published_total` (counter)
- `aether_messages_pushed_total` (counter)
- `aether_publish_duration_seconds` (histogram)
- `aether_storage_write_duration_seconds` (histogram)

**FR-1.29** 优雅关闭：收到 SIGTERM 后，停止接受新连接，排空现有 WebSocket 连接（推送缓冲消息，发送 `close` 帧代码 1001），刷新存储写入，然后退出。超时可配置（默认 10 秒），超时后未完成 close 握手的连接强制断开。

#### 3.1.7 日志

**FR-1.30** 使用 Go 标准库 `log/slog` 输出结构化日志。日志级别和格式通过配置文件控制。

关键日志点：

| 事件 | 级别 | 字段 |
|---|---|---|
| 服务启动/关闭 | INFO | addr, version |
| WebSocket 连接建立 | INFO | subscriber_id, remote_addr |
| WebSocket 连接断开 | INFO | subscriber_id, duration, reason |
| 频道订阅/取消 | DEBUG | subscriber_id, channels |
| 消息发布 | DEBUG | channel, seq_id |
| 认证失败 | WARN | remote_addr, reason |
| 存储错误 | ERROR | channel, operation, err |
| TTL 驱逐统计 | INFO | channels_cleaned, messages_evicted |

### 3.2 v2 — 未来范围

| ID | 功能 | 描述 |
|---|---|---|
| FR-2.1 | 动态 API Key CRUD | 创建、列出、轮换和撤销 Key 的 HTTP API，支持范围权限 |
| FR-2.2 | 集群模式 | 多节点部署，共享存储后端进行跨节点消息扇出 |
| FR-2.3 | 频道 Presence | 跟踪并暴露每频道在线订阅者 |
| FR-2.4 | 批量发布 | 单个 HTTP 请求发布到多频道 |
| FR-2.5 | Webhook 发布 | 入站 Webhook 触发发布 |
| FR-2.6 | 消息确认 | 订阅者确认接收，服务器跟踪每订阅者游标 |
| FR-2.7 | 管理面板 | 监控连接、频道和消息流的 Web UI |
| FR-2.8 | 速率限制 | 每发布者、每频道的速率限制 |
| FR-2.9 | SSE 传输 | Server-Sent Events 作为 WebSocket 替代 |

## 4. 非功能需求

### 4.1 性能

**NFR-1** 单节点至少 10,000 并发 WebSocket 连接。

**NFR-2** HTTP 发布到 WebSocket 投递的端到端延迟 P99 < 50ms（不含客户端网络传输）。

**NFR-3** 发布吞吐量：小于 4KB 的消息，跨所有频道至少 5,000 msg/s。

**NFR-4** 每条消息的内存开销 < 1KB（不含 payload 本身）。

**NFR-5** 历史回放：单频道最多 1,000 条消息的查询 P99 < 100ms。

### 4.2 可靠性

**NFR-6** 发布 API 返回 200 后消息必须已持久化到 PostgreSQL。推送为异步操作，200 不保证消息已送达订阅者。进程崩溃不得导致已持久化消息丢失。

**NFR-7** 依赖 PostgreSQL 的 ACID 事务保证持久化可靠性。写操作在事务提交后即视为持久化。

**NFR-8** 服务器启动时间（从二进制到接受连接）< 2 秒（PG 连接就绪后）。

### 4.3 安全

**NFR-9** API Key 必须是加密随机、至少 32 字节、Base64url 编码（43 字符）。

**NFR-10** JWT 签名密钥必须是加密随机、至少 256 位（32 字节）。

**NFR-11** 所有 HTTP API 仅通过 HTTPS 可用（反向代理或 Go TLS 服务器）。

**NFR-12** WebSocket 升级必须验证 Origin 头，防止跨站 WebSocket 劫持。可配置允许源列表，**默认拒绝所有跨源连接**（空列表）。配置示例中须明确标注 `["*"]` 仅用于开发环境。

**NFR-13** 消息 payload 大小限制为可配置最大值（默认 64KB），超出返回 413。

**NFR-14** 拒绝不安全 JWT 算法（none、RS256/HS256 混淆等），仅接受 HS256。

### 4.4 可部署性

**NFR-15** 作为单个 Go 二进制发布，外部运行时依赖仅为 PostgreSQL。PG 连接串通过配置文件或环境变量指定。

**NFR-16** 配置通过单个 YAML 文件和可选环境变量覆盖（`AETHER_<SECTION>_<KEY>`）提供。

**NFR-17** 数据库 Schema 通过内建迁移自动管理——服务启动时检测并执行未应用的迁移，无需手动建表。

## 5. API 设计

### 5.1 HTTP 端点

#### 发布消息

```
POST /api/v1/publish
Authorization: Bearer <api_key>
Content-Type: application/json

Request:
{
  "channel": "order.1234",
  "payload": { "event": "created", "order_id": 1234 },
  "idempotency_key": "optional-dedup-key"
}

// payload 可以是任意 JSON 值：对象、数组、字符串、数字、布尔值、null
// 例：{ "channel": "iot.temp", "payload": 23.5 }

Success 200:
{
  "ok": true,
  "seq_id": 42,
  "timestamp": "2026-05-09T12:00:00Z"
}

Error 400:
{ "ok": false, "error": { "code": 40001, "message": "invalid channel name" } }

Error 401:
{ "ok": false, "error": { "code": 40101, "message": "invalid or missing API key" } }

Error 413:
{ "ok": false, "error": { "code": 41301, "message": "payload exceeds maximum size of 65536 bytes" } }

Error 503:
{ "ok": false, "error": { "code": 50301, "message": "failed to persist message" } }
```

#### 检索历史

```
GET /api/v1/history?channel=<name>&after_seq=<n>&limit=<m>
Authorization: Bearer <api_key>

Success 200:
{
  "ok": true,
  "channel": "order.1234",
  "messages": [
    { "seq_id": 43, "timestamp": "...", "payload": { ... } }
  ],
  "has_more": false
}
```

#### 健康检查 / 就绪 / 指标

```
GET /healthz    -> 200 "ok"
GET /readyz     -> 200 "ok" or 503 "not ready"
GET /metricsz   -> Prometheus text format
```

### 5.2 WebSocket 协议

所有帧为 JSON 文本帧 (opcode 0x1)。

#### 客户端 → 服务器

**订阅**

```json
{
  "type": "subscribe",
  "channels": ["order.1234", "system.alerts"],
  "after_seq": { "order.1234": 40, "system.alerts": 0 }
}
```

`after_seq` 可选。值为 0 或省略表示不追赶。

**取消订阅**

```json
{ "type": "unsubscribe", "channels": ["order.1234"] }
```

> 心跳使用 WebSocket 标准 ping/pong 帧，不设应用层心跳帧。

#### 服务器 → 客户端

**消息推送**

```json
{
  "type": "message",
  "channel": "order.1234",
  "seq_id": 42,
  "timestamp": "2026-05-09T12:00:00Z",
  "payload": { "event": "created", "order_id": 1234 }
}
```

**订阅确认**

```json
{ "type": "subscribed", "channels": ["order.1234", "system.alerts"] }
```

**取消订阅确认**

```json
{ "type": "unsubscribed", "channels": ["order.1234"] }
```

**历史缺口**

```json
{
  "type": "gap",
  "channel": "order.1234",
  "available_from_seq": 38,
  "requested_from_seq": 30,
  "message": "history not available from seq 30; earliest available is seq 38"
}
```

**错误**

```json
{ "type": "error", "code": 40301, "message": "not authorized to subscribe to channel: private.admin" }
```

### 5.3 错误代码

| 代码 | 类别 | 描述 |
|---|---|---|
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

## 6. 数据模型

### 6.1 核心实体

**频道**
| 字段 | 类型 | 描述 |
|---|---|---|
| name | string (PK) | 频道名称 |
| current_seq | int64 | 最新消息的序列 ID |
| created_at | timestamp | 第一条消息发布时 |
| updated_at | timestamp | 最后一条消息发布时 |

**消息**
| 字段 | 类型 | 描述 |
|---|---|---|
| id | int64 (auto PK) | 内部行 ID |
| channel | string (FK) | 频道名称 |
| seq_id | int64 | 每频道单调序列号 |
| payload | blob | 原始 JSON 字节（PG 中为 JSONB） |
| idempotency_key | string (nullable) | 幂等去重键 |
| created_at | timestamp | 服务器分配的时间戳 |

**订阅**（仅运行时，不持久化）
| 字段 | 类型 | 描述 |
|---|---|---|
| conn_id | string | WebSocket 连接标识符 |
| subscriber_id | string | 来自 JWT `sub` 声明 |
| channels | set\<string\> | 当前订阅的频道 |
| cursors | map\<string, int64\> | 每频道最后投递的序列 ID |

### 6.2 实体关系

```
Channel 1---* Message      (一个频道有多条消息)
Channel 1---* Subscription (一个频道有多个订阅者，仅运行时)
```

订阅是短暂的运行时状态，保存在 Hub 的内存映射中，不持久化。连接断开后订阅自动清除，服务端从频道的订阅者集合中移除该连接，内存立即释放。客户端重连时需要重新发送 subscribe 帧建立订阅，并通过 `after_seq` 补追离线期间的消息。

**设计决策**：Aether 是推送中间件而非消息队列，订阅表示"实时关注"而非"持久消费"。客户端本就知道自己需要哪些频道（由业务逻辑决定），重连时自行重新订阅即可。若需要"离线期间消息排队等待消费"的能力，应在上游使用消息队列（Kafka/RabbitMQ）而非让 Aether 承担此职责。

## 7. 架构概述

### 7.1 技术栈

| 组件 | 选型 | 理由 |
|---|---|---|
| Go 版本 | 1.22+ | 标准库增强路由 + `log/slog` |
| HTTP 路由 | `net/http` 标准库 | Go 1.22+ 支持方法路由，5 个端点无需第三方库 |
| WebSocket | `github.com/coder/websocket` | 纯 Go，API 现代，`ctx.Context` 驱动，维护活跃 |
| 数据库驱动 | `github.com/jackc/pgx/v5` | 纯 Go，性能优于 `lib/pq` |
| 日志 | `log/slog` | Go 1.21+ 内建结构化日志，零依赖 |
| 指标 | `prometheus/client_golang` | Prometheus 标准客户端 |
| JWT | `github.com/golang-jwt/jwt/v5` | 社区标准 JWT 库 |
| 配置解析 | `gopkg.in/yaml.v3` | 标准 YAML 解析 |

### 7.2 组件图

```
                      +-----------------------+
                      |   HTTP API Handler    |
                      |   (net/http 1.22+)    |
                      +-----------+-----------+
                                  |
                          publish request
                                  |
                                  v
+-------------+         +-----------------------+         +-------------+
|   Publisher | ------> |      Hub (core)       | ------> |   Storage   |
|   (HTTP)    |         |                       |         |   Engine    |
+-------------+         |  - channel registry   |         | (PostgreSQL)|
                        |  - subscription map   |         |   (pgx/v5)  |
+-------------+         |  - message dispatcher |         | - messages  |
|  Subscriber | <------ |                       | <------ | - channels  |
|  (WebSocket)|         +-----------+-----------+         +-------------+
| (coder/ws)  |                     |
+-------------+         +-------------+-------------+
                        |                           |
                +-------+-------+           +-------+-------+
                | Auth Module   |           | Config Module |
                | - API key val |           | - YAML loader |
                | - JWT verify  |           | - env overlay |
                +---------------+           +---------------+
```

### 7.3 数据流：发布与推送

1. 发布者发送 POST `/api/v1/publish`，带 API Key 和消息体。
2. HTTP Handler 通过 Auth 模块验证 API Key。
3. HTTP Handler 验证频道名称和 payload 大小。
4. HTTP Handler 调用 `Hub.Publish(channel, payload, idempotencyKey)`。
5. Hub 委托 Storage Engine：`Store.WriteMessage(channel, payload)`。
6. Storage Engine 在 PostgreSQL 事务中写入消息并递增频道 `current_seq`，返回 `(seqID, timestamp)`。
7. Hub 查找频道订阅者映射，为每个已订阅 WebSocket 连接封装 `message` 帧放入连接出站 channel。
8. 每个连接的写 goroutine 从出站 channel 读取并写入 WebSocket。
9. HTTP Handler 返回 200 响应，含 `seq_id` 和 `timestamp`。

关键：步骤 5-6 先于步骤 7，确保持久化在推送前成功。步骤 9 可能在所有 WebSocket 写入完成前返回——推送是异步的。

### 7.4 数据流：订阅与追赶

1. 客户端打开 WebSocket 连接到 `/ws?token=<jwt>`。
2. WebSocket Handler 通过 Auth 模块验证 JWT。
3. Auth 提取 `sub`（订阅者 ID）和 `channels`（授权频道列表）。
4. 创建连接对象，启动读写 goroutine。
5. 客户端发送 `subscribe` 帧，含频道列表和可选 `after_seq` 映射。
6. Hub 验证每个请求频道是否在 JWT 授权列表中。未授权返回 `error` 帧。
7. 对已授权频道，如有 `after_seq`：Hub 调用 `Store.ReadHistory(channel, afterSeq, limit)`。
8. Storage 返回可用消息，Hub 逐条推送 `message` 帧。如历史不足以覆盖完整间隔，发送 `gap` 帧。
9. **历史回放完成后**，Hub 才将连接注册到频道订阅者集合。这保证客户端不会先收到实时消息再收到旧历史消息。

### 7.5 Go 包结构

| 包 | 职责 |
|---|---|
| `cmd/aether` | 入口点，配置加载，服务器引导 |
| `internal/config` | YAML 配置解析，环境变量覆盖，默认值 |
| `internal/auth` | API Key 验证，JWT 验证，频道授权检查 |
| `internal/hub` | 核心 Hub：频道注册表，订阅管理，消息分发 |
| `internal/ws` | WebSocket 升级，连接生命周期，帧读写，心跳（基于 `coder/websocket`） |
| `internal/api` | HTTP Handler：发布，历史，健康检查，就绪检查，指标（基于 `net/http`） |
| `internal/store` | 存储引擎接口和 PostgreSQL 实现（基于 `pgx/v5`） |
| `internal/metrics` | Prometheus 指标注册和收集器 |

## 8. 存储设计

### 8.1 引擎选择：PostgreSQL

使用 `github.com/jackc/pgx/v5` 作为 Go 驱动（纯 Go 实现，性能优于 `lib/pq`），配合 `database/sql` 接口。

选择理由：

- **原生并发写入**：MVCC 多版本并发控制，天然支持多写者。Aether 的发布吞吐不受单写者瓶颈限制。
- **JSONB 类型**：消息 payload 存储为 JSONB，保留结构化信息，未来可按 payload 字段索引/查询，且存储和解析性能优于纯 TEXT/BLOB。
- **LISTEN/NOTIFY**：PG 内置发布-订阅机制。v2 集群模式下，多个 Aether 节点可通过 LISTEN/NOTIFY 实现跨节点消息扇出，无需引入 Redis 等中间件。
- **集群路径直接打通**：多节点共享同一 PG 实例即可，v2 无需替换存储层。这比 SQLite → PG 的迁移成本更低。
- **生产级可靠性**：流复制、PITR（时间点恢复）、成熟的备份恢复方案。多数生产环境已有 PG 实例，不增加额外运维负担。
- **Schema 迁移**：PG 的 DDL 事务支持完整，迁移可原子回滚。

直接使用 `database/sql` + 手写 SQL 而非 ORM：Aether 的存储操作范围窄且性能敏感，手写 SQL 更简单、高效、透明。

### 8.2 数据库 Schema

```sql
-- Schema migrations 表（内建迁移管理）
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 频道表
CREATE TABLE IF NOT EXISTS channels (
    name        TEXT PRIMARY KEY,
    current_seq BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 消息表
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

-- 主查询路径：按频道和序列 ID 范围读取
CREATE INDEX IF NOT EXISTS idx_messages_channel_seq
    ON messages (channel, seq_id);

-- TTL 驱逐任务使用
CREATE INDEX IF NOT EXISTS idx_messages_created_at
    ON messages (created_at);
```

### 8.3 完整配置文件

```yaml
# config.yaml — Aether 配置文件

server:
  addr: ":8080"                 # 监听地址
  tls_cert: ""                  # TLS 证书路径（空则不启用）
  tls_key: ""                   # TLS 密钥路径

database:
  dsn: "postgres://aether:password@localhost:5432/aether?sslmode=disable"
  max_open_conns: 25
  max_idle_conns: 10
  conn_max_idle_time: 5m
  conn_max_lifetime: 30m

auth:
  jwt_signing_key: ""           # HS256 签名密钥（必填，至少 32 字节）
  jwt_clock_skew: 30s           # 时钟偏移容忍
  api_keys:                     # v1 静态配置
    - key: "xxxxx"
      description: "admin key"

websocket:
  ping_interval: 30s            # ping 发送间隔
  pong_timeout: 60s             # 2 个 ping 周期无 pong 则断开
  outbound_buffer: 256          # 出站缓冲区上限
  max_message_size: 65536       # 入站帧最大字节
  allowed_origins: []           # 默认空=拒绝跨源

retention:
  default_ttl: 720h             # 30 天兜底
  default_max_count: 10000      # 每频道默认最大消息数
  eviction_interval: 5m         # 清理任务间隔
  rules:                        # 按频道前缀匹配，第一个命中的生效
    - pattern: "alerts.*"
      ttl: 24h
      max_count: 5000
    - pattern: "orders.*"
      ttl: 2160h                # 90 天
      max_count: 50000
    - pattern: "iot.*"
      ttl: 168h                 # 7 天
      max_count: 100000

shutdown:
  timeout: 10s                  # 优雅关闭超时

log:
  level: "info"                 # debug / info / warn / error
  format: "json"                # json / text
```

环境变量覆盖：`AETHER_<SECTION>_<KEY>`（如 `AETHER_DATABASE_DSN`、`AETHER_AUTH_JWT_SIGNING_KEY`、`AETHER_LOG_LEVEL`）。

### 8.4 关键存储操作

**WriteMessage**（单事务，使用 `SELECT ... FOR UPDATE` 行锁保证并发安全）：

```sql
-- Step 1: 确保频道存在
INSERT INTO channels (name) VALUES ($1)
ON CONFLICT (name) DO NOTHING;

-- Step 2: 锁定频道行并读取当前序列号
SELECT current_seq FROM channels WHERE name = $1 FOR UPDATE;

-- Step 3: 插入消息（seq_id = current_seq + 1）
INSERT INTO messages (channel, seq_id, payload, idempotency_key)
VALUES ($1, $2, $3, $4)
ON CONFLICT (channel, idempotency_key) DO NOTHING
RETURNING seq_id, created_at;

-- Step 4: 推进序列号
UPDATE channels SET current_seq = current_seq + 1, updated_at = now()
WHERE name = $1;
```

Go 实现逻辑：
1. 开启事务
2. 执行 Step 1（确保频道存在）
3. 执行 Step 2（`FOR UPDATE` 锁定该频道行，防止并发写入获得相同 seq_id）
4. 计算 `newSeq = current_seq + 1`
5. 执行 Step 3，传入 `newSeq` 和 `idempotency_key`
   - 若 `RETURNING` 返回空行（idempotency_key 冲突），查询现有消息返回，**跳过 Step 4**（不递增 seq）
6. 执行 Step 4（递增序列号）
7. 提交事务

若因 `idempotency_key` 冲突导致 `ins_msg` 为空，查询现有消息：

```sql
SELECT seq_id, created_at FROM messages
WHERE channel = $1 AND idempotency_key = $2;
```

**ReadHistory**：

```sql
SELECT seq_id, payload, created_at FROM messages
WHERE channel = $1 AND seq_id > $2
ORDER BY seq_id ASC LIMIT $3;
```

**EvictExpiredMessages**（后台任务，每 5 分钟，按频道逐个清理）：

Go 实现逻辑：
1. 查询所有频道：`SELECT name, current_seq FROM channels`
2. 对每个频道，根据频道名匹配 retention.rules 获取该频道的 TTL 和 max_count（未匹配则用默认值）
3. 按 TTL 驱逐：`DELETE FROM messages WHERE channel = $1 AND created_at < now() - $2::interval`
4. 按最大计数驱逐：`DELETE FROM messages WHERE channel = $1 AND seq_id <= (current_seq - max_count)`
5. 清理空频道：`DELETE FROM channels WHERE NOT EXISTS (SELECT 1 FROM messages WHERE messages.channel = channels.name)`

优化：可记录每个频道的上次清理时间，仅在频道有新消息写入后才重新检查，减少全量扫描开销。

### 8.5 Schema 迁移

Aether 内建轻量迁移管理器，不依赖第三方迁移工具。启动时自动执行：

```go
type Migration struct {
    Version int
    Up      string  // SQL to apply
}

var migrations = []Migration{
    {Version: 1, Up: "CREATE TABLE channels (...)"},
    {Version: 2, Up: "CREATE TABLE messages (...)"},
    // future migrations append here
}
```

启动流程：

1. 读取 `schema_migrations` 表获取当前版本
2. 按版本号顺序执行未应用的迁移（每条在独立事务中）
3. 写入 `schema_migrations` 记录已应用版本

## 9. 安全模型

### 9.1 API Key 验证（发布者）

1. 提取 `Authorization: Bearer <key>` 头
2. 在配置的 Key 集合中查找
3. 使用 `crypto/subtle.ConstantTimeCompare` 固定时间比较，防计时攻击
4. v1：所有 Key 授予全部频道发布权限

### 9.2 JWT Token 验证（订阅者）

1. 连接时解析 JWT，使用配置的签名密钥和明确算法列表 `[HS256]`
2. 验证 `exp` 声明（允许 30 秒时钟偏移）
3. 提取 `sub`（订阅者 ID）和 `channels`（授权频道列表）
4. `channels` 支持精确匹配 (`order.1234`) 和前缀通配符 (`order.*` 匹配所有以 `order.` 开头的频道，即递归匹配所有子频道，不限于单层)。`["*"]` 授权所有频道。

JWT 声明结构：

```json
{
  "sub": "user-abc123",
  "channels": ["order.*", "system.alerts"],
  "iat": 1746796800,
  "exp": 1746883200
}
```

### 9.3 频道授权

- subscribe 帧中每个请求频道与 Token 的 `channels` 声明匹配
- 精确匹配优先，然后检查前缀通配符：`prefix.*` 匹配所有以 `prefix.` 开头的频道名（递归，不限层级）
- 不支持其他通配符模式（如 `**`、`?`），保持简单
- 拒绝的频道返回 `error` 帧，代码 `40301`

### 9.4 Origin 验证

WebSocket 升级验证 HTTP Origin 头：

- `["*"]`：接受任何源（仅开发用，配置中须标注警告）
- `["https://app.example.com"]`：仅接受此源
- 空列表（默认）：拒绝所有跨源连接

### 9.5 传输安全

- Aether 可直接启用 TLS（配置证书/密钥路径）或运行于反向代理之后
- v1 建议反向代理方案，保持二进制简单

## 10. 分阶段路线图

### v1：最小可行产品（目标 4-6 周）

| 周 | 里程碑 | 交付物 |
|---|---|---|
| 1 | 项目脚手架 + 存储 | Go 模块初始化，配置解析，PostgreSQL 存储引擎（含内建 schema 迁移），WriteMessage/ReadHistory/Evict |
| 2 | HTTP 发布 API + 认证 | 发布端点，API Key 验证，JWT 验证模块，频道授权逻辑 |
| 3 | Hub + WebSocket | Hub 核心（频道注册表），WebSocket 升级，连接生命周期，帧协议 |
| 4 | 历史回放 + 追赶 | 历史端点，subscribe 追赶，gap 帧，幂等去重 |
| 5 | 运维 + 指标 | 健康检查/就绪检查，Prometheus 指标，优雅关闭，后台驱逐任务 |
| 6 | 测试 + 加固 | 集成测试，负载测试，README，部署指南，示例配置 |

**v1 边界**：

- 单节点、单进程、PostgreSQL 存储
- API Key 通过配置文件管理（无 CRUD API）
- 仅 WebSocket 传输（无 SSE）
- JWT Token 由外部系统颁发（Aether 不颁发 Token）
- 无 Presence、无消息确认、无速率限制

### v2：可扩展性增强（v1 后 3-6 个月）

| 阶段 | 功能 | 描述 |
|---|---|---|
| 2a | 动态 API Key | CRUD 端点，范围权限 |
| 2b | 集群模式 | 多节点共享 PG，利用 LISTEN/NOTIFY 实现跨节点扇出，节点发现 |
| 2c | SSE 传输 | Server-Sent Events 端点 |
| 2d | 高级功能 | Presence，消息确认，速率限制，批量发布 |
| 2e | 管理面板 | Web UI 监控和 Key 管理 |

## 11. 验证计划

### 功能验证

- 发布-订阅端到端：发布者 POST 消息 → 订阅者 WebSocket 收到消息
- 历史回放：订阅者带 after_seq 重连 → 收到补推消息
- Gap 通知：客户端离线超过 TTL → 收到 gap 帧后继续实时推送
- 认证：无效 API Key 返回 401；未授权频道订阅返回 40301
- 幂等：相同 idempotency_key 重复发布 → 返回相同 seq_id，不重复推送
- 优雅关闭：SIGTERM → 现有连接收到 close 帧 → 存储刷新 → 进程退出

### 性能验证

- 并发连接测试：10,000 WebSocket 连接下稳定运行
- 延迟测试：P99 端到端延迟 < 50ms
- 吞吐量测试：5,000 msg/s 下无消息丢失

### 可靠性验证

- 崩溃恢复：发布 200 后 kill 进程 → 重启后消息仍可从历史 API 读取（PG ACID 保证）
- PG 连接断开恢复：模拟网络闪断 → 连接池自动重连 → 服务恢复无消息丢失
