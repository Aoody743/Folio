# 重构实施与验收记录

最新执行安排（2026-10-15）：用户已将本地改动更新至 GitHub，并恢复云端开发。本轮先按要求使用 GPT-6.1 Sol 完成数据库/Docker/DNS 批次；随后用户为节约额度改用 **GPT-6 Luna、Max 推理强度** 继续 PHP/Supervisor 闭环，由主 Agent 统一审查和集成。已从 `refine` 同步 `b893eef`，保留本地安装恢复与 Web 迁移实现。第二阶段继续进行，第三阶段未开始；下方旧批次保留为历史记录。

日期：2026-10-13（Asia/Singapore）。所有检查只代表记录的环境与当前代码，不代表尚未运行的发行版或第三方服务已经兼容。

## Luna 并行续开发：dev.19（2026-10-15）

用户暂缓真实服务器 SSL/SSH 验收，优先推进其他第二阶段功能。dev.19 源码提交 `04154fc31` 已冻结并构建；Go 全量 race、vet、前端 63 项测试、类型检查、静态导出、双架构构建及实际 amd64 包独立前缀安装检查通过。具体范围与可携带日志见 [dev.19 检查](evidence/phase2-dev19-checks-20261015.json) 和[源码身份](evidence/phase2-dev19-source-snapshot-20261015.json)。

本批增加 Docker 单次 exec、同 Agent 证明的镜像归档、binlog 管理及显式恢复、FTP 目标目录维护、账号启停状态、缓存集群脱敏视图和手动 HTTP-01。归档不接受无证明的任意外部镜像；关闭 exec 流不保证终止容器内命令；FTP 账号暂停尚待实际包登录验证。以上测试不替代真实服务、浏览器与 MCP 完整验收，也不表示第二阶段完成。

下一批由 Luna 并行实现交互 TTY、PHP 慢日志与站点会话维护、数据库复制及 rsync/lsyncd，对应界面同步开发；独立子代理负责真实 VM 验收，主 Agent 负责共享接线、审查和整合。第二阶段进行中，第三阶段未开始。

## 云端第二阶段接续：2026-10-15

- 本轮补齐 SQL 管理员凭据初始化、随机/自定义密码轮换与显式恢复。自定义密码先加密暂存为不可变版本，持久任务仅保存版本标识；实际解密发生在执行时授权检查后，通过受认证 Unix IPC 交给 root Agent。SQL 提交与本地凭据写入分离时保持待恢复状态，不自动重放密码 SQL。
- Core SecretPut/Get/Delete 的审计保留动作、资源与标签，但不再返回内部 `secret_ref`；新增真实审计存储回归，修复提交为 `ae1b2b582`。
- Docker 增加私仓凭据 CRUD/CAS、不可变版本、严格仓库匹配的 digest pull，以及逐镜像授权的 typed list；界面接入精确摘要选择、检查/删除和私仓操作。凭据不写入 Docker login 配置、任务或审计输出。SQL 管理员界面显示实际状态与允许的恢复动作。
- 原生 DNSPod 国际版 HTTPS 驱动补入完整分页、精确 TXT/zone/record 归属和响应丢失恢复；[协议证据](evidence/phase2-dpi-protocol-20261015.json) 同时保留另外四个 DNS provider 的具体限制，未宣称真实供应商账户或公共 CA 验收。
- 全量 Go race：13 个测试包通过，2 个包无测试；`go vet` 通过。前端 48/48、类型检查、Next.js 静态构建通过。dev.13 amd64/arm64 固定包构建及实际 amd64 包 private-prefix smoke 通过；ARM64 本轮仅构建。见 [批次检查与可携带日志](evidence/phase2-sol-20261015/checks.json) 和 [冻结源码清单](evidence/phase2-dev13-source-snapshot-20261015.json)。
- 真实 Web/PHP 跨实例迁移、安装恢复、SQL 密码轮换和新增浏览器流程继续在隔离 VM 验收，结果另列，未执行项不计通过。Ubuntu dev.13 实际安装升级已通过，API/Agent 进程校验一致且策略与配置连续；这不等于迁移流程已通过。
- 逐项缺口已另列 [第二阶段剩余工作](PHASE2-REMAINING-20261015.md)，明确区分缺实现、缺原生验收与外部条件，不按 stale tracker 状态推算完成比例。后续 Luna Max 批次已补 PHP instance error log/session 快照清理与 Supervisor 作业日志查看/双流清理，定向测试通过，统一回归与固定包检查继续进行。PHP 未配置的 slow log 和站点 pool session 管理仍列缺口；未执行原生/浏览器项不计通过。
- 实际 dev.13 固定二进制的 REST、HTTP MCP、stdio MCP 私仓凭据 CRUD/CAS/权限隔离/撤销与 SQLite 秘密检查 8 项通过，全部自建进程已清理。见 [实际协议证据](evidence/phase2-dev13-registry-live-protocol-20261015.json)；这不代表真实 Docker daemon/private registry pull 验收。

### Luna Max 后续与原生发现

PHP/Supervisor 与 Docker Hub 名称核验修复后，前端 **57/57**、类型检查及静态导出通过。dev.14 首次 ARM64 构建发现 `Stat_t.Nlink` 类型差异，已以显式 `uint64` 转换修复；dev.15 双架构构建与 amd64 实际安装包 smoke 通过，旧失败记录保留。

- dev.13 实际 Ubuntu 迁移首次去程与静态/PHP HTTP 通过；回迁暴露按字母排序重载导致同端口抢占的产品问题。失败已回滚至目标实例 revision 2，实际 HTTP 保持可用。修复后执行顺序为源→目标→PHP，回滚为目标→源→PHP；WAL 校验将旧源与 protected beforeimage 绑定，权限/锁仍保持排序。两种名称顺序、部分 reload 失败和 SIGKILL/WAL 恢复的定向 race 通过。
- dev.16 最终修复源码再次通过全量 Go race（13 包）、vet；双架构构建及真实 amd64 固定包 private-prefix smoke 通过。前端源码未变，复用 dev.15 已验证静态导出。源码身份见 [dev.16 清单](evidence/phase2-dev16-source-snapshot-20261015.json)，检查与旧失败边界见 [Luna 批次](evidence/phase2-luna-20261015/checks.json)。实际 Ubuntu 同一站点双向迁移已在 dev.16 通过：两端静态/PHP HTTP 正常，worker、PHP 与内容保持不变。见 [原生检查记录](evidence/phase2-dev13-native-20261015.json)。
- Debian 13 amd64 的真实 Redis/systemd 安装恢复 source binary 检查通过：continue 与 stop_preserve，32.37 秒；测试 marker 和临时 unit/目录清理另由 QA 核实。它是实际内核/服务的 source integration，不能等同面板安装包故障闭环。
- Debian dev.18 固定包已通过实际恢复：原 `mariadb-p2adminqa` 的 `health_failed` revision 11 由新检查确认 active/MainPID 后，显式 continue 任务成功，journal completed revision 15 且 receipt 存在；原 PID 3004 与数据目录 inode/文件数/字节数保持。真实 PostgreSQL/MariaDB 的 source integration 也已通过（3.34 秒）：自定义密码 SQL、提交前显式 apply、提交后取消、凭据保存失败与重启恢复；执行的是 dev.13 test binary，对接 dev.18 管理的实际 daemon。dev.18 实际 API 自定义密码暂存→持久任务→Unix Agent 轮换在两引擎均通过，旧版本与新版本不同、任务返回与当前状态一致且健康；持久输入只含实例/旧凭据版本/密码标识。Maria 本次密码在被检查的响应、任务行、任务日志、近期审计与 API/Agent journal 中均未出现；PG 首次 checker 误将两种标识要求相等，已纠正断言并从成功任务核实，无重复轮换，因原秘密已丢弃未做该秘密的精确值扫描。
- Supervisor 4.2.5 的独立非 root daemon 与子进程已验证两路日志 RPC 清理，子进程 PID 与运行状态保留，见 [实际协议证据](evidence/phase2-supervisor-daemon-protocol-20261015.json)。该检查不包含生产 API/Agent/浏览器全链。
- 实际 dev.16 Chromium 检查通过：读取并显示 100 行 PHP-FPM 错误日志、加载 session 统计并显示零候选清理预览；未提交清理任务，见 [浏览器证据](evidence/phase2-dev16-php-browser-20261015.json)。
- 真实 Debian MariaDB 第 9 秒才就绪，原 8 秒启动期限提前失败且留下运行进程和受保护 journal。已增加 SQL 45 秒、MongoDB 60 秒的有界等待；为 `health_failed` 增加重新核验归属、初始化凭据、当前进程与真实协议健康后的显式 continue，不重放包安装、初始化或密码 SQL。专项普通/race 测试及 dev.17 全量 race/vet、双架构构建、安装包 smoke 通过；独立审查随后发现还需确认 systemd ActiveState/MainPID 与该 PID 一致。该遗漏已补入 dev.18：固定 `/usr/bin/systemctl show` 的有界查询、严格输出解析和 MainPID 等值检查；inactive、PID 不符、缺字段/重复/截断/取消等负例拒绝且 journal 不变。独立复审无遗留发现；全量 race/vet、双架构及实际 amd64 安装包 smoke 再次通过。[最终源码清单](evidence/phase2-dev18-source-snapshot-20261015.json) 绑定产品提交 `6bc033fc4`，原实例恢复和双 SQL 引擎复验另见原生证据。
- 原生测试收尾：Debian 恢复已保存的原始 HTTPS/Signed-By 软件源，Ubuntu 原始 source 哈希保持并删除本轮 relay override/mirror；本轮 APT tunnel/relay 已停止，既有共享 relay/API forward 与受管 SQL 数据保留。临时路由、最终源/密钥哈希和扫描边界见原生证据。
- 云端普通用户预览已升级 dev.18，离线保留原数据和二进制备份，健康与重复启动通过；未在宿主安装 root Agent 或全局服务。真实软件管理继续使用隔离 VM。

## 本地第二阶段接续：2026-10-14

本批从远端 `refine` 的 `92752b9` 建立 `refine-local`；旁边旧工作区的未提交改动未修改。使用 GPT-6 Luna max 分模块实现并独立审查。第二阶段仍在进行中，第三阶段未开始。

- 安装恢复：修复 unit 原子不覆盖发布、receipt/plan 目录同步、已提交 receipt 仅完成 journal、普通管理动作与恢复共用跨进程锁、未完成安装写操作守卫，以及 systemd 实际 FragmentPath/drop-in/待 reload 归属证明。`is-active` 正常退出码 3 不再被错误包装丢失；unit 文件缺失但仍运行时拒绝卸载。早期无可靠服务证明的 checkpoint 只允许检查，不能自动重放包安装/DDL/密码初始化。
- 软件界面增加按实例 ID 检查安装恢复、显示后端可执行动作和 CAS 版本、显式提交恢复任务；重试保持幂等标识。
- 网站跨 Web 实例迁移增加 Agent 私有冻结计划、源/目标/PHP/worker/配置/TLS 证明、两端资源授权与执行期重验，复用现有 WAL 做失败和崩溃恢复。内容与 PHP 配置保留；高级配置需显式转为托管配置并保存原文。网站详情增加专门规划、复核、应用入口。
- Debian 13 **原生 arm64** 可丢弃 VM 已建立，宿主未全局安装 Go/插件或修改 SSH/防火墙。真实 Redis/systemd 源测试的 continue/stop_preserve、外部 drop-in 拒绝和 missing-unit 拒绝通过。迁移 root 文件系统 fixture 的两种真实子进程 SIGKILL 检查通过；其 Web check/reload 为协议 fixture，不能当作 OpenResty/PHP 服务验收。
- 普通 UID、umask 077 的全量 Go race 13 包通过、2 包无测试，387 项顶层测试通过、55 项环境或 helper 用例跳过；对应源码快照与逐项跳过记录见 [源码证据](evidence/phase2-local-20261014/source-checks.json)。前端 43 项源码测试、类型检查及静态导出通过。最后安全修复的插件定向 race、全量 vet 和依赖校验通过；固定包验收随后补入，未执行前不宣称可部署或阶段完成。

本机临时 VM、工具链、缓存和专用 SSH key 集中于 `.cache/rewrite`。用户要求开发结束可清理：验收后先 poweroff 并确认 launcher 停止，保留代码、脱敏证据和发行包，再清除本次临时环境。源码、真实服务、固定安装包和浏览器证据分别记录。

## 历史云端 Luna 批次（2026-10-13）

该历史批次按当时要求使用 GPT-6 Luna max。首批源码闭环已完成，以下保留当时的验证状态；当前安排与结果以上方 2026-10-15 记录为准。

- ACME 证书签发/续期及订单 confirm/cancel producer 已迁移到 `Tasks.SubmitPrepared`；order receipt 与 task、权限在同一 SQLite 事务提交。
- Core 已启用 SQLite `synchronous(FULL)`，新建 master secret 在文件和目录同步成功后才继续；post-commit enqueue 对明确 transient busy 错误做有界重试。
- Sites 已落地有界 durable WAL、身份校验及 SIGKILL 后恢复检查。
- 数据库删除 CAS/receipt code、删除状态 UI 和备份下载已完成首批闭环。
- 本批全量 Go race 有 13 个测试包通过、2 个包无测试文件；`go vet` 通过。前端 24 项测试和 typecheck 通过。SQL/Mongo 源协议检查通过；Debian 13 QA guest 的 Sites 场景由 source-built 测试二进制执行，PHP CLI 使用真实 `/usr/bin/php8.4` 并验证普通 UID/访问控制。Sites journal 未验证 dev.11 包安装或 FPM/Web 服务生命周期。汇总见 [首批检查摘要](evidence/phase2-luna-batch1-checks-20261013.json)、[Sites durable journal 记录](evidence/phase2-sites-journal-20261013.json) 与[数据库源协议记录](evidence/phase2-database-source-protocol-20261013.json)。
- dev.11 的 amd64/arm64 构建和 amd64 固定包 private-prefix smoke 已通过；实际 VM 升级及 managed-service 生命周期仍在验收。摘要中列有发行包 SHA256。不能据此宣称第二阶段完成。

### 第二阶段 batch2 状态（dev.12，2026-10-13）

dev.12 源码与固定包检查通过，但固定包的真实 VM 验收仍待 QA；第二阶段继续进行中。全量 Go race 13 个测试包通过、2 个包无测试文件，`go vet`、前端 36 项测试、TypeScript、Next.js 静态构建与双架构构建、amd64 安装包 private-prefix smoke 均通过。缓存状态对真实 Redis/Memcached 的只读检查通过；首次使用的旧认证 fixture 失败后已换成私有 fixture 重测通过。详情见 [batch2 检查摘要](evidence/phase2-luna-batch2-checks-20261013.json) 与 [dev.12 产品源码快照](evidence/phase2-dev12-product-snapshot-20261013.json)。

- Sites raw-v2 私有备份与 pending-marker 恢复、分类并发 gate 已进入本批源码；分类更新省略 `category_id` 时会继承站点现有分类并纳入任务授权与执行时复核。这会收紧兼容性：只有站点权限、没有继承分类权限的调用可能被拒绝；清空分类或迁移到其他分类仍可用于修复悬空分类。系统变更工作流与站点分类 UI 已接入。
- Cache 增加 typed status、context timeout 和 Valkey 版本来源处理；Redis、Memcached live 检查通过，Valkey 原生/主从尚未验收。Docker image 已有两项 action，registry、import/export 与 exec 仍待实现或验收。
- dev.11 真实生产 MariaDB 自启场景暴露未解决问题：任务失败，活动 unit 已 disabled，且没有 receipt。dev.12 固定源码包含后续修复，但真实 VM 复验仍待 QA；在复验前不将 SQL/MariaDB 生命周期记为关闭。
- 剩余范围包括首次安装失败恢复 journal、固定包原生升级及 SQL/Web/PHP 生命周期、root 高级配置精确恢复、实际浏览器新流程、ARM64 原生执行、其余插件动作，以及外部 DNS/CA 供应商与凭据条件。上述范围未完成前，不宣称第二阶段退出。

## 历史交接快照（保留当时记录）

截至该交接快照，用户曾要求先收尾并推送 `refine` 新分支，让本地 Codex 继续；以下内容用于保留当时状态，不代表当前执行安排。本轮交付完整源码、计划、证据摘要与 [本地交接](HANDOFF.md)、[可复制 prompt](CONTINUE-PROMPT.md)。第一阶段完成；第二阶段仍在开发，第三阶段未开始，没有全功能完成声明。

- dev.10 已封装为固定开发包：amd64 SHA256 `01a5fd1a9e8597826e775d1473be0e42d568ff12465765f08b43d56cd2481aa8`；arm64 仅构建。
- dev.10 实际文件 API 14/14、强化浏览器和新增三项编辑器回归全部通过；Ubuntu 的真实锁、端口故障、中断日志恢复、升级、回滚及重升六项通过。
- dev.10 原生 Root DNS/ACME 与网站恢复测试六组通过，但部分 Check/Reload 采用测试运行时，不代表真实 Web/PHP 生命周期。
- MariaDB banner 解析、PostgreSQL 数据目录祖先权限、托管 Web/PHP reboot 自启均保留真实失败证据；下一批源码修复已落，尚未新固定包复验。
- 当前源码新增原子任务 SubmitPrepared、SSE 订阅/认证修复、SQL/Redis 精度保留、新 DNS 驱动和网站备份下载；ACME producer 迁移原子提交、网站持久 WAL、全部核心生命周期仍待完成。
- 本轮收尾源码验证的准确结果和可携带日志见 [HANDOFF-VALIDATION](HANDOFF-VALIDATION.md)。历史工作区路径、VM/凭据/发行产物不会随 Git 克隆，重建条件见 HANDOFF。

下方保留过程记录用于追踪失败与修复；早期版本结果不代表最终交接源码的实际发行验收。

## 当前阶段

第一阶段已完成并通过八项退出标准，证据与边界见[第一阶段验收记录](PHASE1-ACCEPTANCE.md)。第二阶段正在实施真实文件、终端、网站、软件、数据库及系统管理；第三阶段尚未开始。旧实现保留作为功能与数据迁移参考，新运行时不调用旧Python安装器。

新模块：`cmd/powerlinux`、`internal/core`、`internal/httpapi`、`internal/mcpbridge`、`internal/agent`、`internal/jobs`、`internal/webassets`、`frontend`、`scripts/rewrite`与`tests/rewrite`。

## 已完成的检查

- Go正常全套与统一race测试通过：CLI、Agent、SQLite业务、HTTP、任务与官方SDK MCP；随后必要修复分别重跑对应包。
- Next.js静态导出、TypeScript、实际Astryx组件构建通过；前端API测试4项通过。五套主题、便签和导航使用真实后端接口。
- 安装器21项测试通过，覆盖校验、路径/归档边界、幂等性、升级/回滚与故障恢复。`0.1.0-dev.3`两架构实际包构建成功、校验通过，实际amd64包的初始化状态、完整Next静态资源/CSP、运行锁、退出、重复安装与保留数据卸载全部通过；真实VM另行记录，arm64仅构建。
- 官方SDK完成Streamable HTTP与stdio共享数据，以及OAuth发现、注册、cookie/CSRF授权同意、S256 PKCE、撤销与受众隔离的实际协议测试。
- 真实QEMU虚拟机已启动Debian13、Linux6.12.107、systemd257与SSH。初次安装调用超过150秒，但确认服务随后以普通用户启动并健康；尚需完成恢复与重复安装后才能称通过。无KVM，测试采用TCG，不能将其耗时当作生产性能数据。
- 实际dev.2二进制14项HTTP/MCP黑盒验收全部通过。完整测试中的20并发/200次状态请求p95为80.22ms，RSS100,724KiB（98.36MiB）；独立诊断同路径p95为46.85ms。Go测量程序计时实际HTTP请求，保留100次预热和连接复用。当前环境为云工作区CPU配额4核／32GiB，不代表2核／2GiB的标准宿主性能验收。
- 云端依赖脚本完整重跑通过（Go模块校验、npm ci、类型检查）；当前普通用户运行时启动、重复启动、停止与重启已验证。install_script/start_skill保存为环境配置草稿，尚未发布或验证新任务恢复。

## 当前门禁

- 第一阶段浏览器、最终黑盒、干净 Debian VM 安装闭环及资源/系统变更基线均已通过，最终静态产物为 dev.4；先前过程记录保留，下方早期 dev.2/dev.3 数据不替代正式验收记录。
- 第二阶段按 137 组验收矩阵检查 235 个旧路由与 23 个核心插件，尚不能称网站、数据库或所有插件可用。
- 后续继续真实升级/失败回滚、多平台、标准宿主性能和第三方集成验收。
- 每个已实现功能需填入实际用例与日志，功能声明和交叉编译不能代替运行证据。

## 第二阶段当前验证结果（2026-10-13）

- 冻结测试版本 dev.8 的全量 Go 普通/race 测试与 Next.js 生产静态构建通过；TypeScript 和 12 项前端测试通过。amd64/arm64 发行包已生成并校验，arm64 尚无实际运行证据。
- dev.7 的文件 API、HTTP/stdio MCP 与浏览器原有流程通过；强化浏览器检查发现 MCP 修改后文件大小仍显示 0B。保留失败证据，dev.8 已改用后端返回的实际字节大小，等待同一检查复验。
- 真实 Ubuntu VM 上 7 组站点、严格 HTTPS ACME 协议、手工 DNS 持久化与配置回滚检查通过；这些检查使用隔离运行时 Check/Reload 测试夹具，不代表真实 OpenResty/PHP-FPM 生命周期或正在使用的证书续期已通过。
- 真实 Debian VM 上 4 组 PHP CLI 检查通过，普通用户执行、版本及扩展隔离得到验证。真实 Ubuntu VM 上 10 组 Agent 策略/CLI 检查通过；1 组普通用户专用负例在 root VM 明确跳过，其普通用户云端检查另有通过记录。
- dev.8 实际数据库/缓存 REST、HTTP MCP 与 stdio 验收 10 组全部通过，覆盖 MariaDB、PostgreSQL、MongoDB、Redis、Memcached；安装、升级、主从与备份恢复仍需独立验收。嵌套 MySQL 驱动模块的完整 race 测试通过，含接收报文上限回归。
- dev.8 实际 Ubuntu 24.04 安装与重复安装通过：API 普通用户、root Agent 与终端独立用户得到验证，策略预检无副作用、真实 peer 校验、拒绝覆盖 vendor/FIFO 服务单元、受保护的全局文件不变、便签跨 API/Agent 重启持久化均通过。amd64 包 SHA256 为 `9a6b4f487a3f752610e93732e677eaaed46bd5ca204b3bcc04768be67db421bb`。完整生产终端和失败升级/回滚仍需后续验收。
- dev.8 文件 14 组检查中 12 组通过、2 组失败，已定位实际 SQLite 适配器缺失的 not_found 转换，开发源码已有真实集成回归，待下一固定包复验。强化浏览器已验证原大小显示、冲突/离线草稿和历史恢复；大文本 GB18030 编辑卡顿仍待修复后复验。
- 可信来源按实际系统区分：Debian 13 默认 sqv 拒绝 OpenResty 官方旧密钥的 SHA1 自签绑定；Ubuntu 24.04 的原生 gpgv/APT 在无策略修改时验证相同官方来源成功，后续可进行真实生命周期检查。Debian 当前 Lego 不满足新版适配要求，已另行构建并核对现代官方产物；DNS 颁发尚未验收。未降低验签策略。

证据位于 `tests/rewrite/phase2/` 与云端 `/workspace/powerlinux/test-artifacts/`；各版本的通过、失败和未执行项分别保留。第二阶段与第三阶段均未达到退出标准。

## 已修复的问题

- MCP SDK错误输出的typed nil会将错误详情覆盖为null；现返回一致的结构化错误并有回归断言。
- HTTPS反向代理部署需要Secure会话Cookie；公共HTTPS MCP必须实际TLS或回环代理，忽略不可信转发头。无TLS时CLI拒绝非回环监听。
- 便签、修订历史与审计使用同一SQLite事务，审计失败不会留下未经记录的写入。
- SSE撤销与退出生命周期有检查；服务关闭同时取消活动请求，避免订阅拖延退出。
- 所有任务执行器统一限4并发，等待任务保持queued；排队取消不会执行操作，队列总量有界。
- Token子链受父期限、权限、资源与受众限制，撤销级联；管理Token只能管理其委派子树。
- Go默认embed忽略下划线目录，造成`_next`脚本404；改为`all:dist`并通过实际包资源检查。
- 手动合并后的状态未清零，导致下一次编辑不再自动保存；成功保存后清理标志，失败仍保留草稿，浏览器完整回归进行中。
- 初始Python计时包含每次创建HTTPS证书上下文，不能作为HTTP延迟；已独立复现该开销并采用Go端到端计时，原20并发/200次与性能阈值保留。

## Git交付（历史记录）

开发原分支为 `rewrite/next-go-mcp`。按当时交付安排，以 `refine` 新分支交付全部源码与进度文档，并由本地 Codex 继续逐阶段实施。最终推送结果见当时的交付消息。

## 第二阶段 dev.9 验收与 dev.10 修复批次

- dev.9 全量 Go race、嵌套 MySQL 驱动 race、Next.js 生产构建通过；实际固定包文件 14 组及完整强化浏览器检查全部通过，覆盖 4,000 行 GB18030 编辑、双方版本冲突/离线恢复、5 MiB 续传下载校验及四种窗口宽度。追加的小文本编码重读与保存后残留提示检查在原 dev.9 上复现 3 项失败，已保留独立失败证据；源码修复待下一固定包复验。
- dev.10 源码新增真正的默认站点发现与双站点 CAS 切换、保留内容清除任务注册修复，并在网站与软件变更间接入同一受控运行时事务，防止并发卸载或回滚覆盖站点配置。相关 CLI/Sites/AgentConfig race 通过；真实 Debian root VM 的默认切换双修订/两种失败回滚，以及严格 umask 下备份恢复的 UID/GID/权限检查共两组通过。Check/Reload 使用测试夹具，不能替代真实 Web 服务生命周期。
- 真实 Debian VM 的两个 EXDEV 内核检查通过，验证不同设备上的移动、目录、UID/GID/权限、回收恢复、真实取消及维护锁。真实 GID0 系统账户负面检查验证终端与脚本策略/构造器均拒绝，临时账户已清理；两类证据分别见 `files-kernel-dev10-evidence.json` 与 QA 原生记录。这些是内核/服务层证据，不替代发行包 HTTP/MCP 验收。
- 实际隔离数据库权限与恢复检查通过：MongoDB BSON/索引/角色及备份删除、PostgreSQL 所有权与 SECURITY DEFINER 降权、MariaDB 自动存储过程权限撤销；运行任务的令牌撤销租约得到验证。托管 SQL daemon 生命周期、工具备份恢复、复制/binlog/SSH 同步、旧权限档位与缓存管理仍待完成。
- 实际 Ubuntu 上 dev.8 → dev.9 升级通过，原策略与数据保留。通过真实面板动作安装 OpenResty/PHP、建站、访问与备份恢复已推进；发现恢复权限受 umask 影响，源码已补原模式与隔离所有权保持，完整软件生命周期仍在验收。QEMU 出站网络需临时受控仓库镜像：上游正常 HTTPS 验证、原始签名与包校验保持，测试前置条件与清理单独记录。
- 可复用云端运行时随后已升级到校验后的 dev.10，启动/重复启动、健康/session 及 12 个脚本资源通过。环境配置的 install_script/start_skill 已更新保存为草稿 `cecfgdraft_6acdfa946220819a83ca2d31845e0087`，尚未发布或证明新任务恢复。
- 原生 Knot TSIG 与 ISPConfig HTTPS 驱动已通过本地真实协议及持久归属/响应丢失检查；dev.10 完整原生订单颁发、下载中断/重启恢复与清理已有隔离协议证据。下一批 Zilore/Online 驱动已接入，其余五个专属 DNS 适配器仍未完成。外部公共 DNS/真实供应商账户与 CA 仍未验证，不能由本地协议测试替代。

dev.10 固定包已完成部分实际验收，当前交接源码包含其后的修复；新源码的实际服务复验仍待继续。第二阶段尚未退出，第三阶段仍未开始，所有未完成范围保留。
