# HTTP 层迁移至 gin 设计文档（ADR）

> 记录 internal/api 从 net/http 标准库路由迁移至 gin 的决策过程、边界约束与行为差异。开发前定义决策与验收边界，开发后回填实测证据。

## 1. 背景与动机

### 1.1 现状

HTTP 层当前基于 `net/http` 标准库：`http.ServeMux`（Go 1.22 方法+通配符模式）注册 **19 条路由**，2 个手写中间件（`authMiddleware`、`adminMiddleware`）逐路由包装，handler 统一为 `func(w http.ResponseWriter, r *http.Request)`。

### 1.2 动机

- **可辨识度与技术广度**：gin 是 Go 求职市场与企业代码库中最常见的 Web 框架。在真实项目中有边界地使用并完整记录取舍，比单纯"不使用框架"更能展示工程判断力。
- **工程收益**：v2 端点增长（keys/webhooks/batch/webhook 接收）后，手写注册、路径参数解析、请求体读取的样板代码持续累积；路由分组（v1/v2/admin）、中间件链、统一请求体解码为第3层速率限制与第5层 SSE 提供自然的承载点。
- **原始决策依据失效**：PRD §7.1 选择标准库的理由是"5 个端点无需第三方库"，该前提已不成立。

## 2. 决策

| 决策点 | 选择 |
|---|---|
| 框架与版本 | gin v1.12.0（go.mod `go 1.25.0` 与项目一致） |
| 使用边界 | gin 只出现在 `internal/api/*.go` 与 `go.mod`；hub/store/auth/keymgmt/webhook/ws 零框架依赖 |
| Handler 契约 | `Server.Handler()` 继续返回 `http.Handler`（gin.Engine 即 http.Handler）；引擎在 `New()` 构建一次并缓存，`Handler()` 返回缓存实例 |
| /ws 挂载 | `gin.WrapH(s.wsManager)`，internal/ws 零改动，保持原始 hijack 路径 |
| 引擎配置 | `gin.New()`（不用 `Default()`：保留 slog，不引入 gin logger/recovery）；`HandleMethodNotAllowed=true`；`RedirectTrailingSlash=false`；`SetTrustedProxies(nil)`；`GIN_MODE` 未设置时 ReleaseMode |
| 响应格式 | 手写 `writeJSON`/`writeError` 保持原有字节输出（`Content-Type: application/json` 无 charset + `json.NewEncoder`），**禁用 `c.JSON`**（会追加 charset） |
| 中间件身份传递 | 保持现状：只校验、不向 handler 传身份（不新增 `c.Set`，避免行为变更） |
| 请求体解码 | 共享 `readBody`/`decodeBody`（ContentLength 快拒 + `io.LimitReader(max+1)`），保持 413 语义 |

### 2.1 边界强制手段

```bash
# 必须为空：框架使用范围守卫
grep -rn "gin-gonic/gin" --include=*.go internal cmd | grep -v "^internal/api/"
```

- `Handler() http.Handler` 契约由平价测试钉住（幂等 + 路由清单），现有 60+ 单测与 36 个集成用例零改动即回归证据。

## 3. 备选方案

| 方案 | 评估 |
|---|---|
| 继续 net/http | Go 1.22+ 路由能力足够（19 条路由仍可辩护），但分组/中间件链/绑定需继续手写，动机 1.2 中的收益无法获得 |
| chi | 更贴近 stdlib、依赖轻；生态与可辨识度不及 gin |
| echo | 能力相近；同样的框架锁定成本，社区规模小于 gin |
| gin v1.11.0 | 依赖更少（无 mongo-driver）；主版本更新且 go 1.25 对齐，故取 v1.12.0 |

## 4. 迁移映射（ServeMux → gin）

| 现状 | 迁移后 |
|---|---|
| `mux.HandleFunc("POST /api/v1/publish", auth(handler))` | 路由分组：`v1 := r.Group("/api/v1", s.authMiddleware); v1.POST("/publish", s.handlePublish)` |
| `r.PathValue("id")` | `c.Param("id")`（同位置参数名统一 `:id`，冲突在注册期 panic） |
| 中间件 `func(next http.HandlerFunc) http.HandlerFunc` | `func(c *gin.Context)` + `c.Abort(); return` / `c.Next()` |
| `r.URL.Query().Get(x)` / `r.Header.Get(x)` / `r.Context()` | `c.Query(x)` / `c.GetHeader(x)` / `c.Request.Context()` |
| 逐 handler 手写 body 读取 | `readBody` / `decodeBody` 共享 helper |

## 5. 行为差异清单

| 差异点 | net/http 现状 | gin 迁移后 | 处理 |
|---|---|---|---|
| 尾斜杠 `/healthz/` | 404 | 默认 301/307 | `RedirectTrailingSlash=false`，保持 404（平价测试钉住） |
| 方法不匹配 405 | 405 + `Allow` | 默认 404 | `HandleMethodNotAllowed=true` 恢复；响应正文文案有差异（`405 method not allowed` vs `Method Not Allowed\n`），无测试依赖 |
| `HEAD /healthz` | GET 模式自动匹配 HEAD → 200 | 405 | 接受差异，记录于此 |
| 路径清洗 `//x`、`/a/../b` | 301 重定向 | 404（未启用 `RedirectFixedPath`/`RemoveExtraSlash`） | 接受差异，记录于此 |
| keys/webhooks 请求体超限 | `LimitReader(max)` 静默截断 → 400/40003 | 413/41301 | 有意修正（与 publish 及 SPEC 一致），记录于此 |
| 响应 Content-Type | `application/json`（无 charset） | 保持 | 刻意不用 `c.JSON`（会追加 `; charset=utf-8`），平价测试钉住 |

## 6. 代价

- 依赖增量：gin v1.12.0 引入 `quic-go`（HTTP/3）、`mongo-driver/v2`（bson 绑定）、`validator`、`sonic` 等（间接依赖）；二进制体积与基准性能差值：**待实测回填**。
- 框架锁定：仅限 `internal/api`，核心包不受影响；如回退，改动面局限于该包。

## 7. 验证证据

> 开发完成后回填：单测/集成测试输出摘要、平价测试结果、边界 grep、手工 smoke、二进制体积与 bench 前后对比。

## 8. 后续接入点

- **第3层速率限制**：gin 组级中间件（`r.Group("/api/v1", rateLimit)`），复用既有错误信封（42901）。
- **第5层 SSE**：作为独立 handler 挂载（原生 `http.Handler` + `gin.WrapH`，或 gin handler）。
- **MQ 桥接**：绕过 HTTP 层直接调用 `Hub.Publish`，不受本次迁移影响。
