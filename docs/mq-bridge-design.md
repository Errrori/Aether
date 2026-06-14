# MQ 桥接设计文档

> v2 第2层交付物。本文档为 Kafka 和 RabbitMQ 消息队列接入 Aether 的设计方案，实现在第5层。

## 1. 概述

### 1.1 背景

Aether 目前仅通过 HTTP API 接受消息发布（POST `/api/v1/publish`、Webhook）。许多生产环境中，系统间通信以 Kafka 或 RabbitMQ 作为骨干。MQ 桥接器作为长期运行的消费者，从 MQ 消费消息并调用 `Hub.Publish` 注入 Aether 频道。

### 1.2 目标

- 支持 Kafka 消费者组模式和 RabbitMQ 直接消费模式
- 消息 value 为 JSON 格式，透传为 Aether payload
- Topic/Queue → Channel 的映射复用 Webhook 模板引擎
- 提供死信队列机制处理投递失败的消息

## 2. 架构

```
+----------------+     +------------------+     +-----------+
| Kafka Cluster  |---->| MQ Bridge        |---->| Hub.Publish|
| RabbitMQ       |     | (goroutine)      |     +-----------+
+----------------+     +------------------+
```

桥接器作为独立 goroutine 运行，在 Aether 进程内启动。每类 MQ 源一个 goroutine 组，共享同一个 `Hub.Publish` 接口。

启动生命周期：
1. Aether 主进程完成 Hub、Store 初始化
2. 若配置启用了 MQ 桥接，启动对应消费者 goroutine
3. 优雅关闭：收到 SIGTERM 后停止接受新消息，提交当前偏移/ack，等待发布中的消息完成

## 3. Kafka 桥接

### 3.1 消费者模型

使用消费者组（Consumer Group）模式，支持：
- 多节点部署时，同一 group_id 的消费者自动分区分配
- 手动提交偏移（commit after successful `Hub.Publish`）
- At-least-once 语义：先发布成功再提交偏移

### 3.2 配置草案

```yaml
mq_bridge:
  kafka:
    enabled: false
    brokers: ["localhost:9092"]
    group_id: "aether-bridge"
    session_timeout: 10s
    max_poll_records: 500
    topics:
      - topic: "orders.*"
        channel_template: "{topic}"
        dead_letter_topic: "aether-dlq"
      - topic: "system.events"
        channel_template: "system.events"
```

### 3.3 Topic → Channel 映射

- 若 `channel_template` 为字面字符串（无 `{...}` 占位符），直接作为频道名
- 若含占位符，从 Kafka 消息中提取值拼接：
  - `{topic}` — Kafka topic 名
  - `{key}` — Kafka message key（字符串）
  - `{partition}` — 分区号
  - `{offset}` — 偏移量
  - `{value.field.path}` — 消息 value JSON 中的字段

示例：
- Topic `orders.created`，template `{topic}` → channel `orders.created`
- Template `order.{value.order_id}` → 从消息 JSON 中提取 `order_id`

### 3.4 消息格式

Kafka 消息 value 预期为合法 JSON，直接作为 Aether payload。消息 key 可选，用于分区路由。

不添加 Aether 自定义 envelope——消息透传。

### 3.5 错误处理

```
消息消费 → Hub.Publish 成功 → 提交偏移
           Hub.Publish 失败 → 重试 (指数退避，最多 3 次)
           重试全部失败 → 发送到 DLQ topic → 提交偏移（不阻塞消费）
```

DLQ 消息格式（JSON）：
```json
{
  "original_topic": "orders.created",
  "original_offset": 12345,
  "error": "storage unavailable",
  "timestamp": "2026-06-14T12:00:00Z",
  "payload": { ... }
}
```

## 4. RabbitMQ 桥接

### 4.1 消费者模型

使用 AMQP `basic.consume` 模式：
- 手动 ack（ack after successful `Hub.Publish`）
- Prefetch count 可配置，控制并发度
- At-least-once 语义

### 4.2 配置草案

```yaml
mq_bridge:
  rabbitmq:
    enabled: false
    url: "amqp://guest:guest@localhost:5672/"
    prefetch: 100
    queues:
      - queue: "aether.inbound"
        channel_template: "inbound.{routing_key}"
        dead_letter_exchange: "aether-dlx"
        dead_letter_routing_key: "dead"
```

### 4.3 Queue → Channel 映射

与 Kafka 一致，模板变量：
- `{queue}` — 队列名
- `{routing_key}` — 路由键
- `{exchange}` — 交换机名
- `{value.field.path}` — 消息 body JSON 中的字段

### 4.4 错误处理

```
消息消费 → Hub.Publish 成功 → ack (multiple=false)
           Hub.Publish 失败 → nack (requeue=false) → 进入 DLX
```

RabbitMQ 利用 Dead Letter Exchange (DLX) 原生机制，无需桥接器自行实现。

## 5. 与 Hub.Publish 集成

桥接器直接调用 `hub.Publish(ctx, channel, payload, nil)`：

- 不携带 `idempotency_key`：MQ 本身有重试机制，桥接器不重复幂等（第5层可讨论是否需要）
- `ctx` 传递消费者 goroutine context，支持取消
- 发布成功视为消息已持久化（满足 NFR-6）

**安全性**：桥接器绕过 HTTP API 层，无认证检查。这是因为：
1. 桥接器是 Aether 进程的可信内部组件
2. 消息源的可信性在 MQ 连接层面保证（SASL/SSL/TLS）
3. 未来如果需要按来源追溯，可在桥接器配置中关联 API Key 身份

## 6. 健康监控

### 6.1 指标

| 指标 | 类型 | 描述 |
|------|------|------|
| `aether_mq_messages_consumed_total` | Counter | 消费的消息总数 |
| `aether_mq_messages_published_total` | Counter | 成功发布的消息数 |
| `aether_mq_messages_failed_total` | Counter | 发布失败的消息数 |
| `aether_mq_consumer_lag` | Gauge | Kafka consumer lag |
| `aether_mq_connection_status` | Gauge | 1=已连接, 0=已断开 |

### 6.2 健康检查

当桥接器消费者断开连接时，通过现有的 `/healthz` 端点反映（可选：仅在严格模式下报告不健康）。

## 7. 实现计划（第5层）

1. **配置** — `internal/config/config.go` 新建 `MQBridgeConfig` 节
2. **MQ 桥接包** — `internal/mqbridge/`
   - `bridge.go` — Bridge 接口定义
   - `kafka.go` — Kafka 消费者实现
   - `rabbitmq.go` — RabbitMQ 消费者实现
   - `template.go` — 复用 Webhook 模板引擎（提取到 `internal/webhook/template.go` 为公共函数）
3. **装配** — `main.go` 检查配置，启动桥接 goroutine
4. **优雅关闭** — bridge 注册到 shutdown 序列
5. **测试** — 集成测试使用 `testcontainers-go` 启动 Kafka/RabbitMQ 容器

## 8. 技术决策

| 决策 | 选择 | 理由 |
|------|------|------|
| Kafka 库 | `github.com/segmentio/kafka-go` | 纯 Go，无 CGo 依赖，简单 API |
| RabbitMQ 库 | `github.com/rabbitmq/amqp091-go` | 官方维护的 AMQP 0-9-1 客户端 |
| 消费者线程模型 | 每个 topic/queue 一个 goroutine | 简单、与 Go 并发模型对齐 |
| 偏移提交/ack 时机 | 发布成功后立即提交 | At-least-once 语义，简单可靠 |
| 模板复用 | 直接调用 `webhook.ResolveChannel` | v2 已实现，零重复 |
