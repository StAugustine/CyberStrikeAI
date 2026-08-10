# 桌面客户端 D0 基线报告

> 基线日期：2026-07-31
> 分支：`codex/desktop-client`
> 参考提交：`main@5d1f5d28868f1935ece32d8d5fd538576d82aa3e`
> 状态：本机基线已固化；跨平台与正式发布环境见“未满足环境”

## 1. 可复现环境

| 项目 | 本机结果 |
| --- | --- |
| 操作系统 | macOS 26.5.2 arm64 |
| Go | go1.26.5 darwin/arm64，CGO 可用 |
| C 编译器 | Apple clang 21.0.0 |
| Node.js / npm | Node.js 22.23.1 / npm 10.9.8 |
| Rust / Cargo | 本机未安装；D1 使用仓库 `.tmp/` 下的临时稳定工具链验证 |
| Xcode | 未安装完整 Xcode，仅有 Command Line Tools；正式签名、公证和安装验收需补齐 |
| Windows WebView2 | 本机不可验证，必须由 Windows x64 runner/设备验证 |

Go 1.26 的 host-tool 缓存会尝试写入 Go 安装目录旁的缓存路径，因此在受限沙箱内执行时可能报 `operation not permitted`。该环境问题通过受控执行测试命令处理，未修改仓库外文件或项目配置。

## 2. 基线命令与结果

| 验证 | 命令 | 结果 |
| --- | --- | --- |
| Go internal 测试 | `go test ./internal/...` | 通过 |
| Go cmd 测试 | `go test ./cmd/...` | 通过；基线入口无测试 |
| Server 构建 | `go build -o .tmp/desktop-d0/bin/cyberstrike-ai ./cmd/server` | 通过 |
| 工作流包客户端测试 | `node --test web/static/js/workflow-package-client.test.cjs` | 4/4 通过 |
| 工作流包 UI 测试 | `node --test web/static/js/workflow-package-ui.test.cjs` | 8/8 通过 |

以上命令的 Go module cache、Go build cache、npm cache、临时文件和输出均放在仓库 `.tmp/desktop-d0/`，避免污染源码目录。

## 3. 静态基线清单

| 清单 | 数量或路径 | D1 以后用途 |
| --- | --- | --- |
| 页面面板 | 28 个；desktop 最终纳入 20 个、排除 8 个 | 生成范围边界验收清单 |
| Gin 路由注册调用 | 约 316 处 | D2 提取真实路由清单并建立 desktop allowlist |
| goroutine 启动点 | 约 121 处 | D2/D5 核对 desktop worker 与退出顺序 |
| `exec.Command`/`CommandContext` | 约 25 处 | D2/D5 核对子进程树与取消行为 |
| 持久化/运行目录 | `data/`、`tools/`、`roles/`、`skills/`、`agents/`、`knowledge_base/` | D3 改为应用数据目录绝对路径 |
| 主要 SQLite | `data/conversations.db`、`data/knowledge.db` | D3/D6 迁移、备份和恢复验证 |

这些数字是用于发现漂移的静态基线，不等同于最终运行时清单。D2 必须从实际 desktop 配置画像生成路由、worker 和子进程清单。

## 4. D0 测试数据与环境登记

| 场景 | 无真实凭据方案 | 执行阶段 |
| --- | --- | --- |
| READY、REST、SSE、WebSocket、退出 | `cmd/desktop-poc-sidecar` 自包含测试服务 | D1 自动化 |
| 首启、配置和 SQLite | 在 `.tmp/` 动态创建最小配置与临时数据库，测试结束清理 | D2/D3 自动化 |
| AI 成功/401/429/500/流中断 | `httptest.Server` 假 AI endpoint | D3/D4 自动化 |
| 外部 MCP 启动/退出/超时 | 测试进程通过 stdin/stdout 模拟 MCP，不连接真实服务 | D2/D5 自动化 |
| 本地安全工具 | 使用固定假可执行文件验证 PATH、输出和取消；真实工具仅在授权靶场人工验收 | D5 自动化 + 人工 |
| 数据导入/升级失败 | 从测试动态生成带 WAL 的 SQLite 样本和损坏副本 | D6 自动化 |
| Windows/macOS 安装与原生壳 | 干净 Windows x64、macOS arm64、macOS x64 runner/设备 | D1、D7、D9 人工/CI |

所有自动化夹具都不得包含生产密钥、真实用户数据或公网攻击目标。需落盘的临时数据统一放在仓库 `.tmp/`；提交到仓库的固定夹具只能是可审查的合成数据。

## 5. 未满足环境与停止条件

- 本机没有完整 Xcode，不能把当前结果视为签名、公证、DMG 或正式安装验收。
- 本机不能验证 Windows WebView2、Windows Defender/第三方终端防护和 Windows 安装包。
- 当前没有 macOS x86_64 原生设备证据；交叉构建不能替代启动与协议验证。
- D1 只有在 Windows x64、macOS arm64、macOS x64 三种目标都通过协议与生命周期验证后才能完成。

这些是计划中已有的明确人工/CI 环境，不把它们误记为代码失败，也不允许跳过 D1 门禁进入 D2 全量集成。
