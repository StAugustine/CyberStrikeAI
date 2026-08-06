# 桌面客户端 D3 路径、首启与桌面壳集成报告

> 验证日期：2026-07-31
> 分支：`codex/desktop-client`
> 状态：已完成；本地门禁与 Windows x64、macOS arm64、macOS x64 CI 全部通过

## 1. 交付结果

| 能力 | 实现与证据 |
| --- | --- |
| 运行路径 | Tauri 解析绝对数据、配置、缓存、日志、临时和只读资源目录；Go core 只接受壳层传入的绝对路径并在使用前验证可写性，对话附件固定写入数据目录下的 `chat_uploads` |
| 默认资源 | 版本化 manifest 校验 schema、应用版本、相对路径和 SHA-256；首次复制、用户修改保留、默认文件升级与失败回滚均有测试 |
| 正式 sidecar | Tauri 只通过参数传递非敏感路径与版本；stdout 专用于 v1 握手协议，运行日志进入 stderr；READY 仅接受精确 `127.0.0.1` 随机端口 |
| 首次初始化 | 独立 bootstrap 窗口通过受限 Tauri command 将密码写入 sidecar stdin；密码不进入 URL、命令行、日志、页面存储或持久文件 |
| 凭据保护 | macOS Keychain 与 Windows Credential Manager 保存随机账户项；YAML 只保留 `keyring://` 引用；旧明文密钥必须经独立窗口明确确认后迁移，失败回滚 |
| AI 向导 | 桌面首启检查默认 AI 通道凭据状态，支持连接测试、401/429 分类、保存与热应用；浏览器模式不显示桌面入口 |
| 启动恢复 | 启动期 stderr 只映射为固定安全类别；错误页提供重试、打开固定日志/数据目录和安全退出，不向 WebView 暴露原始错误、路径或密钥 |
| 窗口与实例 | 恢复合法尺寸和最大化状态、始终居中避免屏幕外恢复；第二实例聚焦当前 bootstrap、迁移、错误或主窗口 |
| 壳层边界 | 内部页面、当前 core 回环源、下载和新窗口采用默认拒绝；bootstrap、凭据迁移、错误恢复和主窗口目录入口分别使用最小 capability allowlist |

## 2. 失败恢复门禁

| 场景 | 行为与验证 |
| --- | --- |
| 全新安装 | 创建临时运行根，完成资源安装、管理员 bootstrap、READY 和优雅关闭；生命周期 smoke 通过 |
| 配置或资源损坏 | core 失败关闭；壳层显示固定 `local_configuration` 类别并允许查看日志、修复后重试；损坏资源 smoke 通过 |
| 目录不可写或路径异常 | core 在打开配置、日志或数据库前验证目录；壳层映射为 `local_storage`，不自动删除用户数据 |
| 数据库打开/迁移失败 | sidecar stderr 不进入页面，只映射为 `local_data`；用户可查看日志、修复或恢复数据后重试 |
| 凭据库不可用 | 配置保持原状，新建凭据项回滚；壳层映射为 `credential_store`，提示解锁系统凭据库后重试 |
| 协议/版本不兼容 | 壳层拒绝 READY 并映射为 `version_mismatch`；不会导航到未经允许的 URL |
| core 运行中退出 | 当前进程代际失效并显示恢复页；重试生成新代际，旧 sidecar 的迟到事件不能终止新实例 |

## 3. 验证记录

本地执行并通过：

```bash
go test ./...
go vet ./...
go test -race ./cmd/desktop-core ./internal/app ./internal/desktopcredentials ./internal/desktopprotocol ./internal/desktopruntime ./internal/handler ./internal/mcp
node --test web/static/js/workflow-package-client.test.cjs web/static/js/workflow-package-ui.test.cjs web/static/js/desktop-setup-ui.test.cjs
cargo fmt --manifest-path desktop/src-tauri/Cargo.toml -- --check
cargo test --locked --manifest-path desktop/src-tauri/Cargo.toml
npm run desktop:build
npm run desktop:smoke
npm run desktop:smoke:single-instance
npm run desktop:smoke:failures
```

最终提交 `13d5f0d` 的 [GitHub Actions 运行](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30667174358)在 Windows x64、macOS arm64、macOS x64 上全部通过 Go/Web/Rust 测试、sidecar 和 Tauri 构建、生命周期、单实例与失败场景 smoke。

Windows CI 同时发现并修正两个已有 handler 测试未关闭 SQLite 的问题；测试现在在 `t.TempDir` 清理前显式关闭连接，不再依赖 Unix 可删除已打开文件的语义。

## 4. D4 交接边界

D3 只完成桌面壳、路径、配置、首启和凭据保护。D4 继续复用现有 Gin 页面和鉴权链，按登录、AI、对话/Agent/HITL、工具执行、监控/通知、设置、主题和国际化建立桌面黄金路径；Terminal、WebShell、C2、机器人、多用户 RBAC 和远程服务仍不进入桌面范围。
