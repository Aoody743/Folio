# Folio 首发上线前差距清单

更新：2026-10-18（UTC）。本文是接续开发和首发验收的当前执行清单；[第二阶段核心版收尾](PHASE2-CLOSEOUT-20261015.md)保留详细范围与退出条件。早期交接、阶段计划和 ARM64 记录中的历史目标不覆盖本文确认的首发范围。

## 当前结论

**RC7 已冻结为可复现的 amd64 候选，但首发验收尚未完成。** 产品源码提交 `20866c12870adde1ff1d0136e1b621ee2da4ac42` 是当前 GitHub `main` 的后代；干净构建包与独立重建包 SHA-256 均为 `ce35662bffd2349f938cca4034cc2a9b9331ddc254e2f8cd6046394284f5b69b`，源码检查和包 smoke 全部通过。RC7 在 Debian 13 克隆上通过升级、回滚/重升级、卸载/重装数据保留、Agent 文件、PostgreSQL 慢日志与备份恢复；Ubuntu 24.04 OCI 派生 guest 上通过升级、文件/终端及完整重启后的 API、Agent、OpenResty 验收。Ubuntu PostgreSQL 软件计划已修复并解析出 20 个签名包，但安装在该不完整 dpkg 镜像的 `postgresql-common` postinst 失败；这不能代替官方干净 Ubuntu cloud image 上的验收。PHP 源、MySQL 仓库、PHP 站点/TLS 和 G3/G4 剩余矩阵仍未通过，G5 未通过。GitHub `main` 仍为 `037b80832fe7e57ebfc08d3a8eef8f655fed70e1`，远端尚无 `legacy`；按你的条件，G1–G5 通过前没有改写远端分支。

### 2026-10-18 RC7 当前验收检查点

完整结果见 [RC7 证据](evidence/release-gates-rc7-20261018.json)。候选包 build info 记录 Go `1.27.2`、`working_tree_dirty=false`；RC7 全量 Go race、MySQL driver race、安装器 33 项、前端 82 项、类型检查、包 smoke 和双构建字节比较通过。

| 门槛 | RC7 结果 | 发布判断 |
| --- | --- | --- |
| G0 源码与候选包 | 固定源码提交、amd64 构建、全部源检查、包 smoke、独立重建一致；提交是当前 GitHub `main` 后代 | 通过 |
| G1 PostgreSQL | Debian RC7 慢日志四种协议、筛选、脱敏、停用和备份恢复通过；Ubuntu PostgreSQL 计划成功，但 `postgresql-common` 安装配置失败，无实际 Ubuntu 集群 | 部分通过 |
| G2 Web 与数据库安装 | Ubuntu PostgreSQL 包安装未完成；PHP/Sury 网络失败、PHP 站点/TLS 未测、MySQL 源未开放；RC7 只复验既有 Nginx/OpenResty 服务，没有完成干净双系统 Web/DB 矩阵 | 未通过 |
| G3 网页/MCP/任务/审计 | Debian/Ubuntu Agent 文件测试各 4/4，Ubuntu 终端 3/3；更完整的网页/MCP 权限、任务恢复及审计矩阵未完成 | 部分通过 |
| G4 生命周期与数据保护 | Debian RC6→RC7 升级、SIGKILL 后中断升级恢复、RC7→RC6 回滚再升级、卸载/重装后 Folio 登录与 PostgreSQL canary 保留通过；Ubuntu RC7 重启恢复通过。两台 2 vCPU/2 GiB guest 均记录了空闲健康接口延迟与服务 RSS；Ubuntu 卸载、代表性并发负载和系统变更清单仍未完成 | 部分通过 |
| G5 发布冻结 | G1–G4 仍有未完成项，不能把 RC7 标成首发完成 | 未通过 |

### 2026-10-18 RC5 冻结验收检查点

本轮在本地 `work` 分支创建冻结提交 `fee6e9189a74ba6eb3c828a5a956f3ff56a94f2a`，确认工作树干净后构建了 Folio `0.1.0-rc.5` amd64 包（SHA-256：`c4e4e817ab400e5a237ac0fe02ef6d26ac4d636904534ea54cac16ab01e8cb49`）。包内 `BUILDINFO` 记录 `working_tree_dirty=false`、Go `1.27.2`。Python/Go race、前端 82 项测试、类型检查和发行包 smoke 均通过；具体结果见[RC5 证据](evidence/release-gates-rc5-20261018.json)。RC5 已在隔离 Debian 13 验收机上从 RC4 升级，并复验 API/Agent/PostgreSQL 活动状态与 PostgreSQL 慢日志行为。原 RC4 验收摘要仍保留在[RC4 证据](evidence/release-gates-rc4-20261018.json)。原始日志位于当前 QA 工作区 `/workspace/tools/qa-acceptance/logs/`，不随仓库分发。

| 门槛 | 本轮结果 | 发布判断 |
| --- | --- | --- |
| G0 源码冻结与可复现包 | 本地 `work` 已冻结；RC5 干净构建、Python/Go race、前端测试/类型检查和包 smoke 通过。尚未整合到最新 `main`，也未做双构建字节一致性比较 | 部分通过 |
| G1 PostgreSQL 慢日志 | Debian 13 验收机升级到 RC5 后，实测 `statement`、`parse`、`bind`、`execute` 记录、数据库筛选、字面量/参数脱敏及启停生效；Ubuntu 尚未复测 | 部分通过 |
| G2 Web 与数据库安装 | Debian 13 上实际安装、管理并重启 Nginx 成功，受管 HTTP 返回 200。PHP 安装在 `php8.4-common` 配置阶段失败；此测试镜像的 dpkg 状态缺少基础包的 `Maintainer`/`Description` 元数据，需在干净官方镜像重跑。PostgreSQL 冷安装计划在 2 vCPU / 2 GiB TCG 验收机等待超过 360 秒未返回，安装链未通过。没有完成 PHP 站点、OpenResty、Ubuntu 或数据库选择/备份恢复验收 | 未通过 |
| G3 网页/MCP/任务/审计一致性 | Debian 13 上 Agent 文件实测 4/4、终端实测 3/3 通过，覆盖实际 MCP、WebSocket 和 SSH PTY；完整授权、撤权、任务状态及审计矩阵尚未完成 | 部分通过 |
| G4 系统生命周期与数据保护 | Debian 13 上此前已通过 RC4 全新/重复安装、服务重启、卸载保留数据、RC3→RC4 升级、中断恢复、回滚再升级及 API/Agent/Nginx 自启；本轮 RC4→RC5 升级保留数据并正常启动 API/Agent/PostgreSQL。Ubuntu、数据库备份恢复、PHP 失败恢复和 2 vCPU / 2 GiB 性能基线尚未验收 | 部分通过 |
| G5 发布冻结 | 依赖 G0–G4 全部通过；当前仍有未验收目标系统和核心功能 | 未通过 |

RC5 证明了当时 Debian 13 上的构建、包 smoke、升级和 PostgreSQL 慢日志路径可工作。RC5 的历史结果仍保留在上表，RC7 是当前候选。

### RC6 历史验收检查点（已由 RC7 替代）

RC6 修复了 Next.js 随机 build ID 使相同源码产物不一致的问题。提交 `0da6e18473815301621f862bf543fe49ed9d9f3a` 的干净构建包与独立重建包 SHA-256 均为 `290b4465b04f9506812f63c58163c5614abdc4c2373476fda2baa1130b887d7a`，`cmp` 逐字节一致。Go race、MySQL driver race、安装器 Python 33 项（2 项 root-only skip）、前端 82 项、类型检查及发行包 smoke 全部通过。完整结果见[RC6 证据](evidence/release-gates-rc6-20261018.json)；VM 原始日志留在 `/workspace/tools/qa-acceptance/`。

| 门槛 | RC6 结果 | 发布判断 |
| --- | --- | --- |
| G0 源码冻结与可复现包 | 干净 amd64 构建、全部源码检查、发行包 smoke、同提交独立重建字节一致；源码提交是当前 GitHub `main` 后代 | 本地候选通过；尚未发布 |
| G1 PostgreSQL 慢日志 | Debian 13 RC6 实测 statement/parse/bind/execute、筛选、脱敏、停用生效通过；Ubuntu 软件计划返回 `unsupported_platform` | 部分通过 |
| G2 Web 与数据库安装 | Ubuntu RC6 OpenResty 自启和 HTTP 200 通过（测试 guest 补齐缺失的 `/bin/sleep` 链接后复验）；Nginx 只在 RC5 实测。PHP/Sury 计划返回签名/网络错误，Ubuntu PostgreSQL/MySQL 计划不支持；PHP 站点、TLS 和官方干净 Ubuntu cloud image 未验收 | 未通过 |
| G3 网页/MCP/任务/审计一致性 | RC6 Agent 文件测试 Debian/Ubuntu 各 4/4；Ubuntu 实机终端 3/3；完整网页与 MCP 权限、任务、撤权及审计矩阵未完成 | 部分通过 |
| G4 系统生命周期与数据保护 | RC6 在 Debian/Ubuntu 升级通过；Ubuntu 完整重启后 API/Agent/OpenResty 恢复；Debian PostgreSQL 备份恢复通过。RC6 回滚、中断升级、数据保留卸载和 2 vCPU/2 GiB 性能基线未做 | 部分通过 |
| G5 发布冻结 | G1–G4 尚未全部通过同一候选包 | 未通过 |

当前 RC7 的源码快照和 amd64 包已经固定，阻塞点是实机门槛而非无法执行 Git 分支操作。云环境设置中已有包含 `cloud-images.ubuntu.com`、`packages.sury.org`、`repo.mysql.com` 的受限网络配置草稿，但草稿还未应用；onboarding 配置工具明确说明“Saving does not apply or publish”，需要你在环境设置检查并保存草稿，再发布环境。发布后用官方干净 Ubuntu 镜像复验 PostgreSQL postinst、PHP/MySQL 源及完整 Web/数据库矩阵，再完成 G3/G4 剩余项和 G5。分支操作仍按你的条件，验收全部通过后执行；当前 GitHub `main` 保持不变。

接续基线是 GitHub [`main`](https://github.com/Aoody743/Folio/tree/main)。另有一份为保留本地 WIP 而上传的[历史快照分支](https://github.com/Aoody743/Folio/tree/handoff/pre-closeout-wip-20261016)，提交 `c93eac3f3819bdcd66b30fec2a84948bc4ad3f97`。该快照基于较早的 `refine-local` 状态，供比对遗留草稿；它不是当前源码或发行基线，不要整体合并到 `main`。

## 首发范围

- **系统：** 全新 Debian 13 amd64 与 Ubuntu 24.04 amd64。ARM64 构建、其他发行版或旧系统的记录不能替代这两个目标的原生验收。
- **Web：** 首次设置时选择 Nginx + PHP 或 OpenResty + PHP；已选栈不能直接切换。两种选择都要在两个目标系统上验收。
- **数据库：** MySQL 和 PostgreSQL 各自可选，默认都不安装，可只选一种或同时选。必须验证每一种选择对应的实际安装结果。
- **核心验收：** 安装与重复安装、网站/PHP、数据库管理与备份恢复、文件和终端、网页与 MCP 共用权限/任务/审计、重启和失败恢复、升级回退、卸载后保留用户数据，以及发布所需的性能和系统变更基线。
- **暂缓范围：** Next.js/Go 托管及 Git/归档导入按 2026-10-17 决策列为可选扩展；若作为首发能力对外提供，则须先完成对应验收。MongoDB、phpMyAdmin/pgAdmin、FTP、旧版数据迁移、公网 CA/DNS、SSH 服务端配置和第三阶段 47 个插件不阻塞当前核心版。

## 上线前必须完成

### G0：冻结当前源码并建立可复现候选包

当前本地产品源码候选 `20866c12870adde1ff1d0136e1b621ee2da4ac42` 已从 GitHub `main` 派生，并完成干净构建和字节级可复现重建。待 G1–G5 通过后，再按约定先把现有远端 `main` 保存到 `legacy`，再将已验收候选晋升为 `main`；在那之前不写远端分支。

- [x] 固定本地 RC7 产品源码 commit `20866c12870adde1ff1d0136e1b621ee2da4ac42`；它是 GitHub `main` 后代，构建时工作树干净。
- [x] 执行 Go race、MySQL driver race、安装器、前端和发行包检查；2 项需要 root-owned policy 的安装器测试明确跳过。
- [x] 构建 Folio `0.1.0-rc.7` amd64 候选，SHA-256 为 `ce35662bffd2349f938cca4034cc2a9b9331ddc254e2f8cd6046394284f5b69b`；独立重建包与原包逐字节一致。

### G1：关闭 PostgreSQL 慢日志缺陷

- [ ] 在 Debian 13 与 Ubuntu 24.04 的 PostgreSQL 原生服务上验证 `statement`、`parse`、`bind`、`execute` 日志读取、数据库筛选和敏感参数脱敏。
- [ ] 验证配置应用、实际生效、读取、停用、恢复及失败回滚；不支持的日志记录不能被静默当成“没有日志”。
- [ ] 测试和实机证据都关联到 G0 固定的源码 commit 与候选包。

### G2：完成两种网站栈和数据库选择的干净安装链

- [ ] 在两个目标系统分别安装并验收 Nginx + PHP、OpenResty + PHP；确认签名来源、systemd 所有权、端口占用、重启自启和站点实际 HTTP 访问。
- [ ] 创建 PHP 站点，验证文件操作/上传、日志、站点启停、配置错误回滚、手动证书导入与绑定、本地 HTTPS。
- [ ] 对 MySQL、PostgreSQL 分别执行“未选、仅选该数据库、两者都选”的首次安装检查；确认未选的软件不会被间接安装。
- [ ] 对已选数据库验证建库、账号权限、导入/导出、备份与独立目标恢复，并检查文件属主、权限和内容完整性。

### G3：关闭网页、MCP、任务与审计的一致性矩阵

- [ ] 对核心资源和动作覆盖允许、拒绝、撤权；网页 API 与 MCP（HTTP、stdio）得到一致授权结果。
- [ ] 验证任务排队、运行、取消、失败、重启恢复和完成结果；撤权后不能继续执行未授权操作。
- [ ] 将每个断言对应到真实动作、运行身份、任务记录和审计记录；不以 mock、静态路由盘点或 UI 按钮存在代替验收。

### G4：完成系统生命周期、恢复与数据保护

- [ ] 在两个目标系统验证重启、重复安装、升级成功、升级中断/失败回退，以及卸载面板后保留站点和数据库用户数据。
- [ ] 对配置变更和备份恢复验证中断后的恢复行为、属主/权限、独立目标和已有数据完整性。
- [ ] 记录安装前后的系统改动、服务与资源占用；用固定的 2 vCPU / 2 GiB 条件记录响应时间和内存基线。若与现有预算不符，记录差距并修复或明确决定，不能静默改低标准。

### G5：发布冻结

- [ ] 确认 G1–G4 全部通过，失败项已修复并使用新候选包复验；跳过项和外部前提单独标明。
- [ ] 固定发布 commit 和候选包校验和，保留安装、升级、回退、恢复的可复现步骤与原始摘要证据。
- [ ] 更新支持矩阵、已知限制、安装/更新/恢复说明和 MCP 接入说明；文档描述必须与候选包行为一致。
- [ ] 在证据全部对应同一冻结源码快照后，才将第二阶段核心测试版标记为完成并准备发布。

## 推荐接续顺序

1. 从当前 `work` 上的 RC7 候选继续，先读本文、[本地交接](HANDOFF.md)、[第二阶段收尾说明](PHASE2-CLOSEOUT-20261015.md)、[验收运行手册](PHASE2-ACCEPTANCE-RUNBOOK.md)、[验收证据目录](../../tests/rewrite/phase2/README.md) 和 [验收矩阵](../../tests/rewrite/phase2/acceptance-matrix.json)。RC7 产品源码提交已是当前 GitHub `main` 后代；不要在所有门槛通过前移动远端分支。
2. 在官方、干净的 Debian 13 amd64 与 Ubuntu 24.04 amd64 隔离系统补齐 G1–G4；不要在日常工作主机上运行 root、systemd、防火墙或 SSH 主机操作验收。
3. 每轮修复后用新 commit/包复验对应失败项，保存精确版本、哈希、环境和结果，再更新本清单与矩阵。G1–G4 全部通过后，先将原 GitHub `main` 指针保留到 `legacy`，再把验收候选推为新 `main`。
4. 若没有目标 VM、root 权限或外部测试账户，继续可独立完成的代码和源测试，并把实机项保持为“未验收/受阻”；不得用 ARM64、mock 或旧候选包替代。

## 证据与历史记录边界

- `docs/rewrite/evidence/phase2-hosting-core-source-checks-20261016.json` 记录的是当时源码检查，不是当前 `main` 的完整发行验收。
- [RC5 验收检查点](evidence/release-gates-rc5-20261018.json) 记录的是本地 `work` 冻结提交和 Debian 13 amd64 QA 包的本轮结果；其中通过项不能替代主线整合、Ubuntu 24.04 或完整发行矩阵验收。
- [RC7 验收检查点](evidence/release-gates-rc7-20261018.json) 是当前 amd64 候选的源码、可复现构建和实际 guest 证据；明确列出 Ubuntu PostgreSQL 安装、官方镜像、PHP/MySQL 源和生命周期矩阵的限制。它通过 Git 继承当前远端 `main`，但尚未被推送或发布。
- [RC6 验收检查点](evidence/release-gates-rc6-20261018.json) 保留 RC6 的历史测试结果；RC7 已替代它作为当前候选。
- [RC4 验收检查点](evidence/release-gates-rc4-20261018.json) 是上一轮未冻结候选的历史记录。
- `docs/rewrite/evidence/phase2-closeout-dev24-20261016.json` 是 ARM64 方向的历史检查；用户随后把首发目标改为 Debian 13 / Ubuntu 24.04 amd64。
- 较早 dev.21/dev.23 的原生记录只能证明对应旧包与当时环境的结果，不能代替 G0 当前候选包。
- 当前仓库内源代码可以接续开发；云端 VM、运行数据、凭据、原始大体积日志不会随 Git 一起迁移。凭据不得加入仓库、计划或验收日志。
