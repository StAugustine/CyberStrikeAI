# 桌面客户端 R1 免安装候选验收报告

> 验证日期：2026-07-31
> 分支：`codex/desktop-client`
> 交付类别：Windows/macOS 未签名免安装开发候选
> 状态：R1 当前交付门禁已通过，项目所有者已批准继续 R2

## 1. R1 功能矩阵

| 功能域 | 自动化验收 |
| --- | --- |
| 首启、登录与会话 | 新管理员 bootstrap、错误密码、登录、权限、退出、token 撤销、core 重启后旧 token 失效、重新登录 |
| AI 通道 | 系统凭据库存储、配置只保留引用、连接与流式调用；401、429、500、流中断和密钥脱敏 |
| 对话与 Agent | 流式首事件、取消、历史、分组、置顶、附件；eino_single、deep、plan_execute、supervisor 四种模式 |
| HITL、工具与监控 | 真实工具调用、拒绝后恢复、pending 清理、运行详情、通知、取消和关闭清理 |
| 工作流与项目 | 校验、dry-run、CRUD、执行/回放、包导入导出；项目、事实、关系、攻击链生成与持久化 |
| 资产、漏洞与任务 | 资产导入/合并/筛选、信息收集四服务商、漏洞统计/导出/批量操作、批任务并发/暂停/取消/定时/重跑 |
| 知识库与扩展 | 知识 CRUD、扫描、索引、检索、日志和 Agent 调用；Agent 角色、Skills、Markdown Agents、文件管理、审计 |
| MCP | stdio、Streamable HTTP、SSE 的发现、schema、Agent 调用、启停、失败诊断、重启恢复和子进程回收 |
| 数据生命周期 | 升级恢复点、旧实例导入、显式恢复、保留策略、ENOSPC、凭据库失败和中断续接 |
| 范围边界 | Terminal、WebShell、C2、机器人、平台多用户 RBAC、远程服务从导航、页面、路由、后台服务、Agent 工具和发布资源中排除；R1 不展示 API 文档/插件入口 |

核心证据是 `TestDesktopCoreLocalAdminGoldenPath`、升级/维护集成测试、Web 资源边界测试、Tauri 单元测试和三个原生 runner 的生命周期冒烟。R1 黄金路径还会关闭并重新启动 core，复核登录、会话失效、对话、分组、附件、管理对象、外部 MCP 和审计数据。

## 2. 三平台候选

[基础桌面流水线 30684180212](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30684180212)在 Windows x64、macOS arm64 和 macOS x64 上全部通过：

- Go 桌面 core 与 R1 黄金路径；
- 桌面 Web 单元测试和资源 manifest；
- Rust 格式、Tauri 单元测试和同架构 sidecar；
- 原生未打包构建、首次启动/READY/退出、单实例和失败恢复冒烟。

[免安装候选流水线 30684180216](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30684180216)在同三个目标上全部通过：

- release 主程序、Go sidecar、版本化默认资源和 ZIP 结构；
- 安全路径、架构、敏感内容、排除资源、SBOM 和 SHA-256；
- 打包 sidecar 的真实恢复与恢复前恢复点复核；
- 删除程序目录后数据保留、重新解压和再次运行。

## 3. 发布判定

R1 当前目标是每个平台一个免安装 ZIP，不交付安装器。三个 artifact 均带 `portable-unsigned` 标记且仅作为开发候选：

- Windows 目标机需要已有 WebView2 Evergreen Runtime；
- macOS 候选未签名和公证，Gatekeeper 可能阻止直接打开；
- 自动安装更新保持关闭；
- 未配置 Windows 代码签名、Apple Developer ID 与 notarization 前，不将其描述为公开稳定版。

项目所有者已接受免安装交付并授权按计划持续执行，因此 R1 开发候选门禁关闭，进入 R2 本地插件联动。签名、公证和真实设备长时间睡眠/唤醒仍保留在最终公开分发门禁中，不通过降低系统安全设置规避。
