# 第二阶段剩余工作盘点（2026-10-15）

最新范围调整：用户允许暂缓无用软件，优先以 [核心版收尾清单](PHASE2-CLOSEOUT-20261015.md) 决定首发范围；下方完整旧功能盘点作为参考，不再将全部可选插件强制算作当前退出阻塞。

初版为只读源码盘点；随后按已冻结的 Luna Max 批次补入 PHP runtime 日志/session 与 Supervisor job 日志实现。包含本轮新增的 DB administrator、Docker registry、DNSPod International 改动。依据 [DEVELOPMENT-PLAN](DEVELOPMENT-PLAN.md) 的 P2-01～09 和七项退出标准，交叉读取 [feature-inventory](feature-inventory.json)、[migration-tracker](migration-tracker.json)、实际 Go bindings/执行器与 `plugins/<id>/index.py`。盘点本身不运行构建、测试或 guest 操作；验证结论只引用对应批次证据。

`planned`、旧动作字符串数、catalog capability 声明均不是剩余功能数量；不据此计算百分比。下表的“已有”指具体源码能力，不表示整插件验收完成。错误返回、策略限制与缺功能分别保留，不能用通用 SQL、终端或可编辑配置替代专用用户管理闭环。

## dev.23 增量（优先于下方历史盘点）

[dev.23 检查](evidence/phase2-dev23-checks-20261015.json)绑定源码 `3e74660d6`，修复 PostgreSQL 慢日志重载顺序、原未配置状态恢复及旧 pending 记录兼容；后端 race/vet、26 项 Debian root 源码 fixture、双架构构建和实际 amd64 安装包检查通过。实际升级/旧记录恢复已通过；新配置生效/停用通过，扩展协议慢日志读取待修。新安装/重装入口也已接入统一 journal 保护。rsync 撤权/恢复/界面、管理应用与 PG 远程 TLS 尚未集成，第二阶段未完成。

## dev.22 增量（优先于下方历史盘点）

[dev.22 检查](evidence/phase2-dev22-checks-20261015.json)绑定源码 `362d2d63b`：表维护后端与界面、复制界面、PHP 原计划恢复发现/继续和 root 重启修复已实现，后端 race/vet、前端 80 项测试及双架构构建/实际 amd64 安装包检查通过。Debian root 源码 fixture 15 项通过；实际数据库维护、复制、PHP 扩展安装及浏览器/MCP 完整矩阵仍待验收，不能将其标为已完成。

rsync/lsyncd 后端与管理应用尚未集成；同步目录策略撤销时必须停止旧授权服务，不能仅拒绝新任务。PG 远程 TLS、其他引擎复制/SSH 同步、明确不可用 DNS 供应商、FTP 只读和跨插件原生完整链路仍是余项。第二阶段未完成，第三阶段未开始。

## dev.21 增量（优先于下方历史盘点）

[dev.21 检查](evidence/phase2-dev21-checks-20261015.json) 绑定源码 `ad531c410` 与实际包校验和。PHP 官方 APT 模块安装/实例停用、PostgreSQL loopback SCRAM 与慢日志、同主机 MySQL/MariaDB 复制、旧 FTP 记录恢复已进入已实现/待原生验证范围；复制 UI 正在下一批制作。Supervisor 源码已修复真实 VM 揭示的 programs 祖先权限和无效 clear 被误报成功问题，[dev.21 原生证据](evidence/phase2-native-dev21-20261015.json)中实际 Supervisor API/MCP 子进程闭环已通过，浏览器未测；另发现 PHP 待恢复 journal 阻断 root Agent 重启，dev.22 正优先修复，不能据普通 UID 测试宣称通过。

仍有明确功能限制：PHP source/yum 模块安装及系统包物理移除；PostgreSQL 远程受管 TLS；其他引擎复制与 SSH 同步；FTP 账号只读；rsync/lsyncd；数据库表维护完整界面；phpMyAdmin/pgAdmin 的可信实际部署与专用管理；四个 DNS provider、跨阶段 webhook 依赖及下表其他未关闭项。真实发行版/重启恢复/升级卸载/浏览器/MCP 权限验收矩阵仍须逐项完成。不能把构建、协议 fixture、catalog 声明或部分原生成功当作第二阶段完成。

## dev.20 增量（优先于下方历史盘点）

[dev.20 检查](evidence/phase2-dev20-checks-20261015.json) 记录交互 Docker TTY、PHP 默认/站点慢日志、站点 session 清理、FTP 强暂停与 Supervisor 脚本进程识别的源码及测试范围。新增 root Sites 用例在普通用户云端跳过，实际固定包服务/浏览器/MCP 验收仍需继续。

FTP 的旧 dev.19 暂停记录迁移尚未补齐；外部 pure-pw 并发写入尚需统一临时文件协议，当前要求停用外部并发修改。账号只读是明确功能缺口，不能以顶层目录 chmod 代替现有文件的禁止覆写；schema 现固定 false 并在副作用前拒绝 true。复制骨架未注册业务动作，不能视为复制完成。上述限制正在下一批开发中。

## dev.19 增量（2026-10-15，优先于下方历史盘点）

[dev.19 检查与边界](evidence/phase2-dev19-checks-20261015.json) 已记录实际实现与检查。下方表格是较早盘点，以下项目已从纯功能缺口进入“已实现、待原生验收”：Docker 单次 exec、同 Agent 证明的镜像归档、MySQL/MariaDB binlog、FTP 备份目标目录维护、FTP 账号状态、Redis/Valkey 脱敏集群视图。手动 HTTP-01 也已有源码与协议测试，公共 CA 验收由用户暂缓。

仍未关闭的功能包括交互 TTY、PHP 慢日志/站点 session/扩展安装、数据库复制与 SSH 同步及引擎专属操作、rsync/lsyncd、管理应用配置，以及其他下表列明的专用行为。FTP 暂停的实际账号拒绝语义正在核验；不能仅凭 enabled 字段或参数测试宣称登录已受阻。所有已有实现仍保留各自原生、平台、权限、任务恢复与 UI/MCP 验收条件。

## 结论与分类

第二阶段仍有确定的功能缺口：PHP slow log/站点 session/扩展安装，数据库 binlog 与复制变更/SSH 同步，Docker exec/镜像导入导出，rsync 实时同步，FTP 备份目标远端目录维护，以及若干管理应用的专有配置界面。PHP instance error log/session 与 Supervisor job log 已补入 Luna 批次；Ubuntu dev.16 浏览器已验证 FPM 日志、统计及零候选预览，未提交清理任务；独立 Supervisor daemon 的双路日志 RPC 与 PID 保持已通过，安装包 REST/MCP/Agent/UI 全链仍待验收。另有四个 DNS provider 明确不可用。通用 lifecycle、数据库 CRUD 或服务启动成功均不能覆盖这些行为。

同时存在三类不同的剩余条件：

- **功能缺口 F**：未找到等价的已注册业务动作及对应执行路径；需要代码、UI、MCP、权限、任务/恢复和行为测试。
- **原生验收缺口 N**：已有实现/本地协议证据，仍须指定发行版、架构、真实 daemon、systemd、失败恢复及升级保留数据验证。本轮新增源码闭环均不能直接升级为原生验收通过。
- **外部条件 E**：供应商账户、DNS zone、真实 CA、SMTP/Telegram 接收端、远程 SSH/FTP 等。无条件时明确阻塞；本地 TLS fixture 只证明其测试范围。可信历史版本产物缺失是供应链/兼容缺口，不等同缺凭据。

## 23 个核心插件：已实现行为与确定余项

共同已有：`internal/plugins/provider.go: Manager.Bindings` 注册 `software_plan/apply/service_control/recovery*`、catalog/实例读取；`lifecycle.go`、`install_journal.go`、`runtime.go`、`autostart.go` 提供受控生命周期、恢复与服务归属。每一行仍须按版本/平台逐项检查安装、升级、回滚、重启、卸载保留数据，不能共享一次 lifecycle 测试即视为全部完成。

| 插件 | 已有具体行为与新源码定位 | 确定剩余用户行为 / 旧动作定位 |
| --- | --- | --- |
| `openresty` | `internal/sites/provider.go`：站点 CRUD、分类/默认站点、Web 迁移、配置检查、日志、备份恢复、TLS/PHP 绑定；`web_reload.go` 检验 reload。 | N：真实 Web/PHP/TLS/备份到临时目标的串联、失败 reload 恢复、升级/保留数据。旧 `plugins/openresty/index.py` 的 `cron_add_check/cron_del_check` 自动健康看护，没有对应专用配置/可追踪调度映射；不把 `software_service_control check` 当作自动看护已经完成。 |
| `php` | `php.go: phpBindings` 七个动作：status、extensions、configuration、configure、guard status/configure、extension toggle；`sites_php_cli_*`、站点独立 FPM pool；`php_maintenance.go` 增加 instance FPM error log、有界 session 统计/快照清理。 | F：`plugins/php/index.py:421/425` slow log（现 runtime 未生成）；`:858/863` 的站点 pool session 管理仍待补，当前仅实例自有 files handler；`:973 installLib` 与 `uninstall_lib` 的可信安装/卸载。`php_extension_toggle` 仅切换已安装且可信的模块，缺模块明确报错；尚无等价 phpinfo、独立 OPcache blacklist 管理。 |
| `php-apt` | 复用上述 PHP typed 行为，发行版包来源；独立实例/版本及配置。 | F：同 `php` 的 slow log/站点 session/模块安装闭环，旧 `plugins/php-apt/index.py` 同名 actions；N：apt 版本、模块 ABI、CLI/FPM 一致与实际升级。 |
| `php-yum` | PHP typed 行为及 RPM 来源路径。 | F：同 `php` 的 slow log/站点 session/模块安装；N：yum/dnf 实际发行版、模块 ABI、版本共存，不可用 Debian 结果覆盖。旧 `plugins/php-yum/index.py`。 |
| `php-guard` | `php_guard_status/configure` 与 PHP 独立 worker/pool 配置；integration 标记。 | N：实际 worker 异常/重启、隔离与恢复；旧插件静态只声明 `status`，不能额外捏造动作计数；其守护行为需结合运行实现核验，enabled 标记不等于守护已验收。 |
| `phpmyadmin` | catalog 保留 signed-package 来源；`runtime.go` 写 integration 配置。新版数据库查询 UI 是自有能力。 | F：旧 `plugins/phpmyadmin/index.py` 的首页、PHP 版本选择、数据库目标选择、访问路径、监听端口及管理登录配置（`set_php_ver/set_pma_choose/set_pma_path/set_pma_port/set_pma_username/set_pma_password`）；未见对应专用新 binding。自有 SQL 查询页不能直接算第三方 phpMyAdmin 行为已迁移。N：实际安全部署/访问/日志。 |
| `mysql` | `database_managed.go`：受管库/用户 CRUD、权限/密码、备份恢复；`database_connections.go` 查询；`database_records.go` diagnostics/只读 replication status；本轮 SQL admin 初始化/轮转/恢复。 | F：旧 `plugins/mysql/index.py` 的 `binlog_list/bin_log/clean_bin_log`（`:422 cleanBinLog`）、主从配置启停/复制账户、SSH 目标/全量同步/修复、表优化修复与迁移进度；通用 SQL 不等价这些任务。N：本轮 admin 的真实 Unix Agent/daemon 轮转与崩溃恢复。 |
| `mysql-community` | 同 SQL 管理闭环，独立来源/版本。 | F：同 MySQL；旧 `plugins/mysql-community/index.py` 的 master/slave/SSH/binlog actions。N：社区版版本差异、重启与工具恢复。 |
| `mariadb` | 同 SQL 管理闭环，保留 MariaDB 来源与诊断差异。 | F：同 MySQL；旧 `plugins/mariadb/index.py`，含 `get_db_rw/set_dbrun_mode` 等运行模式专有行为需独立映射；N：MariaDB 用户/grant/备份工具及跨平台验收；本轮 Debian dev.18 管理员轮换 API/实际 daemon 子集通过。 |
| `postgresql` | 受管数据库/角色权限、参数查询、备份/恢复、只读复制状态；本轮 SQL admin 轮转。 | F：旧 `plugins/postgresql/index.py` 的 master/slave 配置、复制用户、SSH 全量同步、`pg_hba_conf` 专门访问规则及 slow/run log 行为。现 `replication_status` 只读，不能配置复制。N：实际 PG 主从/恢复、凭据轮转。 |
| `mongodb` | 受管库/角色、BSON 文档 CRUD、BSON/索引备份恢复、诊断和只读 replica-set status。 | F：旧 `plugins/mongodb/index.py:1126 replInit`、`repl_set_name/repl_set_node/del_repl_node/repl_close` 的 replica-set 变更；auth/key 配置与检查调度。Mongo admin 自定义轮转不在本轮 SQL-only 实现内。已删除库同名重建当前明确 `database_recreation_unsupported`，仍是受限行为，不能写为已支持。 |
| `pgadmin` | catalog 与 integration 占位保留。 | F：实际 pgAdmin 部署入口、首页/监听及登录配置；旧 `plugins/pgadmin/index.py:316/339/358`（port/username/password）。当前 catalog `distribution` 来源仍为 unavailable；不是只缺用户密码即可安装。 |
| `data_query` | `database_connection_*`、`database_query/database_instance_query`、SQL 表清单与 Mongo document actions，前端数据库资源页。 | N：网页/MCP 同身份查询/写入/拒绝/审计，真实 SQL/Mongo 与连接凭据绑定；不能因旧 index 只有 start/stop 就忽略其查询页面，但自有查询实现也不要求继续运行旧 Python 服务。 |
| `redis` | `cache_connections.go/cache_operations.go/cache_status.go`：连接、受限 namespace KV/hash/set/list、cursor 扫描、实际 INFO（含 replication）。 | F：旧 `plugins/redis/index.py` 的 `cluster_info/cluster_nodes` 专门集群视图无等价 binding；typed 软件配置只覆盖已定义键，不能视为旧任意配置都迁移。N：真实持久化/重启、集群条件与升级。 |
| `valkey` | 与 Redis 同 namespace/cache 行为，独立 engine。 | F：同 Redis 集群视图，旧 `plugins/valkey/index.py`；N：Valkey 实际协议/持久化/版本生命周期，不能直接套 Redis 证据。 |
| `memcached` | 缓存 profile、命名空间 KV、实际 stats；`runtime.go` 的受控配置与 lifecycle。 | N：真实服务配置/重启/内存参数与故障；ASCII adapter 明确不支持 SASL/database index，是支持边界。旧 `plugins/memcached/index.py` 的配置应逐键对照，不能把任意 `conf_env` 声明都算验收。 |
| `docker` | `docker.go` 容器/网络/卷创建、查看、启停、日志；`docker_images.go` digest 镜像列举/inspect/remove；本轮 `docker_registry.go/docker_registry_executor.go` 加密版本化 registry 凭据与绑定 digest pull；前端 `docker-management.tsx`。 | F：旧 `plugins/docker/index.py:282 dockerExec`、`:486/505` image save/load 与归档流；旧 registry repo catalog 列表不等同当前凭据 metadata 列表。N：真实 daemon、私有 registry auth、删除使用中镜像与失败 pull 恢复；不能用本地 HTTP fixture 代替。 |
| `pureftp` | `utility.go` 的 `ftp_list/status/create/update/delete`，受管虚拟用户、口令与读写目录；软件配置负责端口。 | F：旧 `plugins/pureftp/index.py` 的单账号 `start_ftp/stop_ftp` 启停，没有等价账号 enabled mutation；不可用删账号替代暂停账号。N：真实 FTP/FTPS 登录、只读/隔离、改口令/端口与服务重启。 |
| `rsyncd` | `utility.go` 的模块/账号受控 CRUD 与服务生命周期。 | F：旧 `plugins/rsyncd/index.py:735 lsyncdAdd`、`:845 lsyncdRun`、`:877` 及后续 exclude 管理；实时同步配置、运行状态/日志与排除规则未迁移。不能将 rsync daemon 模块 CRUD 当作 lsyncd 已完成。N/E：真实源/目的端同步、SSH/认证与失败恢复。 |
| `backup_ftp` | `backup_ftp.go`：目标注册/凭据、bounded remote list、受管 backup artifact upload/download，FTPS 校验。 | F：旧 `plugins/backup_ftp/index.py:131 createDir`、`:161 deleteFile` 与 `delete_dir` 的受限远端目录/文件维护；当前仅四种 bindings，无对应 mutation。N/E：真实 FTPS 目标与备份上传→下载→临时恢复闭环。 |
| `acme_pandominassl_apply` | `sites/acme_provider.go`：签发/续期/status/confirm/cancel、credential store；原生或受信 Lego DNS driver；本轮 `dns_dpi` 固定国际 API typed adapter。 | F：4 provider `dns_ddnss/dns_kinghost/dns_freedns/dns_cyon` explicit unavailable；旧 `plugins/acme_pandominassl_apply/index.py` 的 hook/sync-CF/sync-DNSPod 不可自动视为签发动作同义，须保留有效部署/同步需求并设计受限替代。N/E：真实 DNS/CA、泛域名/续期/传播/精确 cleanup 与部署。 |
| `supervisor` | `utility.go`：受信 program_id 的 job CRUD、per-job start/stop/restart；`supervisor_logs.go` 增加有界 stdout/stderr 读取、双流预览、revision/inode 核验后的 durable clear。 | N：旧 `confd_list_error_log/confd_list_trace_log/sup_clear_log` 已有 typed 行为，仍需真实进程组/异常重启/日志清理、网页/MCP 和卸载保留验收。clear 明确影响两路日志，允许运行时新追加，不承诺只清预览字节。任意 Shell command 旧入口须由受信 profile 替代。 |
| `webssh` | 委托 `internal/terminalbridge/provider.go` 与 `internal/terminal/` 的本地 PTY、SSH host-key/credential target、resize/input/read/close、分享授权、保存命令；非旧 Python daemon。 | N/E：真实 PTY/SSH 交互、断线回收、凭据轮转/host-key 拒绝、网页与 MCP 同权限。旧 index 静态动作空不意味着功能空；需连同旧 webssh 页面/后端 server 管理行为对照，不能凭 integration enabled 完成验收。 |

## 跨插件核心管理的剩余门槛

这些能力已找到实际实现，不应由于 tracker 中旧行仍 `planned` 而再列成“未开发”：

- 文件：`internal/files/actions.go` 提供浏览/编辑/历史、批操作预检、权限、归档解包、回收站、分段上传、远程下载；终端由上述 bridge/Agent 执行。须验收大文件流、跨 root/符号链接/恢复冲突、取消及断线清理。
- 系统：`internal/systemops/provider.go/host_changes.go/server_config.go` 有服务、SSH/firewall、时区/NTP、延迟电源、面板 listen/public_url/TLS 的受控 change plan/confirm/rollback；`schedules.go/scripts.go/notifications.go/history.go` 有持久任务、受信脚本、SMTP/Telegram 投递与历史监控；`internal/core/temporary.go` 已有临时 grant，不应把旧临时登录条目仍标无实现。
- 网站/TLS：`internal/sites/provider.go` 已有站点与恢复/Web 迁移/引用保护；`acme_provider.go` 与 journal/receipt 形成签发控制面。退出标准要求的是安装 Web/PHP→建站→TLS→DB→上传→真实访问→备份→临时目标恢复整个链路；分散 source tests 不等于这条链已在本轮原生通过。
- REST/MCP：动作统一注册是基础；所有剩余新动作仍须 UI 入口、scope/resource 拒绝、任务身份、审计和重试/并发验证。任何“后端存在”都不能直接完成网页/MCP 对称验收。
- 旧/新资源共管：有 protected instance、ownership receipt、revision、恢复/迁移计划，不等于旧管理器已停止修改全部目标。仍需真实切换、旧引用归属、冲突拒绝和恢复演练。

另外，旧核心 `web/admin/dashboard/dashboard.py:113 webhook` 委托 **第三阶段** `plugins/webhook`；当前未找到同等新版 webhook ingress。tracker 将核心入口列 P2、插件列 P3，属于确切跨阶段依赖；须明确保留阻塞或调整实施顺序，不可因有 `scripts_run` 就认为 webhook 外部触发已迁移，也不可自行删范围。

## 本轮已落地的首批三项

| 闭环 | 当前实现/证据 | 尚缺条件 |
| --- | --- | --- |
| SQL administrator | `database_admin_rotation.go/database_admin_executor.go` 的 inspect/initialize/rotate/recover、私有 prepare password/version；`frontend/src/components/database-admin-credentials.tsx`；`database_admin_*test.go`。 | Debian dev.18 + 实际 MariaDB/PostgreSQL 的 API→持久任务→Unix Agent 自定义轮换通过，source binary 的 SQL 提交前后/保存失败/重启恢复通过；N：其他引擎、平台、完整权限/升级与界面验收。仅 SQL，不能把 Mongo admin 自动列已完成。 |
| Docker registry/image | `docker_registry*.go`、`docker_images.go`；`frontend/src/components/docker-management.tsx`；registry/image source protocol tests。 | N/E：真实 daemon、实际可信 registry/账户的 authenticated digest pull 与凭据版本/错误恢复；archive save/load 与 exec 仍是 F。 |
| DNSPod International | `acme_dns_legacy_dpi*.go`、catalog native 注册；[DPI 证据](evidence/phase2-dpi-protocol-20261015.json)：7 个顶层本地 TLS tests normal/race PASS。 | N/E：真实 international 账户/zone、传播/签发/续期；不能用中国 DNSPod 代替。其余四 provider 协议能力阻塞详见同一证据。 |

最终产品源码身份参见 [dev.18 source snapshot](evidence/phase2-dev18-source-snapshot-20261015.json)；dev.13～17 历史清单保留。源码清单本身不证明 guest 已运行这些新增源码，原生报告必须绑定发行包校验和、架构/OS、动作和实际结果。现存本地协议证据如 [数据库 source protocol](evidence/phase2-database-source-protocol-20261013.json) 仅覆盖各自列明行为。

## 版本、平台和外部条件不能隐藏

`internal/plugins/catalog/core.json` 保存旧版本与 source kind；多个历史版本为 `source.kind=unavailable`，pgAdmin 的 distribution 同样不可用。它们是待建立可信产物/ABI/recipe 的兼容缺口，不能拿当前发行版默认包替代所有旧版本，也不能将列表保留解释为版本已可装。PHP source、apt、yum 与 MySQL/MariaDB 来源必须分别测；amd64/arm64 构建不证明 ARM64 原生执行。

P2 退出仍须干净目标安装/重复安装、升级/回滚、重启、卸载保留、连接保护与失败恢复的适用矩阵。E 类凭据应安全注入，不进入仓库或报告；本地 fake token、HTTP fixture 或零测试不能替代真实服务。没有任何一项可以由 stale tracker 状态直接推算完成比例。

## 后续开发与验收顺序

1. **完成本轮维护功能的实际链路**：PHP instance FPM 错误日志、session 统计/快照/清理与 Supervisor job 双路日志源码及界面已补。PHP 浏览器当前只读日志、统计与零候选预览通过，仍需实际过期/活跃 session 的正向清理和过期预览拒绝；Supervisor 独立 daemon RPC 已通过，仍需安装包 REST/MCP/Agent/UI 权限、审计、并发与任务结果。PHP 站点 pool session 和未配置的 slow log 不在已完成范围。
2. **Docker archive/exec**：补上传/下载、受控临时归属、取消/截断、终端/exec 权限与审计、真实 daemon 和私仓 digest pull；保留当前精确镜像授权与凭据不落盘约束。
3. **数据库专用行为**：补 binlog 查询/清理边界、复制配置与失败恢复、SSH 同步、对应界面和真实双端验收；通用 SQL 执行不能代替专用动作。
4. **其余插件与兼容矩阵**：rsync/lsyncd、FTP 远端目录、管理应用专有配置、可信历史版本与原生平台逐项推进。四个 DNS provider 先补可证实的 list/identity/精确 cleanup 协议；不采用 DDNSS 清空共享 slot 或 Cyon 按 name 删除全部 TXT 的行为来缩短清单。

每个闭环先完成实现和专项测试，再全量必要检查和真实固定包复验，最后更新矩阵的具体子项。第二阶段七项退出标准全部满足前，维持第二阶段进行中、第三阶段未开始。
