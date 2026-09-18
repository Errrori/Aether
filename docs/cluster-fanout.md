# 集群模式跨节点扇出设计文档（ADR）

> v2 第3层交付物。记录"多节点共享 PG、跨节点消息扇出"的机制选型、行为边界与失败模式。开发前定义决策与验收边界（对应 SPEC 7.4），开发后回填实测证据（见文末）。

## 1. 背景与现状

- **单节点现状**：`hub.Publish` 在 `store.WriteMessage` 提交后遍历**本进程**的订阅表分发（`internal/hub/publish.go`）。多节点部署时各节点只认识本进程的连接，跨节点订阅者收不到消息。
- **写路径已多节点安全**：seq 由 DB 层分配（`channels.current_seq` + `FOR UPDATE` 行锁），全局有序、跨节点无冲突。本层不改动写路径的并发控制。
- **方向已在 PRD 定调**：PRD 8.1 为 v2 集群模式选择 LISTEN/NOTIFY——多节点共享同一 PG，不引入 Redis 等中间件；PRD 路线图 2b 明确"利用 LISTEN/NOTIFY 实现跨节点扇出，节点发现"。
- **已知缺陷**：`internal/store/evict.go` 的空频道清理与并发发布存在竞态（偶发 50301）。集群化会放大该竞态（N 个节点并发驱逐），已作为本层前置修复交付（SPEC 7.4.6）。

## 2. 决策摘要

| 决策点 | 选择 |
|---|---|
| 扇出机制 | PG LISTEN/NOTIFY，通知频道 `aether_messages` |
| 通知载荷 | 仅 `{node_id, channel, seq_id}`，接收方按 seq 回读消息体 |
| 通知发射位置 | 写入事务内（`SELECT pg_notify(...)`），与消息插入同事务提交 |
| 接收模型 | 专用 pgx 单连接 + 单 goroutine 串行消费，断线指数退避重连 |
| 去重 | 自身通知按 node_id 跳过；监听路径游标去重追赶/队列重叠；追赶按 `messages.origin_node`（迁移 v5）精确跳过本节点已内联投递的消息 |
| 游标推进 | 仅监听路径推进（自身/远端通知处理时）；本地内联投递不推进——避免越过仍在通知队列中的更早远端消息（规划期发现的漏投窗口） |
| 断连兜底 | 重连后 CatchUp（节点游标 + ReadHistory 分批补投）；超出保留窗口发 gap 帧；客户端离线由 `after_seq` 回放（既有机制） |
| 连接标识 | LISTEN 连接 `application_name = aether-cluster-<node_id>`（截断 63 字节），便于运维与测试定向定位 |
| 驱逐并发 | 会话级 `pg_try_advisory_lock`（固定 key，专用连接持有）实现 leader 化 |
| 单节点开销 | `cluster.enabled: false` 时全部旁路：零额外连接、零额外 SQL |

## 3. 备选方案评估

| 方案 | 优点 | 否决/保留理由 |
|---|---|---|
| **PG LISTEN/NOTIFY（选定）** | 零新增基础设施；事务内发射与提交原子；广播语义天然匹配"发布者不知道订阅者在哪台节点"；PG 已是唯一外部依赖 | 载荷 8000B 上限、通知不持久——均可应对（见第 4 节） |
| Redis Pub/Sub | 生态成熟、吞吐高 | 引入第二套基础设施与运维面，PRD 明确避免；且 Redis 通知同样不持久，不解决可靠投递问题 |
| NATS / JetStream | 可持久化、功能完整 | 同上；为"轻量中间件"定位引入重量级外部依赖 |
| outbox 轮询表 | 只依赖 PG；通知可持久 | 每消息写放大（outbox 行写入 + 清理）；N 节点轮询产生固定查询底噪与投递延迟抖动；LISTEN/NOTIFY 是 PG 面向此场景的原生机制，轮询是在重造它 |
| 节点间 HTTP 网格 | 直连、控制力强 | 发布节点不知道哪个节点持有订阅者 → 需要向所有节点广播 → 需自建节点发现/心跳/重试/背压，等于重造 LISTEN/NOTIFY 且更弱 |
| 逻辑复制 | PG 原生 | 面向整表复制的 DBA 级配置，粒度粗、延迟不可控，不适合应用级事件总线 |

## 4. LISTEN/NOTIFY 已知限制与应对

| 限制 | 应对 |
|---|---|
| 载荷上限 8000 字节（含频道名与转义开销） | 载荷只含 `node_id/channel/seq_id`（< 200B），消息体不进入通知；接收方按 seq 回读，内容与存储强一致 |
| 通知不持久：无监听者时丢弃 | 三层兜底：LISTEN 闪断 → 节点游标追赶；节点重启/客户端离线 → `after_seq` 回放；超出保留窗口 → gap 帧显式告知 |
| 通知按提交顺序投递，跨频道无全局序 | 单频道内顺序由 seq + 提交序保证；跨频道无顺序语义（与单节点现状一致） |
| LISTEN 绑定单个连接（会话状态） | `cluster.Listener` 独占一条 pgx 单连接，不进连接池；断开后重连并重新 LISTEN |
| 通知是数据库本地的（不跨库） | 部署前提文档化：**所有节点必须连接同一数据库**（同库不同 schema 之间不互通） |
| 每个 NOTIFY 广播给所有 LISTEN 者 | 无订阅者的节点仅做一次内存查表（`HasSubscribers`）即跳过，不产生回读查询 |
| 突发流量下回读放大（有订阅者的 N 个节点 × 每消息 1 次 SELECT） | 本层接受并测量（指标）；候选优化：同频道通知合并为一次 `ReadHistory(afterCursor)` 批量回读（列入第5层候选，见第 8 节） |

## 5. 关键时序

### 5.1 正常发布（Node A 发布，Node B 持有订阅者）

```
Publisher → Node A: POST /api/v1/publish
Node A (store.WriteMessage, 单事务):
    ensure channel → lock → seq=N → INSERT message
    → SELECT pg_notify('aether_messages', {node_id:A, channel, seq:N}) → COMMIT
PG → 所有 LISTEN 者: 通知送达（提交后）
Node A: 提交后本地扇出（现状路径，不依赖通知）；收到自身通知 → 按 node_id 跳过
Node B: 解码 → HasSubscribers(channel) = true → ReadMessage(channel, N) → 本地扇出 → 游标=N
Node C: 解码 → HasSubscribers(channel) = false → 跳过（无回读）
```

### 5.2 LISTEN 断连恢复

```
Node B: LISTEN 连接断开 → 记录 WARN, aether_cluster_connected=0 → 退避重连（1s→30s）
Node B: 重连成功 → LISTEN → CatchUp():
    anchor = 频道节点游标（仅由监听路径推进，不含本地内联投递）
    对每个本地有订阅者的频道: ReadHistory(after = anchor) 分批读取至短批
        origin = 本节点 → 跳过（发布时已内联投递，精确去重）
        其余 → 本地扇出；每批结束游标推进到批内最大 seq
    若 MinSeq > anchor+1（保留窗口已越过）→ 向该频道各连接发 gap 帧
Node B: 进入消费循环，处理断连期间队列中的通知
    seq <= 游标 → 去重跳过；自身通知 → 按 node_id 跳过并推进游标
```

## 6. 失败模式矩阵

| 场景 | 行为 | 兜底 |
|---|---|---|
| 发布事务回滚 | 通知不产生（同事务原子，PG 保证） | 无消息落库，无需兜底 |
| LISTEN 连接闪断 | 断连窗口内跨节点通知丢失 | 重连 CatchUp（节点游标）；窗口内本地发布与本地投递不受影响 |
| 回读时消息已被驱逐 | WARN + `_deliver_errors_total`，跳过该条继续 | 客户端可见 seq 跳变；保留窗口内的正常消息不受影响 |
| 订阅者所在节点崩溃 | 该节点全部连接断开 | 客户端重连到任意节点 + `after_seq` 回放（既有机制，不依赖本层） |
| PG 整体不可用 | 发布失败（50301）；LISTEN 退避重连 | 恢复后发布恢复；LISTEN 恢复后按游标追赶 |
| 慢消费者缓冲区满 | 既有逻辑：关闭连接（close 1012） | 客户端重连 + 回放（既有机制） |
| 热点频道通知风暴 | 单 goroutine 串行消费，回读延迟上升 | 指标可观测（received/deliver_errors/connected）；优化路径见第 8 节 |
| 两节点同时驱逐 | advisory lock 保证仅一个执行 | 未获锁节点跳过本轮，下轮重试 |

## 7. 与既有机制的交互

- **幂等**：命中重复 `idempotency_key` 的分支不发送通知（无新消息），与 S-4 语义一致。
- **Gap 帧**：追赶跨出保留窗口时复用既有 gap 帧语义，不新增帧类型、不改协议。
- **驱逐**：leader 化（advisory lock）+ 空频道清理竞态修复（SPEC 7.4.6）均为本层交付。
- **优雅关闭**：停止顺序 = 驱逐循环 → cluster.Listener → HTTP/WS。Listener 先于 ws 停止，保证排空阶段无新跨节点投递进入连接。
- **健康与指标**：`/healthz`、`/readyz` 语义不变；新增 cluster 指标（SPEC CL-13），降级状态以指标观测而非健康检查失败表达。
- **单节点旁路**：`cluster.enabled: false` 时不存在 LISTEN 连接、通知发射与 leader 锁，行为与第2层完全一致。
- **顺序语义**：跨节点下本地发布的内联投递可能先于仍在通知队列中的更早远端消息到达订阅者（乱序但不丢失）；同一节点并发发布在 v1 已存在同类现象。客户端以 `seq_id` 排序/去重，协议不承诺跨节点严格全序。

## 8. 未决事项（后续层候选）

- **同频道通知合并/批量回读**：热点频道下将连续通知合并为一次 `ReadHistory(afterCursor)`，控制 N 节点回读放大（第5层候选）。
- **Presence 聚合机制复用本通道的可行性**：SPEC 7.5 待细化，依据本通道实测表现决策。
- **多 PG / 分库场景**：当前明确不支持（通知是数据库本地的）。

## 9. 实测证据（开发后回填）

> 模块通过验收后补：跨节点端到端延迟实测、突发吞吐与回读放大测量、重连追赶用例结果、驱逐 leader 双节点验证。
