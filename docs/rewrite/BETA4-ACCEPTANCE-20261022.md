# Folio 1.0-beta4 验收

2026-10-22；Linux amd64；运行目标 Ubuntu 24.04.5 LTS + systemd。

- 冻结产品提交：`def554967dae831184ee46d0ffccfbe393de43e6`，标签 `1.0-beta4`。
- 最终发行包：`folio-1.0-beta4-linux-amd64.tar.gz`。
- SHA-256：`c47c8d64ca4e7209f8c1f8f8a19a7bd72afc3167da83e6d11b245c90ede16d80`。
- BUILDINFO：上述提交、`working_tree_dirty=false`；`folio` 与兼容命令 `powerlinux` 字节一致。
- 验收元数据与文档可以在主线继续补充，不移动已发布产品标签，不混入新的产品代码。

## 源与包

| 检查 | 结果 |
| --- | --- |
| 前端测试 | 89 通过，含语言偏好、切换、翻译覆盖与占位符、真实软件计划摘要和敏感字段脱敏 |
| TypeScript 与 Next 静态构建 | 通过 |
| Go 全量 `-race` 与 `vet` | 通过；本轮不改变 Go 业务代码 |
| MySQL 驱动 `-race` | 通过 |
| 安装器测试 | 39 项，37 通过，2 项需 root 的特定归属夹具跳过；没有将跳过记为通过 |
| 安装器生成代码一致性、Bash 语法 | 通过 |
| 最终包烟测 | 私有 prefix 安装/重复安装、HTTP 健康与会话、Next 嵌入资源、运行锁、退出、重装、保留数据卸载通过 |

安装器新增真实 PTY 拒绝安装、管道脚本不等待输入、中英文成功与失败、无颜色输出、私有摘要权限及参数不泄露检查。历史安装器入口未修改。

## 浏览器

系统 Chromium，使用最终发行包、真实 REST 和 MCP，未模拟 API。

- 中文登录、四张首页卡片、五项导航；1440 / 768 / 390 / 320 像素宽度与五套主题。
- 页面创建/MCP 读取、MCP 更新/SSE、历史、真实并发冲突、手动合并、断线草稿与重试、Markdown XSS、来源身份、键盘与 Escape。
- 计划任务成功/失败及重启后的最终状态、关联任务详情、数据库备份动作及调度策略表单。
- 浏览器语言自动选择、中英文即时切换、密码输入保留、无页面重载、用户便签内容保持原文、偏好持久化、稍后设置与恢复入口。
- 英文四种宽度与五套主题；全局搜索选择动作后进入真实操作表单。

实测截图见 [beta4 截图索引](../screenshots/panel-1.0-beta4/README.md)。

## systemd 升级与恢复

隔离 QEMU，Ubuntu OCI 衍生系统镜像，真实 systemd，2 vCPU / 2 GiB。系统环境不代表官方云镜像的完整验收。

- beta3 → beta4 QA 包：中文引导、API/Agent 健康通过。
- QA 包 → beta3：英文回滚、原配置和数据保留、API 健康通过。
- 从独立 beta3 基线升级最终 beta4 包，验证上述精确 SHA-256；中文引导、API 与 Agent 均 active，实际 healthcheck 通过。
- 升级前后 `server.json`、`agent-policy.json` 与保留数据哨兵 SHA-256 一致。
- 重启以新的 boot ID 为证，重新确认最终 beta4 API/Agent 服务和数据。

原始成功日志随 `verification-logs.tar.gz` 发布，机器可读结果为 `ACCEPTANCE.json`。发布工作流从冻结标签重建，要求产物 SHA-256 与这里完全一致，再运行包与浏览器检查。

本轮不扩大 Debian/ARM、真实 DNS 提供商或公开 CA 的既有验收范围，不把历史 RC 的性能数字套用到 beta4。服务端外部诊断与用户内容保持原文，不将原始日志当成可随意改写的界面文案。
