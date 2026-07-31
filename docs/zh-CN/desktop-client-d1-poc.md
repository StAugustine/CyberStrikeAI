# 桌面客户端 D1 技术验证报告

> 验证日期：2026-07-31
> 分支：`codex/desktop-client`
> 状态：Windows x64、macOS arm64、macOS x64 三目标 CI 全部通过
> 结论：Tauri v2 + Go sidecar 路线通过 D1 协议与生命周期门禁，可以进入 D2

## 1. 验证范围

D1 只验证桌面壳与进程/协议边界，不迁移任何现有业务模块：

- Tauri 启动打包的最小 Go sidecar，sidecar 绑定 `127.0.0.1:0`。
- sidecar 通过 stdout 发送带版本的 JSON `READY`，Tauri 只接受显式 IPv4 回环 HTTP URL。
- WebView 直接加载 Gin 页面，并在真实 WebView 内验证 REST、EventSource/SSE 和 WebSocket。
- 只允许 Tauri 内置资源和本次 READY 回环源导航，外部导航默认拒绝。
- 下载事件必须先到达 Tauri；PoC 主动取消测试下载，不写用户目录。
- 验证单实例、优雅关闭、sidecar 意外崩溃和五秒强制关闭。
- 保持现有 `cmd/server` 不变；PoC 使用独立的 `cmd/desktop-poc-sidecar`。

## 2. 固定版本与产物

| 组件 | 固定版本或结果 |
| --- | --- |
| Tauri Rust crate | 2.11.5 |
| Tauri CLI | 2.11.4 |
| `tauri-build` | 2.6.3 |
| `tauri-plugin-shell` | 2.3.5 |
| `tauri-plugin-single-instance` | 2.4.3 |
| Rust | stable 1.97.1；本机临时安装在仓库 `.tmp/desktop-d1/` |
| Go sidecar | `cyberstrike-go-poc-<target-triple>`，CGO 关闭 |
| 锁文件 | `desktop/package-lock.json`、`desktop/src-tauri/Cargo.lock` |

源码目录为 `desktop/`，构建脚本根据 Rust target triple 生成正确命名的 sidecar。当前支持脚本目标为 `aarch64-apple-darwin`、`x86_64-apple-darwin` 和 `x86_64-pc-windows-msvc`。

## 3. macOS arm64 本机结果

| 验证 | 结果 |
| --- | --- |
| Go sidecar 单元/集成测试 | 通过；随机回环、READY、REST、SSE、WebSocket、跨源 WS 拒绝、stdin SHUTDOWN |
| Rust/Tauri 单元测试 | 6/6 通过；READY 版本、URL allowlist、浏览器结果和导航策略 |
| Tauri 无安装包 debug 构建 | 通过；主程序与 sidecar 被收集到同一输出目录 |
| WebView lifecycle smoke | 通过；真实 WKWebView 回报 REST、EventSource、WebSocket、外链拒绝，Tauri 捕获并取消下载，正常退出码 0 |
| 稳定性复跑 | 正常 lifecycle 连续两次通过 |
| 单实例 smoke | 通过；第二次启动触发首实例聚焦回调，未形成第二套 core |
| sidecar 崩溃 smoke | 通过；sidecar 受控退出 17，桌面主进程最终退出 1 |
| 强制关闭 smoke | 通过；sidecar 忽略 SHUTDOWN，约 5.7 秒后被强杀，桌面主进程退出 2 |
| 进程残留 | 已检查；本机冒烟后无主进程或 sidecar 残留 |

代表性命令：

```sh
go test ./cmd/desktop-poc-sidecar
cargo test --locked --manifest-path desktop/src-tauri/Cargo.toml
cd desktop && npm run poc:build
cd desktop && npm run poc:smoke
cd desktop && npm run poc:smoke:single-instance
cd desktop && npm run poc:smoke:failures
```

本机关闭竞态验证曾发现：下载取消请求刚发生就关闭可能让 Go HTTP shutdown 等到超时。PoC 在自动 smoke 收到协议与下载事件后保留 250ms 排空窗口，同时壳层现在严格检查关闭期间的 sidecar 退出码，非零不再被误报为成功。

## 4. 验证中发现并修复的问题

| 问题 | 处置 |
| --- | --- |
| sidecar 与 Cargo 包同名导致 Tauri 拒绝构建 | sidecar 改名为 `cyberstrike-go-poc` |
| Rust API 中误把 `binaries/` 写进 sidecar 运行名 | 运行名只使用 Tauri 配置中的逻辑名称 |
| Tauri 生成上下文要求 RGBA 图标 | PoC 复用仓库已有 RGBA 资源；正式图标仍是 D7 外部依赖 |
| 退出回调重复拦截最终退出，主进程不结束 | 只拦截第一次退出，关闭状态中的最终退出放行 |
| macOS event loop 未保留请求的非零退出码 | 壳层原子记录首个失败码，Tauri 清理后显式返回 |
| 关闭期间 sidecar 非零退出曾被当作成功 | `Terminated` 始终检查 sidecar 退出码 |
| 仅验证 endpoint、未证明 WebView JS 协议 | 页面把真实 WebView 结果回报 sidecar，再由 stdout 回报壳层作为 smoke 门禁 |
| CI 在生成 sidecar 前运行 `cargo test` | 在 Rust 测试前按矩阵 target 构建 Go sidecar |
| Windows 资源编译要求 `.ico` | 复用 `web/static/favicon.ico` 并将其加入 Tauri 图标配置 |

## 5. 三平台 CI 矩阵

`.github/workflows/desktop-d1.yml` 已配置：

| 目标 | GitHub runner | 当前状态 |
| --- | --- | --- |
| Windows x64 | `windows-2025` | [通过](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30654959205/job/91236982659) |
| macOS arm64 | `macos-15` | [通过](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30654959205/job/91236982594)；另有本机通过证据 |
| macOS x64 | `macos-15-intel` | [通过](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30654959205/job/91236982586) |

每个作业都执行 Go 测试、Rust 格式与单测、无安装包 Tauri 构建、正常 lifecycle、单实例、崩溃和强制关闭 smoke。runner 标签依据 GitHub 当前官方 runner 列表选择；工作流只授予 `contents: read`。

## 6. 门禁结论

提交 `e8e21bc` 对应的 [三目标工作流](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30654959205) 已全部通过：

- Windows x64 在 WebView2 中通过 REST、EventSource/SSE、WebSocket、下载拦截、单实例、崩溃和强制关闭验证。
- macOS arm64 与 macOS x64 在 WKWebView 中通过同一组构建和运行验证。
- 三个作业均使用固定 Rust/Tauri 版本、锁定依赖和目标平台原生 runner。

D1 标记完成，不触发 Electron 回退评审，可以进入 D2 的共享 `internal/app`/`cmd/server` 改造。本机缺少完整 Xcode 仍会阻塞后续签名、公证和正式 DMG 验收，但不影响 D1 结论。

## 7. 参考

- Tauri sidecar：<https://v2.tauri.app/develop/sidecar/>
- Tauri shell plugin：<https://v2.tauri.app/plugin/shell/>
- Tauri single-instance plugin：<https://v2.tauri.app/plugin/single-instance/>
- GitHub-hosted runners：<https://docs.github.com/en/actions/reference/runners/github-hosted-runners>
