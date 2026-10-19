# 第二阶段核心版收尾

2026-10-19 接续检查点：当前冻结候选 RC15，官方 Ubuntu 原生栈、浏览器/MCP/任务、完整重启及卸载重装已有直接证据；Debian 镜像出口、同候选双系统矩阵和性能目标仍阻塞，`stage_2_complete=false`。RC15 已上传到独立交接分支 [`rc/0.1.0-rc15-handoff`](https://github.com/Aoody743/Folio/tree/rc/0.1.0-rc15-handoff)；`main` 与 `legacy` 未修改。以[当前验收结论](LAUNCH-READINESS-20261018.md)和 [RC15 证据](evidence/release-gates-rc15-20261019.json)为准；下文历史状态不覆盖该结论。

2026-10-15，用户允许不迁移无用软件，要求准备收尾。本文件优先于旧计划中必须迁移全部 23 个核心插件的假设。2026-10-16 用户先将目标收敛到 ARM64，随后又批准 Nginx + PHP / OpenResty + PHP 首次启动二选一、Next.js/Go 应用托管，并将目标改为全新 Debian 13 与 Ubuntu 24.04 amd64 机器；下面较早的 ARM64 记录保留为历史证据，不代表当前产品目标。Nginx 使用 Debian/Ubuntu 签名仓库原生包；不以机器上已有 Nginx 为前置条件。2026-10-17 起进入新版首发验收准备，第二阶段仍未完成，第三阶段尚未开始实现。

当前范围约束：Web 栈仅首次安装时选择；选定后不提供直接切换，需先移除网站配置、卸载当前栈，再安装另一栈，网站内容默认保留。首次设置继续使用简单的可视化选择界面，复杂能力通过扩展逐步加入，不要求首页承载全部功能。Next.js 与 Go 的核心生命周期先行。网站栈完成后，用户可分别选择是否安装 MySQL、PostgreSQL（默认均不安装，也可同时安装）。旧版数据迁移不做；扩展支持是产品方向，不作为当前首发阻塞，但每个扩展必须有独立权限、任务、审计和验收证据。MongoDB、phpMyAdmin、pgAdmin 与 FTP 不计入本轮核心验收；FTP 停止新增开发，已有代码和本地改动保留。

## 当前进度

### 历史基线收尾记录（ARM64 方向，早于当前产品方向）

- 当前分支已合并 `origin/refine` 的 dev.23/收尾文档基线：上游 `2a1e945b0087a9f3768d3637fabae889f56c994c`，合并提交 `72273255a3a41bc0a42900d04f2d9c2014b1a0ee`。原本地提交保留在第一父提交 `f613aa411cfd84119c4da7c797ceb0ba05f92833` 及其祖先；本轮工作区改动仍保留，另有原始快照 `stash@{0}`。
- PostgreSQL 慢日志读取现支持 `statement` 与扩展协议 `parse`、`bind`、`execute` 记录，测试覆盖数据库筛选、无匹配记录、参数/字面量脱敏及文件权限。修复了命令行测试读取 QA 主机系统配置的环境耦合，以及本地数据库导入 SQL 校验对多行 `VALUES` 逗号的误判；FTP 凭据拒绝控制字符的单元检查也已补齐。MongoDB、FTP 远程备份及其他暂缓功能没有计入首发验收。
- 检查通过：Debian QA Linux 全量 race（14 个测试包，2 个无测试文件）、`go vet ./...`、安装器 31 项测试与另外 2 项 root 策略来源测试、嵌套 MySQL race、前端 82 项测试、`make rewrite-ui`、双架构 `make rewrite-build REWRITE_VERSION=0.1.0-dev.24`。证据与日志见 [本轮检查记录](evidence/phase2-closeout-dev24-20261016.json) 及 [检查日志目录](evidence/phase2-closeout-20261016/)。
- 已生成 Linux amd64/arm64 `0.1.0-dev.24` 候选包，SHA-256 记录在 [候选包校验清单](../../dist/rewrite-dev24-candidate-20261016/CHECKSUMS.txt)。该构建元数据为 `working_tree_dirty=true`，只作为本地候选构建，不是冻结或已验收发行版。
- 当前一次性 QA 环境是 Debian 13 ARM64（2 vCPU、2 GiB）。ARM64 源码 race 和双架构构建通过，但本轮未安装并原生运行 dev.24 ARM64 候选包；来宾 APT DNS 不可用，因此没有安装 PHP/OpenResty、Docker daemon、MariaDB/PostgreSQL 等服务。网页与 MCP 权限/任务闭环、独立目标备份恢复、重启/升级/回退/卸载保留数据、性能与内存基线仍未在 ARM64 实测。amd64 与 Ubuntu 不属于本轮验收目标。
- 概览页已标出首发“可用 / 暂缓 / 不可用”范围，并明确范围标签不代表已验收。第二阶段核心测试版仍未完成，`stage_2_complete=false`；没有把任何未实测的 ARM64 服务链路改标为 `verified`。第三阶段未开始。

### 2026-10-16 Nginx 与应用托管开发增量

- 首次启动现在提供 Nginx + PHP 与 OpenResty + PHP 两套选择；后端在计划与执行时都锁定 Web 栈，并拒绝另一栈与仍安装的当前栈并存。Nginx 由签名仓库安装；隔离环境使用面板受管配置和 systemd unit，安装后禁用发行包默认 `nginx.service`，避免两个 unit 争抢监听端口。
- Nginx 主配置沿用系统包的动态模块与 MIME 文件路径，Web 临时文件目录置于受管运行目录；新增 Next.js/Go 模板、隔离系统用户、受控构建、systemd 生命周期、日志及网站反代绑定入口。
- Next.js / Go 已增加 tar.gz 归档和 Git 仓库导入入口。Git 支持公开 HTTPS，以及经 SHA256 主机指纹校验的 SSH 仓库；导入在应用独立系统用户下运行，并需要在软件目录安装 Git/OpenSSH。私有仓库凭据、原生运行链路和权限矩阵仍未验收。
- 应用停用默认保留源码和数据；新增独立永久清理任务，要求输入 `DELETE <应用编号>`，确认 systemd 服务已停用后才删除隔离账户和应用目录。中断清理保留 `purging` 状态供管理员重试；若旧 UID 已分配给其他系统用户则拒绝继续清理。该实现尚未验证。
- 当前改动仍未在本轮重新检查；这不代表 Debian/Ubuntu amd64 的原生包安装、服务启动、实际访问、升级回退或权限矩阵已通过。受管证书导入/绑定与公网 CA 签发分别记录；公网 SSH/SSL 操作、MongoDB、phpMyAdmin/pgAdmin 仍不在首发范围。

### 2026-10-16 当前开发验证增量

- 首次设置界面增加 MySQL、PostgreSQL 独立复选项：均默认不安装，可选其一、同时安装或跳过；被选数据库按顺序提交，避免同时争用包管理器。此处为当前工作区源码状态，尚未单独构建或在 Debian/Ubuntu 原生环境验收。
- 本机为 macOS arm64。`GOOS=linux GOARCH=amd64 go vet ./...`、Linux amd64 的 `internal/plugins` 与 `cmd/powerlinux` 测试编译、Linux arm64 的 `internal/plugins` 测试编译通过；这些是交叉静态检查，不是 Linux 上执行测试或原生服务验收。
- 前端 `npm test --prefix frontend` 为 82/82 通过，`make rewrite-ui` 的 TypeScript 检查与 Next.js 静态导出通过；`go -C third_party/mysql test -mod=readonly -race -count=1 ./...` 通过。Linux amd64/arm64 当前源码二进制可构建，但不是安装包；SHA-256 见[当前源码检查证据](evidence/phase2-hosting-core-source-checks-20261016.json)。
- `make rewrite-test` 未通过：macOS 缺少安装器要求的 `flock`，且数据锁测试依赖 Linux 锁语义；原生 `go test ./internal/plugins` 也因仓库 Linux 专用 syscall/build-tag 文件不能在 Darwin 编译。因此本机没有完成全量 Go 执行测试或发行包构建。
- Debian 13/Ubuntu 24.04 amd64 原生 QA 环境本轮不可用。Nginx 原生包安装、PHP 网站与 Next.js/Go 实际构建访问、服务/权限/任务/审计矩阵、升级回退与数据保留仍未验收；候选功能不可标记为 `verified`。当前范围、可用/暂缓/不可用标签均不代表已通过实机验证。

### 2026-10-17 验收准备与收尾决策

- GitHub 主仓库已更名为 `Aoody743/Folio`，默认分支为 `main`；新版品牌为 Folio / 折纸面板，旧版 PowerLinux 兼容标识保留。
- 首发验收按 Debian 13 amd64 与 Ubuntu 24.04 amd64 执行；ARM64 仅作构建与后续扩展目标，不替代 amd64 原生证据。
- 首轮验收优先覆盖：安装器、Nginx/OpenResty + PHP、MySQL、PostgreSQL、网站访问、备份恢复、网页/MCP 权限、任务、审计、重启、重复安装、升级、回退和卸载保留数据。
- Next.js/Go 托管、Git/归档导入、数据库外部导入等能力保留为可选扩展；可以在简单 UI 中按需进入，不因扩展尚未完成而阻塞核心版，但未验收前不得标记为 `verified`。
- 验收期间发现的缺陷直接进入修复批次；修复必须补充对应测试或原生证据，再更新本文件状态。

- `refine` 已推送 dev.23，产品源码 `3e74660d6fb4da2c5300619e8dd0c3cada7a2992`，见 [检查记录](evidence/phase2-dev23-checks-20261015.json)。
- 第一阶段已验收；文件/终端、网站/PHP、数据库/缓存、Docker、系统管理、任务、审计及原生 MCP 已有实际实现。不同服务/平台的完整验收尚未结束。
- dev.23 全量 Go race（14 个测试包）、vet、26 项 Debian root 源码 fixture、双架构构建和实际 amd64 安装包检查通过。前端未变化，复用 dev.22 的 80 项测试、类型检查与静态导出结果。
- Debian 实际从 dev.21 升级至 dev.23，既有 PostgreSQL pending 慢日志事务已由产品 MCP 首次成功恢复，恢复时配置匹配原始字节。新配置实际生效与停用通过；读取发现扩展协议日志格式未被解析，尚待修复。QA HBA 已阻断，测试库/账号已通过产品任务删除，令牌撤销、活跃任务为零；见 [实际原生证据](evidence/phase2-native-dev23-20261015.json)。
- 普通云端预览已更新 dev.23，并保留数据/二进制备份。Ubuntu QA 保持 dev.21；ARM64 仅构建，未原生运行。真实宝塔服务器未修改。

旧台账包含待暂缓软件和历史状态，没有可审计的统一完成率。冻结首发清单后按退出检查计算，不按版本号或插件数量估算百分比。

## 建议保留

| 范围 | 核心目标 |
| --- | --- |
| 网站/PHP | 首次选择 Nginx + PHP 或 OpenResty + PHP；建站、文件、日志、证书绑定、备份恢复 |
| 应用托管 | Next.js、Go 模板创建与受管服务；Git/归档导入源码已接入，仍须原生与权限验收 |
| 数据/缓存 | MySQL 与 PostgreSQL 分别按需安装；建库、账号权限、导入、备份恢复及内置查询 |
| 部署/运维 | Docker、Supervisor、文件管理、终端、本地备份、计划任务、系统设置、安全与审计 |
| 交互/AI | 简洁卡片、底部导航、多配色、共享便签；网页/MCP 同权限、同任务、同结果，无内置 AI 聊天 |

当前首发原生验收目标为 Debian 13 与 Ubuntu 24.04 amd64。ARM64 构建可作为开发检查，不代替这两个目标系统的原生验收；其他发行版、历史版本与 ARM64 原生运行仍标记未验收。

## 建议暂缓

- phpMyAdmin、pgAdmin：先用面板内置数据库管理与查询。
- MongoDB、Valkey、Memcached：先聚焦 SQL 与 Redis。
- FTP 不再开发；既有实现与改动保留但不计入当前首发。rsync/lsyncd 与 SSH 全量同步先用文件传输和本地备份恢复。
- PHP yum/源码及历史 ABI、重复数据库发行来源、稀有 DNS 提供商。
- 主从/集群配置、PostgreSQL 远程受管 TLS/新监听器等高级能力。

暂缓只收敛首发范围，不删除已发布数据、不替用户卸载服务。已有实现与草稿不自动视为已验收，面板入口应标清首发可用/暂缓/不可用。

## 必须关闭的四组收尾工作

1. **真实缺陷闭环**：关闭 PostgreSQL 慢日志扩展协议格式读取缺口（不能把解析不支持当作无日志），并在 Debian 13、Ubuntu 24.04 amd64 原生服务检查配置生效、恢复、停用及读取。
2. **完整用户链**：在两个目标系统安装候选包，走通 Nginx 与 OpenResty 选择、PHP 建站/数据库、Next.js/Go 创建构建和实际访问、上传、受管证书导入/绑定、备份及独立目标恢复，并验证失败回滚。
3. **核心实机与权限矩阵**：验证 MySQL 和 PostgreSQL 的独立可选安装（未选不安装、可同时选择）及建库/权限/导入/备份恢复；完成网站 PHP、Next.js/Go、面板内置 SSH、网页/MCP 同权限断言（允许/拒绝/撤权/任务/审计）；验证重启、重复安装、升级、回退及卸载保留数据。
4. **发布冻结**：基于选定且可复现的源码快照重新构建 Debian 13/Ubuntu 24.04 amd64 包；记录目标设备上的响应时间、内存和系统修改基线，提供固定校验和、安装/升级/回退说明与验收结果，再标记第二阶段核心测试版完成。ARM64 构建不替代原生验收。

公网 CA/DNS、SSH 服务端配置管理、MongoDB、phpMyAdmin/pgAdmin、旧版数据迁移和 47 个第三阶段插件不计入本轮核心测试版完成条件。面板内置 SSH 客户端仍须完成真实目标连接与权限验收。可先验受管证书导入/绑定与本地 HTTPS 配置，不能借此宣称公网签发已通过。

## 保留但暂停的云端草稿

| 草稿 | 状态/位置 |
| --- | --- |
| rsync/lsyncd | 后端/UI 未集成；最后已知撤权/清理问题定向回归通过，UI 87 项测试/类型检查通过，未最终构建及原生验收；`/workspace/powerlinux/dev-lanes/rsync-sync/checkout`、`/workspace/powerlinux/dev-lanes/rsync-ui` |
| phpMyAdmin | 安装/状态草稿已测，缺移除/恢复和原生验收；`/workspace/powerlinux/dev-lanes/admin-apps` |
| PostgreSQL TLS | 仅证书私有桥/锁/索引基础，缺完整业务链；`/workspace/powerlinux/dev-lanes/postgresql-remote-tls` |

这些草稿未作为发行源码推送，不属于 dev.23 完成项。恢复开发时重新核对基线、依赖版本和测试。
