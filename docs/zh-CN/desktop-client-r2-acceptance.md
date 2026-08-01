# 桌面客户端 R2 免安装候选验收报告

> 验证日期：2026-07-31
> 分支：`codex/desktop-client`
> 交付类别：Windows/macOS 未签名免安装开发候选
> 状态：R2 当前未签名免安装开发候选门禁已通过

## 1. 版本与发布边界

| 组件 | R2 版本 | 门禁 |
| --- | --- | --- |
| Tauri 桌面壳与 Go core/native host | 0.2.0 | npm、Cargo、Tauri、资源 manifest 必须一致 |
| Chromium 浏览器扩展 | 0.4.0 | 固定官方扩展 ID，生产 ZIP 排除测试文件 |
| Burp Suite 插件 | 1.1.0 | Maven/Gradle/JAR 路径一致，Java 11 构建 |

当前只交付 Windows x64、macOS arm64 和 macOS x64 的未签名免安装 ZIP，以及浏览器扩展 ZIP 和 Burp JAR；不交付安装器和自动更新。安全差异审查技能按项目所有者决定本轮搁置，不属于本候选完成门禁，也不在本报告中作相关结论。

## 2. R1→R2 替换升级

发布运行时验收使用打包后的 R2 sidecar 构造合法 R1 0.1.0 资源状态和外置配置/数据标记，然后验证：

- 首次 R2 启动从 `BOOTSTRAP_REQUIRED` 到 `READY`，资源状态升级到 0.2.0；
- 自动生成且可校验的 0.1.0→0.2.0 升级恢复点；
- R1 数据和配置标记保持不变；Go 集成测试另确认系统凭据库 secret 不变，配置仍只保存 `keyring://` 引用；
- 删除完整程序解压目录不会删除外置用户数据；重新解压同一 R2 ZIP 后直接 `READY`，不会再次要求初始化或凭据迁移；
- 打包 sidecar 的显式恢复与恢复前恢复点继续有效。

macOS arm64 本机真实候选 `CyberStrikeAI-Desktop-0.2.0-macos-arm64-portable.zip` 已通过上述链路，包含 463 个安全路径条目，主程序、core 和 native host 均为 arm64；发布清单同时包含 590 组件 CycloneDX 1.6 SBOM、审计报告、运行时报告和 SHA-256。

## 3. 功能与页面范围

桌面渲染测试要求以下 20 个页面精确存在，缺少或新增都会失败：

`dashboard`、`chat`、`hitl`、`asset-overview`、`asset-library`、`info-collect`、`projects`、`vulnerabilities`、`chat-files`、`tasks`、`workflows`、`mcp-monitor`、`mcp-management`、`knowledge-management`、`knowledge-retrieval-logs`、`roles-management`、`skills-monitor`、`skills-management`、`agents-management`、`settings`。

以下 8 个页面必须不存在：`webshell`、`platform-rbac`、`c2-listeners`、`c2-sessions`、`c2-tasks`、`c2-payloads`、`c2-events`、`c2-profiles`。Terminal 和机器人配置控件也必须不存在；对应服务、路由、Agent 工具、脚本和专用发布资源继续由现有门禁排除。

R1 黄金路径已全量重跑，并新增以下 R2 断言：

- `/api-docs` 从当前本地窗口打开，受认证 OpenAPI 规范保留对话、文件等纳入接口，并过滤 Terminal、WebShell、机器人、C2、平台 RBAC 和漏洞机器人订阅前缀；共享 server 规范保持原行为；
- 浏览器与 Burp 只接受用户显式启用后的短时 IPv4 回环发现信息，发现后仍需本地管理员登录；
- 约 4.2 MB 受管文件完成上传、下载、core 重启后复核和删除；
- 并发度 2 的批任务、运行中暂停/取消、外部 MCP 三种传输和大工具输出落盘/有界返回继续通过。

## 4. 稳定性与故障恢复

自动化覆盖同一登录会话内的完整 R1/R2 管理操作、实时 SSE 首事件、流中断、取消、HITL、并发任务、大文件、core 正常重启、旧 token 失效、数据持久化、升级中断续接、恢复中断回滚、单实例、启动超时/资源损坏和子进程回收。页面回到前台时的会话/任务探测由现有 `focus`/`visibilitychange` 路径恢复。

当前交付是未签名开发候选。真实设备的多小时睡眠/唤醒、Windows 签名后首次启动杀毒扫描、macOS Developer ID/Gatekeeper 和公证行为仍属于公开稳定分发前的设备验收，不通过关闭系统保护规避，也不阻止本轮免安装开发候选。

## 5. 回归与发布证据

当前改动已通过：

- `go test ./...`、桌面相关 `go vet`、MCP/插件发现/native host 竞态测试；
- 16 个 Tauri/Rust 单元测试与 `cargo fmt --check`；
- 34 个桌面 Web、发布工具与浏览器发现 Node 测试；
- Java 11 Burp 发现协议测试、Maven 1.1.0 JAR 构建；
- macOS arm64 真实 0.2.0 `.app`/ZIP 构建、内容审计、R1→R2 替换、恢复、SBOM 与校验和。

提交 `8b8ec953e9c168be865084f72fa75ff8fd0b554a` 的最终远程门禁全部通过：

- [基础桌面三平台流水线 30689534440](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30689534440)：Windows x64、macOS arm64、macOS x64 的 Go/Web/Java/Rust、原生构建、生命周期、单实例和失败路径全部通过；
- [免安装与插件流水线 30689534423](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30689534423)：三个原生免安装 ZIP 均通过构建、内容审计、SBOM、解压运行、R1→R2 替换、恢复、数据保留、校验和与产物上传；浏览器/Burp 集成 artifact 同时通过；
- 产物为 `desktop-x86_64-pc-windows-msvc-portable-unsigned`、`desktop-aarch64-apple-darwin-portable-unsigned`、`desktop-x86_64-apple-darwin-portable-unsigned` 和 `desktop-plugin-integrations-unsigned`，保留至 2026-08-08。

项目所有者已接受免安装交付形式并授权按计划持续执行至完成，该授权作为本轮未签名开发候选的发布批准。安全差异审查技能仍按项目所有者决定搁置，本报告不作相关审查结论；代码签名、公证与真实设备长时间睡眠/唤醒继续属于未来公开稳定分发门禁。
