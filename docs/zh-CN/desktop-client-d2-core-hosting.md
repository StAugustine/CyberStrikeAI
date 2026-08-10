# 桌面客户端 D2 Go Core 可托管改造报告

> 验证日期：2026-07-31
> 分支：`codex/desktop-client`
> 状态：已完成；本地门禁与 Windows x64、macOS arm64、macOS x64 CI 全部通过

## 1. 交付结果

| 能力 | 实现与证据 |
| --- | --- |
| 调用方监听器 | `App.Serve(ctx, net.Listener)` 接管调用方监听器，测试使用 `127.0.0.1:0` 获得真实随机端口 |
| 就绪状态 | `/health/live` 始终报告进程存活；`/health/ready` 仅在主服务进入 serve loop 后返回 200 |
| CLI 兼容 | `Run`、`RunWithContext` 保留；`cmd/server` 只调整信号与资源关闭顺序 |
| 协议退出 | 请求基准 context 随 core 取消；SSE handler 收到取消；已劫持 WebSocket 在关闭时主动断开 |
| 幂等关闭 | `App.Shutdown`、Agent 任务管理器与外部 MCP 管理器均支持重复调用 |
| 后台任务 | 审计、监控、HITL、stale execution、工作流清理、知识库索引、Agent 调度和 MCP 刷新均接入生命周期 |
| MCP/Agent | 关闭时取消内部与外部 MCP 执行、Agent/Eino 执行并暂停批任务，等待批任务 executor 退出 |
| Web 资源 | `App.WithWebFS` 支持调用方注入；桌面嵌入包包含 R1/R2 核心静态资源并排除 C2、Terminal、WebShell、机器人、多用户 RBAC 与 xterm 专用文件 |
| 启动握手 | `desktop/protocol/handshake.schema.json` 固化 v1 `READY`/`BOOTSTRAP_REQUIRED`；Go 解析未知字段兼容，版本、类型和非精确回环 URL 失败关闭 |

## 2. 验证记录

本地执行并通过：

```bash
go test ./internal/... ./cmd/... ./web
go test -race ./internal/app ./internal/desktopprotocol ./internal/handler ./internal/mcp
go build -o .tmp/desktop-d2/bin/cyberstrike-ai ./cmd/server
```

协议、嵌入资源和连接退出切片 `266ea82` 已在 [Windows x64、macOS arm64、macOS x64 工作流](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30656715458)全部通过。后台生命周期提交 `44f14e1` 的[最终工作流](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30657471951)也在三个目标平台全部通过。

`go vet` 额外验证并修正了 TLS/HTTP2 初始化提前失败时请求 context 可能未释放的问题。

额外发现并修正两处原有竞态：

- MCP 执行快照原先在 worker 写状态时无锁读取，现统一在服务锁内复制。
- 外部 MCP handler 原先先启动异步连接、再原地展开共享配置切片和映射，现改为先规范化再交给 manager。

## 3. D2 门禁对应

| 门禁 | 状态 |
| --- | --- |
| `cmd/server` 构建与全量 Go 回归 | 通过 |
| 端口 0 启动并获得实际地址 | 通过 |
| `live/ready` 与版本信息 | 通过 |
| SSE/WS 在 core 取消后退出 | 通过，含 race 测试 |
| 内部/外部 MCP、Agent、数据库有序关闭 | 通过，含幂等测试 |
| 精选 Web 资源不依赖 CWD | 通过 |
| 版本化握手与未知字段兼容 | 通过 |
| Windows/macOS 最终提交验证 | 通过 |

## 4. 后续边界

D2 只提供可托管 core 和精选资源边界，不提前实现 D3/D5：平台绝对路径、首次启动密码通道、系统凭据库、Tauri 正式 sidecar 接入，以及从共享模板、路由和初始化中彻底移除排除能力，分别在 D3 和 D5 完成。
