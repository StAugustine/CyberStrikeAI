# 桌面免安装候选发布

桌面客户端当前为开发候选，提供以下三个免安装 ZIP：

| 目标 | ZIP 内容 | 启动方式 |
| --- | --- | --- |
| Windows x64 | 主程序、Go sidecar、`defaults/`、许可证和说明 | 解压后运行 `CyberStrikeAI Desktop.exe` |
| macOS arm64 | 完整 `.app`、许可证和说明 | 解压后打开 `CyberStrikeAI Desktop.app` |
| macOS x64 | 完整 `.app`、许可证和说明 | 解压后打开 `CyberStrikeAI Desktop.app` |

不要只复制主程序。主程序、sidecar 和 `defaults/` 属于同一个版本，必须保留 ZIP 中的目录结构。

## 数据与替换升级

用户数据不写入解压目录，而是写入操作系统为 `com.cyberstrikeai.desktop` 分配的用户数据、配置、缓存和日志目录。因此：

- 删除解压目录只删除程序，不自动删除用户数据。
- 替换升级时先完全退出应用，再删除旧程序目录并解压新 ZIP。
- 新版本首次启动会复用原用户数据，并执行既有的备份、迁移和失败恢复流程。
- 需要清除或恢复数据时使用应用内“数据导入与恢复”，不要手工拼接新旧 `defaults/`。

Windows 便携包不安装 WebView2。目标电脑必须已有 Microsoft Edge WebView2 Evergreen Runtime；缺失或损坏时应先通过 Microsoft 官方渠道修复。macOS 开发候选未签名和公证，可能被 Gatekeeper 阻止；正式对外分发前必须完成 Developer ID 签名和 notarization，不能把关闭系统安全检查作为发布步骤。

## 自动构建与证据

`.github/workflows/desktop-release-candidate.yml` 在三个原生 runner 上执行：

1. 固定 Go、Node、Rust、Tauri CLI 和锁文件。
2. 生成同架构 Go sidecar，并构建 Windows release 主程序或 macOS `.app`。
3. 创建免安装 ZIP，复核版本、许可证、资源白名单和排除模块资源。
4. 扫描真实配置、数据库、日志、临时文件、私钥和访问密钥模式。
5. 安全解压 ZIP，校验主程序与 sidecar 架构；使用打包后的 sidecar 恢复已校验数据，并复核自动生成的恢复前恢复点。
6. 删除程序目录，确认外置测试数据仍在；重新解压并再次运行同一维护命令。
7. 生成 CycloneDX 1.6 SBOM、`audit-report.json`、`portable-runtime-report.json`、`release-manifest.json` 和 `SHA256SUMS`。

每个平台的 CI artifact 保留 7 天且明确标记 `portable-unsigned`。普通 PR 不读取任何发布私钥。

当前已验证的候选由 [GitHub Actions 运行 30684180216](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30684180216) 生成，三个 artifact 均已通过上述全部步骤：

- `desktop-x86_64-pc-windows-msvc-portable-unsigned`
- `desktop-aarch64-apple-darwin-portable-unsigned`
- `desktop-x86_64-apple-darwin-portable-unsigned`

同一提交的[基础桌面三平台流水线](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30684180212)也全部通过。artifact 是 GitHub 下载容器，解压后再使用其中带版本和平台名的产品 ZIP、SBOM、审计报告、运行时报告、发布清单与 `SHA256SUMS`。

## 本地命令

先设置 `CYBERSTRIKE_DESKTOP_TARGET` 和仓库内临时 `CARGO_TARGET_DIR`，再在 `desktop/` 执行：

```bash
npm ci --ignore-scripts
npm run release:test
npm run release:verify
npm run desktop:bundle:candidate
npm run desktop:package:portable
npm run release:audit
npm run release:sbom
npm run release:runtime
npm run release:checksums
```

打包、解压、运行时和发布输出目录通过工作流中的 `CYBERSTRIKE_RELEASE_BUILD_ROOT`、`CYBERSTRIKE_PORTABLE_STAGE`、`CYBERSTRIKE_PORTABLE_EXTRACT`、`CYBERSTRIKE_PORTABLE_RUNTIME`、`CYBERSTRIKE_RELEASE_OUTPUT` 和 `CYBERSTRIKE_RELEASE_SBOM` 指定。临时目录必须为空，防止旧产物混入新 ZIP。

发布前用平台工具复核 `SHA256SUMS`。自动更新安装产物保持关闭；签名更新链路完成前不提供静默替换程序目录的功能。
