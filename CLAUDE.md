# CLAUDE.md
This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Aether — 轻量级实时消息推送中间件。Go + PostgreSQL，发布-订阅模型，HTTP 发布 + WebSocket 推送。

## Documentation

所有设计文档和规范位于 `docs/` 目录：

1. 产品需求文档，当需要了解功能需求、非功能需求、API 设计、数据模型、架构、存储、安全模型、路线图时，查阅`docs/prd.md`
2. 实现规格书，当需要了解实现顺序、验收标准、关键接口、技术决策摘要等信息时，查阅`docs/SPEC.md`
架构、接口和存储设计参见 PRD 第 7-8 节及 SPEC 第 3-5 节。

## Tech Stack
此部分什么时候阅读？
- 只在实现对应模块需要时再阅读相关链接，避免污染上下文内容
| 组件        | 选型                             |
| --------- | ------------------------------ |
| Go        | 1.22+                          |
| HTTP 路由   | `net/http` 标准库                 |
| WebSocket | `github.com/coder/websocket`   |
| 数据库驱动     | `github.com/jackc/pgx/v5`      |
| 日志        | `log/slog`                     |
| 指标        | `prometheus/client_golang`     |
| JWT       | `github.com/golang-jwt/jwt/v5` |
| 配置解析      | `gopkg.in/yaml.v3`             |

## Testing

- 每次提交之前要使用 `go vet ./...` 确认无警告，`go test -race ./...` 全部通过
- 测试中优先使用标准库函数（如 `strings.Contains`、`strings.Repeat`），不要手写等价实现
- 测试中设置环境变量统一使用 `t.Setenv`，不要用 `os.Setenv` + `defer os.Unsetenv`
- 环境变量覆盖的测试应覆盖两种场景：覆盖已有值和补全缺失值

## Rules

- 一次只实现一个模块（按 SPEC 定义的顺序：config → store → auth → hub → api → ws），通过验收测试后再进入下一个模块，不得同时开发多个模块
- 返回结构体中多个字段回答不同问题时，每个字段应独立计算，不应因某一字段为空的条件而跳过其他字段
- 长循环体（如逐频道遍历清理）中应检查 ctx.Err() 以支持取消
- 测试辅助函数必须检查并报告错误，不要丢弃 error 返回值
- 需要提交到github仓库的情况（无需额外确认）：
  - 每完成一个目标（通过验收测试）后
  - 修复代码审查问题后
  - 修复 bug 后 
- 创建commit时用中文简明扼要地对本次commit进行总结
- 提交信息仅包含作者本人署名，不得添加 Co-Authored-By 或其他协作者标记
