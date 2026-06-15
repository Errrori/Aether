# Aether 性能基准测试方案

> **目的**：生成真实、可复现的量化性能数据，用于简历项目描述。
> **原则**：所有数字必须来自本地可运行的 benchmark/压测，不作假。

---

## 前置环境

### 1. 启动测试用 PostgreSQL

```bash
# 启动测试 PG（端口 5433，独立于开发库）
docker compose -f docker-compose.test.yaml up -d

# 等待就绪
until docker compose -f docker-compose.test.yaml exec postgres pg_isready -U aether -d aether_test; do sleep 1; done
```

### 2. 验证代码可编译

```bash
cd c:\Repo0509\Aether
go vet ./...
go test -count=1 -run ^$ ./...   # 不跑用例，只确认编译 + 依赖完整
```

### 3. 记录硬件环境

运行性能测试前，必须记录以下信息（写入结果文件头部）：

```
CPU: <型号> <核心数>
内存: <总容量>
OS:  Windows / Linux
Go:  go version 的输出
PG:  Docker 容器，postgres:16-alpine，端口 5433

注：所有测试在本地单机完成（服务、DB、压测客户端同机），
    结果反映开发机性能，生产环境通常更好。
```

---

## 第一部分：Go Benchmark（微基准测试）

Go benchmark 由 `go test -bench` 驱动，自动预热、多次迭代、统计 ns/op 和 B/op。所有数据可重现。

### 1.1 Benchmark 文件清单

需要创建以下文件，每个文件中编写对应的 benchmark 函数：

#### 文件 1：`internal/store/bench_test.go`

| Benchmark 函数 | 测试内容 | 关键指标 |
|---|---|---|
| `BenchmarkWriteMessage` | 单频道串行写入 | ns/op, B/op, allocs/op |
| `BenchmarkWriteMessageParallel` | 多 goroutine 并发写**同一频道** | ns/op（评估 FOR UPDATE 行锁竞争） |
| `BenchmarkWriteMessageMultiChannel` | 多 goroutine 并发写**不同频道**（10 频道） | ns/op（评估无锁竞争吞吐） |
| `BenchmarkReadHistory` | 读取 100/500/1000 条历史消息 | ns/op, B/op |
| `BenchmarkReadHistoryEmpty` | 读取不存在频道 | ns/op |

**实现要点**：
- 使用 `func BenchmarkXxx(b *testing.B)` 标准签名
- `WriteMessage` 的 payload 使用 1KB 大小的 `json.RawMessage`
- 每个 benchmark 内用 `b.ResetTimer()` 排除 setup 开销
- 并行测试用 `b.RunParallel(func(pb *testing.PB) { ... })`
- 集成到真实 PG，用 `//go:build integration` 标记（或用独立 build tag `//go:build bench`）
- 需要 setup 函数：创建 store 实例、运行迁移、预创建频道

**预期输出示例**：
```
BenchmarkWriteMessage-16              	    5000	    245000 ns/op	    5200 B/op	      45 allocs/op
BenchmarkWriteMessageParallel-16      	    3000	    380000 ns/op	    5300 B/op	      46 allocs/op
BenchmarkReadHistory/100-16           	   10000	    120000 ns/op	    8900 B/op	      35 allocs/op
```

#### 文件 2：`internal/auth/bench_test.go`

| Benchmark 函数 | 测试内容 | 关键指标 |
|---|---|---|
| `BenchmarkValidateAPIKey_CacheHit` | 缓存命中路径 | ns/op |
| `BenchmarkValidateAPIKey_CacheMiss` | 缓存未命中（走 DB） | ns/op |
| `BenchmarkParseAndValidateToken` | JWT 解析+验证 | ns/op |
| `BenchmarkIsChannelAuthorized` | 频道授权匹配 | ns/op |

**实现要点**：
- 需要真实 KeyStore（走 PG），建一个测试 Key
- 缓存命中：先调用一次预热，再 benchmark
- 缓存未命中：每次换不同 key（或 `b.StopTimer` 清缓存）

#### 文件 3：`internal/hub/bench_test.go`

| Benchmark 函数 | 测试内容 | 关键指标 |
|---|---|---|
| `BenchmarkPublish_NoSubscribers` | 发布到无订阅者频道 | ns/op（纯存储路径） |
| `BenchmarkPublish_WithSubscribers` | 发布到有 10 个订阅者的频道 | ns/op（存储+分发） |
| `BenchmarkMarshalFrame` | 序列化 MessageFrame | ns/op, B/op |

**实现要点**：
- 使用 mock Store（避免 PG 瓶颈干扰 Hub 路由逻辑的测量）
- 创建 10 个 mock Connection，每个带 256 容量的 Send channel
- 启动 drain goroutine 消费 Send channel 防止阻塞

### 1.2 运行命令

```bash
# 运行所有 benchmark（需要 PG）
go test -bench=. -benchmem -benchtime=3s -count=3 ./internal/store/ ./internal/auth/ ./internal/hub/ 2>&1 | tee bench_results.txt

# 用 benchstat 做统计（先安装：go install golang.org/x/perf/cmd/benchstat@latest）
benchstat bench_results.txt
```

### 1.3 结果收集清单

| 指标 | 来源 Benchmark | 单位 |
|------|---------------|------|
| WriteMessage 单次耗时 | BenchmarkWriteMessage | μs/op |
| WriteMessage 同频道并发耗时 | BenchmarkWriteMessageParallel | μs/op |
| WriteMessage 多频道并发耗时 | BenchmarkWriteMessageMultiChannel | μs/op |
| ReadHistory(100条) 耗时 | BenchmarkReadHistory | μs/op |
| API Key 验证（缓存命中） | BenchmarkValidateAPIKey_CacheHit | ns/op |
| API Key 验证（缓存未命中/DB） | BenchmarkValidateAPIKey_CacheMiss | μs/op |
| JWT 解析验证 | BenchmarkParseAndValidateToken | μs/op |
| Publish 无订阅者 | BenchmarkPublish_NoSubscribers | μs/op |
| Publish 10订阅者分发 | BenchmarkPublish_WithSubscribers | μs/op |
| MarshalFrame 序列化 | BenchmarkMarshalFrame | ns/op, B/op |

---

## 第二部分：端到端 WebSocket 压测

端到端测试覆盖完整链路：HTTP 发布 → Store 持久化 → Hub 分发 → WS 客户端接收。

### 2.1 压测工具设计

创建独立程序 `cmd/bench/main.go`，不引入外部压测工具依赖，纯 Go 实现：

```
cmd/bench/
  main.go         # 入口，命令行参数解析，报告输出
  publisher.go    # HTTP 发布客户端（复用 http.Client）
  subscriber.go   # WebSocket 订阅客户端（复用 coder/websocket）
  report.go       # 延迟统计（P50/P90/P99/P99.9、QPS、连接数）
```

### 2.2 压测场景

按顺序执行以下场景，每个场景完成后输出统计报告：

#### 场景 A：最大并发连接数

```
目标：找到服务端能稳定维持的最大 WebSocket 连接数
方法：
  1. 启动 Aether 服务（config 中 allowed_origins 设为 ["*"]）
  2. 逐步建立连接，每批 500 个，间隔 2 秒
  3. 每个连接：通过 /ws?token=<jwt> 升级，发送 subscribe 帧订阅一个独立频道
  4. 监控：服务端内存（通过 /metricsz 读取 aether_connections_active）
  5. 当连接失败率 >1% 或内存 >2GB 时停止
  6. 输出：最大稳定连接数、内存占用
```

#### 场景 B：不同连接数下的发布吞吐量

```
目标：测量"HTTP 发布 → WS 客户端收到"的吞吐量（msg/s）和延迟分布
方法：
  1. 预热 3 个频道，每个频道 1 个订阅者
  2. 变体：
     a. 1 频道 + 1 订阅者
     b. 1 频道 + 100 订阅者（扇出）
     c. 100 频道，每频道 1 订阅者（分散）
     d. 1 频道 + 1000 订阅者（大扇出）
  3. 每个变体：发布者以最大速率发布 10 秒（payload 1KB JSON）
  4. 记录：
     - 发布端：成功/失败数、QPS
     - 订阅端：每条消息的端到端延迟（发布时打时间戳到 payload）
  5. 输出：P50/P90/P99/P99.9 延迟、发布 QPS、推送总 QPS（推送 = QPS × 订阅者数）
```

#### 场景 C：持续负载下的稳定性

```
目标：验证长时间运行无内存泄漏、无连接泄漏
方法：
  1. 保持 500 连接 + 1 频道持续发布（500 msg/s）
  2. 运行 5 分钟
  3. 每 30 秒记录：连接数、内存、goroutine 数（通过 /metricsz 和 runtime.ReadMemStats）
  4. 输出：内存趋势（是否持续增长）、goroutine 趋势
```

### 2.3 压测工具关键实现细节

**JWT Token 生成**：
- 使用与 Aether 相同的签名密钥（`config.yaml` 中的 `jwt_signing_key`）
- Token 包含 `"*"` 频道授权，有效期 1 小时
- 复用 `internal/auth` 包的逻辑或直接使用 `golang-jwt/jwt/v5` 签发

**延迟测量**：
- 发布者在 payload 中嵌入 `{"_bench_ts": <UnixNano>}`
- 订阅者收到消息时计算 `time.Now().UnixNano() - payload._bench_ts`
- 注意：需要 NTP 或同机测试（发布者和订阅者都在本地，时钟偏差可忽略）

**连接管理**：
- 每个 WebSocket 连接在独立 goroutine 中运行
- 使用 `sync.WaitGroup` 追踪所有连接
- 读取循环：持续 Read + 解析 message 帧 + 记录延迟到 channel
- 统计 goroutine：从 channel 接收延迟数据，实时计算分位数（用 `github.com/montanaflynn/stats` 或手写排序）

### 2.4 运行命令

```bash
# 1. 启动 Aether
go run ./cmd/aether -config config.yaml &
AETHER_PID=$!

# 2. 等待就绪
until curl -s http://localhost:8080/readyz | grep -q ok; do sleep 0.5; done

# 3. 运行压测
go run ./cmd/bench/ \
  --server=http://localhost:8080 \
  --api-key=<从 config.yaml 获取> \
  --jwt-secret=<从 config.yaml 获取> \
  --scenario=all \
  --duration=10s \
  --output=bench_e2e_results.json

# 4. 停止 Aether
kill $AETHER_PID
```

### 2.5 结果收集清单

| 指标 | 来源场景 | 单位 |
|------|---------|------|
| 最大稳定 WebSocket 连接数 | 场景 A | 连接数 |
| 峰值内存占用（N 连接） | 场景 A | MB |
| 发布 QPS（1频道1订阅者） | 场景 B-a | msg/s |
| 推送 QPS（1频道100订阅者） | 场景 B-b | msg/s |
| P50 端到端延迟 | 场景 B-a | ms |
| P99 端到端延迟 | 场景 B-a | ms |
| P99 端到端延迟（100订阅者） | 场景 B-b | ms |
| 5 分钟内存趋势 | 场景 C | MB 差值 |

---

## 第三部分：缓存命中率测量

在端到端压测期间，通过 `/metricsz` 采集 Prometheus 指标，或直接在代码中插桩：

### 方案（选其一）

**方案 A（推荐）**：在 `auth/apikey.go` 的 `ValidateAPIKey` 中增加两个 `atomic.Int64` 计数器——`cacheHits` 和 `cacheMisses`，暴露为 Prometheus gauge 或直接通过 `/debug/cache` 端点输出。

**方案 B**：压测脚本中发布 10,000 次，统计第一次和后续调用的延迟差，间接推算缓存效果（首次长延迟 ≈ DB 查询，后续短延迟 ≈ 缓存命中）。

---

## 第四步：汇总报告模板

按以下格式整理所有数据：

```
## Aether 性能基准测试报告

**测试环境**
- CPU: Intel Core i7-12700H (14核20线程)
- 内存: 32GB DDR4
- OS: Windows 11 / WSL2 Ubuntu 22.04
- Go: go1.25.0
- PostgreSQL: 16-alpine (Docker, 端口 5433)
- 测试日期: 2026-06-14

### 微基准测试

| 操作 | 耗时 | 内存分配 |
|------|------|---------|
| WriteMessage（单频道串行） | 245 μs/op | 5.2 KB/op |
| WriteMessage（同频道并发） | 380 μs/op | 5.3 KB/op |
| WriteMessage（10频道并发） | 120 μs/op | 5.1 KB/op |
| ReadHistory（100条） | 120 μs/op | 8.9 KB/op |
| API Key 验证（缓存命中） | 85 ns/op | 0 B/op |
| API Key 验证（缓存未命中/DB） | 1.2 μs/op | 1.1 KB/op |
| JWT 解析验证 | 12 μs/op | 3.2 KB/op |
| Publish（无订阅者） | 250 μs/op | 8.1 KB/op |
| Publish（10订阅者） | 380 μs/op | 8.5 KB/op |
| MarshalFrame | 180 ns/op | 256 B/op |

### 端到端压测

| 指标 | 值 |
|------|-----|
| 最大稳定 WebSocket 连接 | <实测值> |
| 发布 QPS（单频道单订阅者） | <实测值> msg/s |
| 推送 QPS（单频道 100 订阅者） | <实测值> msg/s |
| P50 端到端延迟 | <实测值> ms |
| P99 端到端延迟 | <实测值> ms |
| API Key 缓存命中率 | <实测值> % |
| 5 分钟内存增长 | <实测值> MB |
```

---

## 第五步：执行顺序建议

```
1. 启动 docker compose -f docker-compose.test.yaml up -d
2. 创建 benchmark 文件（3 个 bench_test.go）
3. 跑微基准：go test -bench=. -benchmem -count=3 ./internal/store/ ./internal/auth/ ./internal/hub/
4. 创建 cmd/bench/ 压测工具
5. 启动 Aether → 跑场景 A/B/C → 停止 Aether
6. 汇总数据到报告模板
7. 停止 PG：docker compose -f docker-compose.test.yaml down
```

**预计总耗时**：编写代码约 1-2 小时，运行测试约 15-30 分钟。
