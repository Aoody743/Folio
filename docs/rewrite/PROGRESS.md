# 重构实施与验收记录

日期：2026-10-13（Asia/Singapore）。所有检查只代表记录的环境与当前代码，不代表尚未运行的发行版或第三方服务已经兼容。

## 当前执行状态（优先于历史交接与下方旧批次）

用户已恢复云端开发，要求使用 GPT-6 Luna max 按小批次继续完成第二阶段。首批源码闭环已完成，但第二阶段仍未达到退出标准，第三阶段未开始。

- ACME 证书签发/续期及订单 confirm/cancel producer 已迁移到 `Tasks.SubmitPrepared`；order receipt 与 task、权限在同一 SQLite 事务提交。
- Core 已启用 SQLite `synchronous(FULL)`，新建 master secret 在文件和目录同步成功后才继续；post-commit enqueue 对明确 transient busy 错误做有界重试。
- Sites 已落地有界 durable WAL、身份校验及 SIGKILL 后恢复检查。
- 数据库删除 CAS/receipt code、删除状态 UI 和备份下载已完成首批闭环。
- 本批全量 Go race 有 13 个测试包通过、2 个包无测试文件；`go vet` 通过。前端 24 项测试和 typecheck 通过。SQL/Mongo 源协议检查通过；Debian 13 QA guest 的 Sites 场景由 source-built 测试二进制执行，PHP CLI 使用真实 `/usr/bin/php8.4` 并验证普通 UID/访问控制。Sites journal 未验证 dev.11 包安装或 FPM/Web 服务生命周期。汇总见 [首批检查摘要](evidence/phase2-luna-batch1-checks-20261013.json)、[Sites durable journal 记录](evidence/phase2-sites-journal-20261013.json) 与[数据库源协议记录](evidence/phase2-database-source-protocol-20261013.json)。
- dev.11 的 amd64/arm64 构建和 amd64 固定包 private-prefix smoke 已通过；实际 VM 升级及 managed-service 生命周期仍在验收。摘要中列有发行包 SHA256。不能据此宣称第二阶段完成。

剩余工作集中在 dev.11 的真实 VM 升级/服务生命周期、第二阶段余下插件动作与验收矩阵、真实 DNS/CA 条件、ARM64 原生执行及核心管理闭环的整体验收。

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
