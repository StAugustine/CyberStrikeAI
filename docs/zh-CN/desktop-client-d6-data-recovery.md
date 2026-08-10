# 桌面客户端 D6 数据导入、备份、升级与恢复报告

> 验证日期：2026-07-31
> 分支：`codex/desktop-client`
> 状态：已完成；本地门禁与 Windows x64、macOS arm64、macOS x64 CI 全部通过

## 1. 交付结果

| 能力 | 实现与证据 |
| --- | --- |
| 升级恢复点 | core 在资源、配置、凭据和数据库迁移前创建唯一恢复点；SQLite 使用在线备份 API 合并已提交 WAL，清单记录版本、时间、类型、权限、大小和 SHA-256，全部复核后才原子发布 |
| 中断续接 | pending 升级、导入和恢复状态均持久化；重启复用原恢复点或回滚未完成的目录切换，不重复生成升级备份，不接受不兼容降级 |
| 旧实例导入 | 支持 `v1.7.8` 至 `v1.7.x`；只导入白名单配置、数据库、附件、检查点和受管资源，移除远程服务、机器人、C2、Terminal、WebShell 和平台多用户数据 |
| 显式恢复 | 恢复前创建当前桌面恢复点；已选载荷先复制到同文件系统私有暂存区并再次校验，再切换配置和数据顶层目录，失败或异常退出恢复原布局 |
| 保留策略 | 目录逐项报告有效性、版本、文件数和空间；至少保留最近两个有效恢复点，pending 升级或导入绑定的恢复点不可删除 |
| 磁盘空间门禁 | 备份、导入和恢复在创建暂存区或切换数据前检查目标卷可用空间并保留 8 MiB 安全余量；SQLite 同时计入稳定源快照和目标快照；Windows 使用 `GetDiskFreeSpaceEx`，macOS 使用 `statfs` |
| 凭据库失败 | 首次写入、部分写入和配置持久化失败均回滚本次 keyring 项；明文配置保持原状以便用户解锁系统凭据库后重试，错误和 stdout 不暴露凭据 |
| 桌面界面 | 原生目录选择、导入预检/确认、恢复点目录、显式恢复和受保留策略约束的删除均通过固定 Tauri command 完成；WebView 不接收旧实例绝对路径，也没有通用 shell 权限 |

## 2. 失败不变式

- 磁盘空间不足、上下文取消、符号链接、特殊文件、损坏 SQLite、损坏清单、载荷篡改或凭据库锁定时，源数据和当前桌面布局不变。
- 导入预检不会修改旧实例；确认导入前当前桌面不变；取消只删除本次导入恢复点。
- 恢复提交前和提交后均有完整性检查；部分目录切换在下次 core 启动前回滚。
- 备份目录和恢复工作区使用私有权限，备份不递归包含备份自身或事务状态文件。

## 3. 发布物真实恢复

免安装发布验证不再只调用恢复点列表。每个原生 runner 都会：

1. 解压实际候选 ZIP 并校验主程序、Go core 架构和安全路径。
2. 在外置运行目录创建带 SHA-256 清单的恢复点并修改在线数据。
3. 调用压缩包内 sidecar 的固定 `restore-backup` 维护命令。
4. 验证目标内容恢复，且恢复前自动生成的新恢复点仍在目录中并通过完整性校验。
5. 删除程序目录、保留外置用户数据、重新解压并再次调用同一 sidecar。

[免安装候选流水线 30684180216](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30684180216)中的 Windows x64、macOS arm64 和 macOS x64 作业均通过上述恢复流程。对应三个未签名 artifact 分别为：

- `desktop-x86_64-pc-windows-msvc-portable-unsigned`
- `desktop-aarch64-apple-darwin-portable-unsigned`
- `desktop-x86_64-apple-darwin-portable-unsigned`

同一提交的[基础桌面三平台流水线 30684180212](https://github.com/StAugustine/CyberStrikeAI/actions/runs/30684180212)也全部通过 Go/Web/Rust 测试、原生构建和生命周期失败冒烟。

## 4. 本地验证

最终变更执行并通过：

```bash
go test ./...
go vet ./cmd/desktop-core ./internal/desktopcredentials ./internal/desktopmigration ./internal/desktopruntime
go test -race ./cmd/desktop-core ./internal/desktopcredentials ./internal/desktopmigration ./internal/desktopruntime
cargo fmt --manifest-path desktop/src-tauri/Cargo.toml -- --check
cargo test --locked --manifest-path desktop/src-tauri/Cargo.toml
npm run release:test
npm run release:audit
npm run release:sbom
npm run release:runtime
npm run release:checksums
```

本机还从 release `.app` 生成 macOS arm64 免安装 ZIP；审计、SBOM、打包 sidecar 恢复、恢复前恢复点复核和全部 SHA-256 校验均通过。

## 5. D6 门禁对应

| 门禁 | 状态 |
| --- | --- |
| 源数据始终不变 | 通过，含导入、WAL、ENOSPC、篡改和取消测试 |
| 失败路径可恢复 | 通过，含升级续接、部分恢复回滚和凭据库失败回滚 |
| Windows/macOS 可恢复 | 通过，三个原生 runner 均使用压缩包内 sidecar 完成真实恢复 |
| 恢复后数据可查询 | 通过，维护集成测试恢复并查询 SQLite，R1 黄金路径验证重启后登录和业务数据持久化 |
