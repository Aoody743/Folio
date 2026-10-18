# 本地 Codex 开发交接

2026-10-19 接续检查点：当前冻结候选 RC15，官方 Ubuntu 原生栈及升级故障注入已有新证据；Debian 镜像出口、同候选双系统矩阵和性能目标仍阻塞，`stage_2_complete=false`，没有写远端。以[当前验收结论](LAUNCH-READINESS-20261018.md)和 [RC15 证据](evidence/release-gates-rc15-20261019.json)为准；下文历史状态不覆盖该结论。

更新：2026-10-18。产品与用户界面统一使用 Folio 品牌；旧版代码和系统兼容标识仍保留，避免破坏已有路径与升级识别。当前范围、首发差距和执行顺序以[上线前差距清单](LAUNCH-READINESS-20261018.md)为准；本文件中的早期要求如与它或[第二阶段收尾说明](PHASE2-CLOSEOUT-20261015.md)冲突，按后两者的当前范围执行。未执行的环境验收不得写成通过。

仓库：https://github.com/Aoody743/Folio

分支：https://github.com/Aoody743/Folio/tree/main

## 从这里开始

1. [上线前差距清单](LAUNCH-READINESS-20261018.md)：当前首发范围、阻塞项和交付顺序。
2. 本文件与 [接续 prompt](CONTINUE-PROMPT.md)：仓库、本地重建条件与接续背景。
3. [三阶段计划](DEVELOPMENT-PLAN.md) 与 [范围补充](SCOPE-CORRECTIONS.md)：历史目标、依赖和迁移索引；当前首发以差距清单为准。
4. [进度记录](PROGRESS.md)、[第二阶段收尾](PHASE2-CLOSEOUT-20261015.md) 与 [第一阶段验收](PHASE1-ACCEPTANCE.md)：实际通过、失败和未执行项。
5. [第二阶段验收运行手册](PHASE2-ACCEPTANCE-RUNBOOK.md)、[验收矩阵](../../tests/rewrite/phase2/acceptance-matrix.json)、[迁移台账](migration-tracker.json)：逐项续做。历史矩阵状态不能代替新候选包证据或计算真实完成百分比。
6. [技术栈](TECH-STACK.md)、[UI](UI-SPEC.md)、[MCP](AI-NATIVE-MCP.md)、[实现契约](IMPLEMENTATION-CONTRACT.md)、[第二阶段契约](PHASE2-CONTRACT.md)、[审计](AUDIT.md)。

## 固定目标

- Next.js 16.4、React 19、TypeScript、Meta Astryx 0.6.8；实际设计系统：https://astryx.atmeta.com/ 。以 lockfile 为准。
- Go 后端、SQLite、独立 root host executor；Next 静态产物嵌入 Go。host executor 只负责受控主机操作，不是内置 AI。用户 Linux 安装后无需常驻 Node、Python 或模型进程。
- 轻量、快速响应、低侵入；精简四卡首页、五项底部导航、五套配色，支持移动端。
- 共享便签由人和外部 AI 修改；原生 MCP 支持 Streamable HTTP 与 stdio，共用真实动作、权限、任务、审计。
- 面板不内置聊天或模型选择界面。Ollama 仅作为旧有受管软件按需迁移。
- 当前核心首发范围、明确暂缓项和退出条件见[上线前差距清单](LAUNCH-READINESS-20261018.md)及[第二阶段收尾说明](PHASE2-CLOSEOUT-20261015.md)。旧功能与 70 个插件清单保留作历史迁移索引；第三阶段 47 个插件和旧版数据迁移不阻塞本轮核心版。

## 当前状态

| 阶段 | 状态 | 说明 |
| --- | --- | --- |
| 第一阶段 | 已完成并验收 | 基础 UI、身份/权限、便签、任务、MCP、发行基础与真实 Debian 安装；正式退出记录见 PHASE1-ACCEPTANCE |
| 第二阶段 | 开发中，尚未退出 | 文件/终端及部分真实数据库、网站与软件操作已验证；生命周期、故障恢复和其余核心能力仍需完成 |
| 第三阶段 | 尚未开始 | 47 插件、完整迁移、多平台、性能/长期运行、正式发行 |

新版位于 `cmd/powerlinux`、`internal/`、`frontend/`、`scripts/rewrite/`、`tests/rewrite/`、`third_party/mysql/`。旧 `web/`、`plugins/`、Python 安装器保留供功能参考；新版不得回调旧安装器实现管理操作。

### 已验证的固定包与当前源码不同

最近完成实际运行验收的是云端开发包 `0.1.0-dev.10`：

- Linux amd64 tar SHA256：`01a5fd1a9e8597826e775d1473be0e42d568ff12465765f08b43d56cd2481aa8`。
- 二进制 SHA256：`938917a7fa9124e1d41aecfafbf304a634f92f2c908f6bd9924f5ca9b49e09cb`。
- Linux arm64 tar SHA256：`16c9315f6c3aa0d3329c59220f86b021a836aec198dbed7e78b79690e7e775db`，仅构建，未经实际 arm64 运行验收。
- 全源码 manifest JSON SHA256：`94f99ba2c4c26a34f173ab116a34b17fd3f2199d35b5aba9e4d0b9337bc1a46f`；其内 canonical files-map SHA256 为 `b598f150b6726e9513b17130893a9c2310c7cabe2d7b7f254519deff77458091`，两者不是同一个摘要。

dev.10 的源码快照、验收摘要位于 [交接证据目录](evidence/handoff-20261013/) 与 `tests/rewrite/phase2/*-evidence.json`。发行 tar、VM 磁盘、私有凭据和原始大型日志没有推入 Git，也未发布 GitHub Release；仅凭本分支当前源码不能重建相同 dev.10 摘要。后续新改动不得覆盖同名固定包或把其证据标成已复验。

dev.10 的实际结果：文件 API 14/14、完整强化浏览器与追加编辑器回归通过；Ubuntu 24.04 安装升级六项锁/失败/中断恢复/回滚检查通过。四项原生 DNS/ACME 与两项网站恢复内核检查通过，测试运行时 Check/Reload 夹具不代表真实 OpenResty/PHP 全生命周期。较早 dev.8 的数据库/缓存 10 组与实际终端 3 组证据分别保留。

dev.10 实际失败仍需新包复验：MariaDB 工具 banner 格式解析、PostgreSQL 私有数据目录祖先权限，以及托管 Web/PHP 安装后未启用开机自启。此分支包含下一批修复，但没有完整 dev.11 发行包及上述真实生命周期复验；不得称这些运行问题已经验收解决。

### 当前源码新增但尚未完成发行验收

- MariaDB 版本格式修复；SQL 大整数使用 `{ "encoding": "integer", "value": "十进制" }` 保留精度，Redis cursor 使用十进制字符串并兼容安全整数输入；前端已有对应适配。
- `internal/jobs/prepare.go` 的容量预留/提交后启动与 `internal/core/prepared_tasks.go` 的 SQLite 原子任务/授权/provider metadata 提交，含真实提交和故障测试。**ACME producer 尚未改用 SubmitPrepared**。
- SSE 先订阅后发送 connected，独立短周期认证租约与心跳，避免注册竞态与闲置撤销延迟。
- 托管软件开机自启动作及 PostgreSQL 目录修复；`RecoverRuntime` 辅助方法已经存在，完整网站 WAL 尚未集成。
- 原生 Zilore/Online 专属 DNS 驱动接入，固定官方 HTTPS 地址默认许可、显式策略空条目仍拒绝；Knot/ISPConfig 已有原生订单与归属恢复。其余五个专属适配器仍未完成，全部外部供应商真实账户颁发未验收。
- 网站备份 HTTP/Agent 二进制流、HEAD/Range、MCP 有界 chunk 与双 scope 已接入；新实现的验证结果看交接验证记录，不可沿用原 metadata-only 备份证据。

网站备份网页下载按钮尚未接入；新增 root FD 与真实 Unix→HTTP/MCP 测试在本轮普通用户检查中明确跳过，待独立 VM 执行。

## 建议接续顺序

1. 在 Linux 开发环境复现本分支检查，记录准确 commit、源码摘要、实际包版本。核对 [最终验证记录](HANDOFF-VALIDATION.md)，不要用云端缓存通过代替本地重建。
2. 完成 ACME producer 使用 `SubmitPrepared`：任务、credential 版本 pin、订单状态 CAS、授权同一 SQLite 事务；只在 COMMIT 后启动。提交取消/重启/未知提交结果均不能重放远端操作。
3. 实现网站持久事务日志和启动恢复：`transactions/` 目前存在但完整 WAL 未接入。覆盖配置、证书/元数据、默认双站点、创建/删除、恢复目录 swap；保护路径从 root 软件 receipt 重推，写前 fsync、明确 commit marker、恢复不启动已停服务、不还原新版 root ledger。现有 defer 回滚仅处理进程内失败，不能证明 SIGKILL 一致性。
4. 构建新的固定 dev.11 或后续版本，在独立 Debian/Ubuntu VM 复验 MariaDB/PostgreSQL 安装、工具备份恢复、managed Web/PHP 真正 reboot 自启、CSS 恢复权限、默认/代理/TLS、更新/停启/卸载保留数据/重装。保持原失败记录；新的通过应引用新版本与源码。
5. 完成剩余第二阶段能力：Web 实例显式迁移与旧/新资源授权；advanced 原配置备份恢复；数据库 root 轮换/外部导入/同名重建 CAS/复制与日志/SSH 同步；缓存特有管理；Docker/FTP/rsync/脚本/计划任务/防火墙/SSH/NTP 等按原矩阵核对。系统操作 VM harness 已准备，不能把 harness 文件当作实际验收。
6. 将动作/UI/权限/测试/证据映射回 137 组矩阵与 235 路由/70 插件台账；所有第二阶段退出标准通过后进入第三阶段。
7. 第三阶段完成其余 47 插件、旧数据干跑/导入/切换/恢复、多平台/架构真实检查、性能/长期运行及正式发布。当前主线为 `main`，提交和发布按用户明确指示进行。

## 本地环境重建

使用 Linux 工作区（或本地 Linux VM）。核心包含 Linux 系统调用与 systemd/权限测试，不保证 macOS/Windows 原生运行。需要 Go **1.27.2**、Node **24**、npm、Python 3（仅开发/测试）、C 编译器（Go race）、bash、flock、tar/gzip；浏览器检查另需 Chromium/Playwright，原生服务检查另需独立 VM/root。

不要复用云端 `/workspace` 路径。工具版本与 npm 锁文件应严格验证，不要为了快速通过悄悄降级 Go/Next/Astryx，或关闭 TLS/签名/校验。

```bash
git clone --branch main https://github.com/Aoody743/Folio.git
cd Folio
export GO_BIN=/absolute/path/to/go-1.27.2/bin/go
export GOTOOLCHAIN=local
export GOMAXPROCS=2
make rewrite-deps
make rewrite-test
make rewrite-build REWRITE_VERSION=0.1.0-local.1 REWRITE_BUILD_FLAGS=--skip-install
make rewrite-dev
```

已有 checkout 时先检查本地未提交修改，再 fetch/switch；不 reset/覆盖用户文件。`make rewrite-deps` 无需 root。`make rewrite-build` 负责刷新 `internal/webassets/dist` 嵌入资产；直接 `go build` 在干净 clone 上可能只嵌入占位 README，不能证明 UI 可用。`make rewrite-dev` 需要先构建静态导出。bootstrap 凭据只能从本地受保护数据目录获取，不进 Git/日志/聊天。

`scripts/rewrite/setup-cloud.sh` 是 amd64 云端工具链便利脚本，默认 `/workspace/.tools`；本地优先使用以上命令和自身可信工具链，不把云路径当成必需条件。

### 不会随 Git 迁移的内容

- 云端 runtime data/SQLite、bootstrap token、VM 的 SSH host/client keys、测试账户、DNS/API 凭据。
- QEMU 磁盘、安装好的数据库/Web/PHP 服务、受控 HTTPS 仓库 relay 与 SSH tunnel。
- `/workspace/powerlinux/test-artifacts`、`/workspace/tools` 的原始日志与 binaries；Git 只含可公开摘要与精选可携带证据。云路径表示当时的来源，不能在新机器当成存在的文件。
- cloud onboarding 的 install_script/start_skill 已保存为草稿，未发布/未验证新任务恢复；本地无需该平台配置。使用过 [cloud-environment-onboarding:setup](skill://plugin_connector_1p_ed5feb9070a08191b08c81c47947bc16/setup/SKILL.md) 指导云端设置。

VM launcher 默认路径指向原云端；先按脚本配置选项重新准备镜像、qemu/BIOS/SSH 和私有 fixture，再运行。systemd、SSH、防火墙与软件安装只能在明确隔离的测试宿主上执行，不能误用本地工作主机。云端 TCG 的耗时不是生产性能证据；正式性能标准仍为计划中的 2 vCPU/2 GiB 主机。

无第三方账户/域名时继续独立开发并记录外部验证待办，不提交虚构的供应商通过记录；凭据通过环境安全配置提供。原 EOL 版本和平台支持需要明确来源/迁移决定，不能靠禁用签名接受旧源。
