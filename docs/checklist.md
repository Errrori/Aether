# Aether — 执行清单

## 阶段 0：PRD 定稿

- [ ] 同步以下决策到 `docs/prd.md`：
  - [ ] FR-1.6：频道名禁止 `*` 字符
  - [ ] FR-1.8：JWT 仅通过查询参数 `?token=` 传递
  - [ ] FR-1.9：允许多次 subscribe 追加频道，重复订阅忽略；单次最多 100 个频道，单连接累计最多 1000 个
  - [ ] FR-1.2：payload 为任意合法 JSON 值
  - [ ] FR-1.13：字段名统一为 `after_seq`（非 `after_seq_id`）
  - [ ] FR-1.22：Retention 按频道前缀模式配置，默认 TTL 30 天
  - [ ] FR-1.29：优雅关闭超时 10 秒
  - [ ] FR-1.30：日志需求（log/slog 结构化日志）
  - [ ] 7.1 技术栈表（Go 1.22+ / coder/websocket / net/http / pgx/v5 / log/slog）
  - [ ] 7.2 组件图更新（标注库选择）
  - [ ] 7.5 包结构更新（标注库选择）
  - [ ] 8.3 完整配置文件结构
  - [ ] 8.4 WriteMessage 改用 `FOR UPDATE` 行锁
  - [ ] 8.4 EvictExpiredMessages 改为按频道逐个清理（匹配 retention rules）
  - [ ] 6.2 补充订阅模型设计决策：断开即清除，重连重新订阅
  - [ ] 错误码表新增 40004（未知帧类型）
  - [ ] 发布 API 示例补充 payload 注释

---

## 阶段 1：项目脚手架 + 存储（第 1 周）

- [ ] 初始化 Go 模块 `go mod init`
- [ ] 创建目录结构：
  
  ```
  cmd/aether/main.go
  internal/config/config.go
  internal/auth/auth.go
  internal/hub/hub.go
  internal/ws/conn.go
  internal/api/handler.go
  internal/store/store.go      # 接口定义
  internal/store/postgres.go   # PG 实现
  internal/metrics/metrics.go
  ```
- [ ] 实现 `internal/config`：YAML 解析 + 环境变量覆盖 + 默认值 + 完整配置结构体
- [ ] 实现 `internal/store`：
  - [ ] 定义 `Store` 接口（WriteMessage / ReadHistory / EvictExpired / Migrate）
  - [ ] PG 连接池配置
  - [ ] Schema 迁移管理器（内建，启动时自动执行）
  - [ ] WriteMessage：`FOR UPDATE` 行锁 + 幂等去重
  - [ ] ReadHistory：按频道 + after_seq 查询
  - [ ] EvictExpiredMessages：按频道匹配 retention rules 清理
- [ ] 实现 `cmd/aether/main.go` 入口：配置加载 → PG 连接 → 迁移 → 启动服务
- [ ] 编写存储层单元测试

---

## 阶段 2：HTTP 发布 API + 认证（第 2 周）

- [ ] 实现 `internal/auth`：
  - [ ] API Key 验证（配置文件查找 + `ConstantTimeCompare`）
  - [ ] JWT Token 验证（HS256 + 明确算法列表 + exp 检查 + 30s 时钟偏移）
  - [ ] 频道授权检查（精确匹配 + 前缀通配符 `prefix.*` 递归匹配）
- [ ] 实现 `internal/api`：
  - [ ] `POST /api/v1/publish`：API Key 认证 → 频道名校验 → payload 大小检查 → Hub.Publish
  - [ ] `GET /api/v1/history`：API Key 认证 → 分页查询 → 返回空列表（非 404）
  - [ ] 统一错误响应格式 `{ "ok": false, "error": { "code": ..., "message": ... } }`
- [ ] 编写 API 端点测试

---

## 阶段 3：Hub + WebSocket（第 3 周）

- [ ] 实现 `internal/hub`：
  - [ ] 频道注册表（map[channel] → set[connection]）
  - [ ] 连接管理（注册/注销/断开清理）
  - [ ] 消息分发：持久化成功后异步推送到订阅者出站 channel
  - [ ] 出站缓冲区上限（默认 256），满则关闭连接
- [ ] 实现 `internal/ws`：
  - [ ] WebSocket 升级（`coder/websocket`）+ JWT 查询参数验证
  - [ ] Origin 头校验（默认拒绝跨源）
  - [ ] 连接生命周期：读 goroutine + 写 goroutine
  - [ ] 帧协议：subscribe / unsubscribe / message / subscribed / unsubscribed / gap / error
  - [ ] 标准 WebSocket ping/pong 心跳（30s 间隔，60s 超时）
  - [ ] 单次 subscribe 最多 100 频道，单连接累计最多 1000
  - [ ] 未识别帧类型返回 error（代码 40004）
- [ ] 编写 Hub + WebSocket 集成测试

---

## 阶段 4：历史回放 + 追赶（第 4 周）

- [ ] subscribe 带 `after_seq` 的历史回放逻辑
- [ ] 回放顺序保证：先回放完 → 再注册到实时订阅者集合
- [ ] `gap` 帧生成：历史不足以覆盖完整间隔时通知客户端
- [ ] 幂等去重：`(channel, idempotency_key)` 唯一约束，冲突时返回已有 seq_id
- [ ] 编写追赶和幂等场景测试

---

## 阶段 5：运维 + 指标（第 5 周）

- [ ] `GET /healthz` — PG 可写时返回 200
- [ ] `GET /readyz` — 完全初始化后返回 200
- [ ] `GET /metricsz` — Prometheus 指标：
  - [ ] aether_connections_active (gauge)
  - [ ] aether_channels_active (gauge)
  - [ ] aether_messages_published_total (counter)
  - [ ] aether_messages_pushed_total (counter)
  - [ ] aether_publish_duration_seconds (histogram)
  - [ ] aether_storage_write_duration_seconds (histogram)
- [ ] 优雅关闭：SIGTERM → 停止新连接 → 排空 WebSocket（10s 超时）→ 刷新存储 → 退出
- [ ] 后台驱逐任务：每 5 分钟按频道匹配 retention rules 清理
- [ ] 结构化日志（log/slog）：连接/断开/发布/认证/存储错误/驱逐统计
- [ ] 编写运维端点测试

---

## 阶段 6：测试 + 加固（第 6 周）

- [ ] 端到端集成测试：发布 → WebSocket 收到消息
- [ ] 历史回放测试：断线重连 → after_seq 追赶
- [ ] Gap 测试：离线超过 TTL → 收到 gap 帧
- [ ] 认证测试：无效 Key / 过期 Token / 未授权频道
- [ ] 幂等测试：重复 idempotency_key → 相同 seq_id
- [ ] 优雅关闭测试：SIGTERM → close 帧 → 进程退出
- [ ] 并发连接测试：目标 10,000 WebSocket 连接
- [ ] 延迟测试：P99 < 50ms
- [ ] 吞吐量测试：5,000 msg/s
- [ ] 崩溃恢复测试：kill → 重启 → 消息可读
- [ ] PG 连接断开恢复测试
- [ ] 编写 README.md
- [ ] 编写部署指南
- [ ] 提供示例 config.yaml

---

## 阶段 7：Docker 化（本地测试通过后）

- [ ] 编写 Dockerfile（多阶段构建：build → scratch/distroless）
- [ ] 编写 docker-compose.yml（aether + PostgreSQL）
- [ ] 验证容器化部署端到端流程
