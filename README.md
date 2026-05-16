# Aether — 轻量级实时消息推送中间件

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://go.dev/)
[![CI](https://github.com/aether-mq/aether/actions/workflows/test.yml/badge.svg)](https://github.com/aether-mq/aether/actions/workflows/test.yml)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

> English | [中文](#aether--轻量级实时消息推送中间件)

Aether is a lightweight real-time message push middleware. **Publish via HTTP, subscribe via WebSocket, persist with PostgreSQL.** Strict pub-sub model — publishers and subscribers are fully decoupled, routed by channel name.

---

**中文简介**

Aether 是一个轻量级的实时消息推送中间件。发布者通过 HTTP API 将消息推送到指定频道，Aether 将每条消息实时推送给所有通过 WebSocket 订阅该频道的客户端。严格遵循发布-订阅模型，发布者和订阅者完全解耦，通过频道名称作为唯一路由键。

设计原则：
- **严格单向推送** — 普通客户端只能接收消息，发布权限仅限持有 API Key 的消息源
- **开闭原则** — 内核路由消息不解析业务内容，扩展新消息类型时内核无需修改
- **拼好码** — 复用成熟方案（`pgx`、`coder/websocket`、`golang-jwt`），不自行实现密码学和网络协议
- **做减法** — 不引入评论、点赞等社交功能，保持内核极简

## 架构

```mermaid
flowchart LR
    P[Publisher<br/>HTTP POST] --> API[HTTP Handler]
    API --> Hub[Hub<br/>channel registry<br/>subscription map]
    Hub --> Store[(PostgreSQL<br/>messages + channels)]
    Hub --> WS[WebSocket Manager]
    WS <--> S[Subscriber<br/>WebSocket]
    API <--> Auth[Auth Module<br/>API Key + JWT]
    WS <--> Auth
    Config[Config Module<br/>YAML + env] --> API
    Config --> Hub
    Config --> WS
```

## 快速开始

### 方式一：Docker Compose（推荐）

```bash
# 1. 克隆仓库
git clone https://github.com/aether-mq/aether.git
cd aether

# 2. 编辑配置（修改 jwt_signing_key 和 api_keys）
cp config.example.yaml config.yaml
# 务必修改 auth.jwt_signing_key 和 auth.api_keys[0].key

# 3. 启动
docker compose up -d

# 4. 验证
curl http://localhost:8080/healthz
# → "ok"
```

> `config.example.yaml` 中 `AETHER_DATABASE_DSN` 已通过环境变量覆盖为 `postgres://aether:aether@postgres:5432/aether`，无需修改数据库连接。

> **Windows Git Bash 用户**：需在 `docker compose` 前加 `MSYS_NO_PATHCONV=1` 防止路径转换错误。

### 方式二：预编译二进制

前往 [GitHub Releases](https://github.com/aether-mq/aether/releases) 下载对应平台的二进制文件。

```bash
# Linux x86_64 示例
wget https://github.com/aether-mq/aether/releases/latest/download/aether-linux-amd64
chmod +x aether-linux-amd64
./aether-linux-amd64 -config config.yaml
```

### 方式三：手动编译

```bash
git clone https://github.com/aether-mq/aether.git
cd aether
go build -o aether ./cmd/aether
./aether -config config.yaml
```

## 使用示例

### 场景一：订单通知

发布者（后端服务）在订单状态变更时发布消息，订阅者（客服面板）实时收到通知。

**1. 生成订阅 Token**

使用 HMAC-SHA256 签发 JWT（签名密钥须与配置 `auth.jwt_signing_key` 一致）：

```bash
# 使用 jwt-cli 或在线工具生成，claims 示例：
# {
#   "sub": "support-agent-1",
#   "channels": ["order.*"],
#   "exp": 1893456000
# }
```

**2. 订阅者连接 WebSocket**

```bash
wscat -c "ws://localhost:8080/ws?token=<JWT_TOKEN>"
```

发送 subscribe 帧：

```json
{ "type": "subscribe", "channels": ["order.1234"] }
```

**3. 发布者推送消息**

```bash
curl -X POST http://localhost:8080/api/v1/publish \
  -H "Authorization: Bearer <API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{
    "channel": "order.1234",
    "payload": { "event": "status_changed", "status": "shipped" }
  }'
```

**4. 订阅者收到实时推送**

```json
{
  "type": "message",
  "channel": "order.1234",
  "seq_id": 1,
  "timestamp": "2026-05-16T12:00:00Z",
  "payload": { "event": "status_changed", "status": "shipped" }
}
```

### 场景二：IoT 数据推送

发布者（传感器网关）推送温度数据，订阅者（监控面板）实时展示。

```bash
# 发布温度数据
curl -X POST http://localhost:8080/api/v1/publish \
  -H "Authorization: Bearer <API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{"channel": "iot.temp.sensor01", "payload": 23.5}'
```

订阅者带 after_seq 重连时可追赶历史数据，超出留存窗口则收到 gap 帧：

```json
{
  "type": "gap",
  "channel": "iot.temp.sensor01",
  "available_from_seq": 100,
  "requested_from_seq": 42,
  "message": "history not available from seq 42; earliest available is seq 100"
}
```

## 配置

| 分类 | 关键字段 | 说明 |
|------|---------|------|
| `server` | `addr`, `tls_cert`, `tls_key`, `max_payload_size` | 监听地址、TLS、payload 上限 |
| `database` | `dsn`, `max_open_conns`, `max_idle_conns` | PostgreSQL 连接配置 |
| `auth` | `jwt_signing_key` **(必填)**, `api_keys` | HS256 密钥 ≥32 字节；API Key 列表 |
| `websocket` | `ping_interval`, `pong_timeout`, `allowed_origins` | 心跳参数、跨源控制 |
| `retention` | `default_ttl`, `default_max_count`, `rules` | 消息留存策略，按频道前缀匹配 |
| `shutdown` | `timeout` | 优雅关闭超时 |
| `log` | `level`, `format` | 结构化日志 |

环境变量覆盖：`AETHER_<SECTION>_<KEY>`（如 `AETHER_DATABASE_DSN`、`AETHER_LOG_LEVEL`）。

完整配置参考：[config.example.yaml](config.example.yaml) · [PRD §8.3](docs/prd.md)

## API

### HTTP 端点

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| `POST` | `/api/v1/publish` | API Key | 发布消息到频道 |
| `GET` | `/api/v1/history` | API Key | 检索频道历史消息 |
| `GET` | `/healthz` | 无 | 健康检查（存储可写 → 200） |
| `GET` | `/readyz` | 无 | 就绪检查（初始化完成 → 200） |
| `GET` | `/metricsz` | 无 | Prometheus 指标 |

### WebSocket

| 端点 | 认证 | 说明 |
|------|------|------|
| `/ws?token=<jwt>` | JWT（查询参数） | 实时消息推送连接 |

客户端帧：`subscribe`、`unsubscribe`  
服务端帧：`message`、`subscribed`、`unsubscribed`、`gap`、`error`  
心跳：WebSocket 标准 Ping/Pong（无应用层心跳）

完整 API 参考：[PRD §5](docs/prd.md)

## 项目对比

| 特性 | Aether | Centrifugo | RabbitMQ Streams |
|------|--------|------------|------------------|
| **定位** | 轻量推送中间件 | 全功能实时消息平台 | 消息队列 + 流 |
| **发布方式** | HTTP POST | HTTP / gRPC / 客户端 SDK | AMQP / Stream protocol |
| **订阅方式** | WebSocket | WebSocket / SSE / HTTP Stream | AMQP / Stream consumer |
| **存储** | PostgreSQL | PostgreSQL / Redis / Tarantool | 内置（Raft 日志） |
| **二进制大小** | ~15 MB（单文件） | ~30 MB | ~100 MB+ |
| **复杂度** | 低 | 中 | 高 |
| **适用场景** | 轻量实时推送、IoT、通知 | 聊天、协作、实时事件 | 高性能消息队列、事件溯源 |

## 开发

```bash
# 运行测试
go test ./...

# 集成测试（需要 Docker PostgreSQL）
docker compose -f docker-compose.test.yaml up -d
go test -tags=integration -count=1 ./internal/store/

# 代码检查
go vet ./...
```

### 项目结构

```
aether/
├── cmd/aether/        # 入口点
├── internal/
│   ├── config/        # 配置解析
│   ├── store/         # PostgreSQL 存储引擎
│   ├── auth/          # API Key + JWT 验证
│   ├── hub/           # 频道注册、订阅管理、消息分发
│   ├── api/           # HTTP Handler
│   ├── ws/            # WebSocket 连接管理
│   └── metrics/       # Prometheus 指标
├── docs/
│   ├── prd.md         # 产品需求文档
│   └── SPEC.md        # 实现规格书
├── Dockerfile
├── docker-compose.yml
└── config.example.yaml
```

## License

MIT © Aether Contributors
