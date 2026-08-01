# 桌面客户端 D8 本地插件联动报告

> 验证日期：2026-07-31
> 分支：`codex/desktop-client`
> 状态：实现与本机门禁已通过，等待三个一级平台 CI 复核

## 1. 交付结果

D8 保留现有本地管理员登录、权限、HITL 和审计链，只新增无凭据的本地实例发现：

- 桌面用户菜单提供“本地插件联动”，默认关闭，启用和禁用均需用户明确确认。
- 启用后，Tauri 在用户配置目录写入权限受限、90 秒有效并每 30 秒刷新的 `plugin-discovery.json`；内容只有 schema、实例 ID、`http://127.0.0.1:<随机端口>`、应用版本和签发/过期时间。
- 桌面 core 不可用、用户禁用、正常退出、异常退出或下次进程初始化时立即删除发现文件；刷新失败也删除旧值。
- Chrome/Edge 使用 `com.cyberstrikeai.desktop` 原生消息宿主读取同一发现文件。宿主只接受固定的 `discover` 操作，使用 Chromium 长度前缀协议，并把错误收敛为不泄漏路径的通用结果。
- Burp 的 **Use Desktop** 直接读取用户明确启用后产生的发现文件；浏览器与 Burp 发现成功后均清空旧会话/密码状态，仍要求输入本地管理员密码并点击 **Validate**。
- 桌面 API 文档使用同窗口 `/api-docs` 入口，继续由本地 core 提供；桌面排除能力的路由仍不可达。

## 2. 身份和来源边界

浏览器扩展清单内置稳定公钥，官方扩展 ID 固定为 `okialefpaaimfgjelpednbehgebgkdgo`。桌面模式不再接受任意格式合法的 Chromium 扩展 Origin，只精确允许：

```text
chrome-extension://okialefpaaimfgjelpednbehgebgkdgo
```

独立 server 模式保留原有兼容行为。原生消息宿主清单也只登记上述精确 Origin；Chrome 与 Edge 使用各平台当前用户级注册位置，不申请系统级注册或管理员权限。

发现文件和两个消费端共同执行以下拒绝条件：

- 非绝对路径、符号链接、非普通文件、空文件或超过 16 KiB；
- macOS/Unix 上存在 group/other 权限；
- 未知或重复 JSON 字段、无效 UTF-8、额外尾随数据；
- 非 schema v1、实例 ID 或版本非法；
- 非精确 IPv4 回环 HTTP 地址、无端口、携带用户信息/路径/查询/fragment；
- 有效期超过 120 秒、已过期或签发时间超过 30 秒未来容差；
- 任何额外 Password、Token、Credential 或 Session 字段。

随机端口和发现文件都不是认证边界。发现完成后的登录、Bearer 会话、权限检查和审计仍由现有 Go core 处理。

## 3. 发布物

桌面便携包新增同架构 `cyberstrike-native-host` sidecar。发布脚本、内容审计和运行时架构检查同时要求主程序、core 与 native host 存在且匹配目标架构。

发布流水线另生成跨平台 `desktop-plugin-integrations-unsigned` artifact：

- `cyberstrikeai-browser-extension.zip`
- `cyberstrikeai-burp-extension.jar`
- `SHA256SUMS`

浏览器生产 ZIP 排除测试源码；Burp JAR 已包含 `DesktopDiscovery`，不新增运行时 JSON 依赖。

## 4. 本机验证证据

当前实现已通过：

- `go test ./...` 全量回归；
- 新发现协议及 native host 的 `go vet` 与 `go test -race`；
- Windows x64 native host 交叉编译；
- 15 个 Tauri/Rust 单元测试和 `cargo fmt --check`；
- 27 个桌面 Web/浏览器联动测试，其中包含固定扩展 ID、远程源、过期、超长有效期、响应额外字段和凭据字段拒绝；
- Java 11 Burp 发现协议可执行测试与完整 Maven JAR 构建；
- macOS arm64 真实 release `.app` 和免安装 ZIP；内容审计确认 native host 位于 `Contents/MacOS/`，三个可执行文件均为 arm64；
- 打包后真实数据恢复、恢复前恢复点、程序目录删除后用户数据保留、重新解压复用、590 组件 CycloneDX SBOM 和 SHA-256 清单。

三个干净原生 runner 的 Windows x64、macOS arm64 和 macOS x64 结果将在本提交 CI 完成后写入最终 R2 验收报告。
