# Aether — 轻量级实时消息推送中间件

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://go.dev/)
[![CI](https://github.com/Errrori/Aether/actions/workflows/ci.yml/badge.svg)](https://github.com/Errrori/Aether/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**English:** Aether is a lightweight real-time message push middleware. **Publish via HTTP, subscribe via WebSocket, persist with PostgreSQL.** Strict pub-sub model — publishers and subscribers are fully decoupled, routed by channel name.

---

**中文：** Aether 是一个轻量级的实时消息推送中间件。发布者通过 HTTP API 将消息推送到指定频道，Aether 将每条消息实时推送给所有通过 WebSocket 订阅该频道的客户端。严格遵循发布-订阅模型，发布者和订阅者完全解耦，通过频道名称作为唯一路由键。

设计原则：
- **严格单向推送** — 普通客户端只能接收消息，发布权限仅限持有 API Key 的消息源
- **开闭原则** — 内核路由消息不解析业务内容，扩展新消息类型时内核无需修改
- **拼好码** — 优先复用成熟方案（`pgx`、`coder/websocket`、`golang-jwt`），不轻易自研或者实现已有优秀可用库的功能
- **做减法** — 不引入评论、点赞等社交功能，保持内核极简

## 项目结构

```
aether/
├── cmd/aether/        # 入口点
├── cmd/bench/         # 压测
├── internal/
│   ├── config/        # 配置解析
│   ├── store/         # PostgreSQL 存储引擎
│   ├── auth/          # API Key + JWT 验证
│   ├── hub/           # 频道注册、订阅管理、消息分发
│   ├── api/           # HTTP Handler
│   └── ws/            # WebSocket 连接管理
└── Dockerfile
```

## 性能基准

所有数字均来自仓库内可复现的基准，下面展示一次本机自跑测试，测试环境：AMD Ryzen 7 7840HS（16 逻辑核）、Windows、Go 1.25.0、PostgreSQL 16-alpine（Docker，本机同机）。

### Publish 进程内路径（DB 以 mock 隔离）
`BenchmarkPublish_NoSubscribers` 隔离出 hub 的发布代码路径，只包含：频道名校验 → 写存储 → 帧序列化 → 扇出。存储层用内存 mock 替换，因此**不含 SQL、TCP、磁盘开销，也不含真实订阅者分发，且不是端到端延迟**；其目的是在不受数据库（毫秒级）淹没的前提下，度量发布路径本身的常数开销。

```bash
go test -tags bench -run '^$' -bench '^BenchmarkPublish_NoSubscribers$' -benchmem -benchtime=1s -count=20 ./internal/hub/
```

20 次运行经 [benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) 汇总：**均值 1.054 µs ± 4%**（中位数约 1.05 µs，范围 0.82–1.15 µs，约 700 B/op、3 allocs/op）。

### 真实数据库

**Store 单次写入**（真实 PostgreSQL，无 mock）：
```bash
AETHER_TEST_DSN=postgres://aether:aether@localhost:5433/aether_test?sslmode=disable \
  go test -tags bench -run '^$' -bench '^BenchmarkWriteMessage$' -benchtime=2s ./internal/store/
```

实测约 **3.1 ms/op**。

**端到端压测**（HTTP 发布 → PostgreSQL 持久化 → Hub 分发 → WebSocket 接收）：

```bash
docker compose -f docker-compose.test.yaml up -d
go run ./cmd/aether -config config.bench.yaml
go run ./cmd/bench --server=http://localhost:8080 --api-key=<KEY> \
  --jwt-secret=<SECRET> --scenario=all --duration=10s --output=bench_e2e_results.json
```

结果保存在 [`bench_e2e_results.json`](bench_e2e_results.json)：

| 场景 | 发布 P99 | 端到端 P99 |
|------|---------|-----------|
| 1 频道 1 订阅者 | 8.49 ms | 8.43 ms |
| 1 频道 100 订阅者 | 9.33 ms | 9.91 ms |
| 100 频道各 1 订阅者 | 7.88 ms | 7.78 ms |
| 1 频道 1000 订阅者 | 16.00 ms | 18.67 ms |

其他结果：最大稳定连接 1206、内存 33.6 MB、API Key 缓存命中率 99.98%、历史回放（1000 条）约 10,850 msg/s。

## 快速开始

### 方式一：Docker Compose（推荐）

```bash
# 1. 克隆仓库
git clone https://github.com/Errrori/Aether.git
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


### 方式二：手动编译

```bash
git clone https://github.com/Errrori/Aether.git
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


## License

MIT © Aether Contributors
