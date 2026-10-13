# 现有功能全量迁移清单

本清单依据当前提交 `a0b700753f6242d40dd60b40a4533960b58a3632`，用于落实“现有面板功能新版也要有”。全部下列功能进入迁移范围；阶段安排只决定顺序，不作为删减授权。

统计：235 个核心 HTTP 路由声明；70 个插件目录；69 个插件存在 index.py，均可被 AST 解析；另一个 system_safe 使用其他结构，不能按缺少 index.py 判为可删除。按插件分别去重后，静态提取到 1,358 条 func/action 字符串。动作字符串不是已经验证的独立功能数，也未覆盖动态构造及其他入口。Socket.IO 终端事件另行登记。

原始路由、函数、代码位置、插件元数据、入口函数、动作字符串和版本目录见 [feature-inventory.json](feature-inventory.json)。原始路由是各文件中的声明，完整 URL 仍需结合 Blueprint 前缀；本轮未逐项执行全部功能。

## 核心能力

| 模块 | 路由声明数 | 必须覆盖的行为 |
| --- | --- | --- |
| 仪表盘与登录 | 10 | 登录、MFA、临时登录、概览、关闭页、Hook；认证功能统一安全实现 |
| 网站 | 84 | 站点/域名、分类、启停、目录绑定、运行目录、默认站点、索引、PHP版本、伪静态、限速、防盗链/访问保护、反向代理、重定向、日志、备份、证书与ACME/DNS API |
| 文件 | 30 | 列表、搜索、读写、编码、上传/分段上传、下载/远程下载、复制移动、批量操作、权限所有者、压缩解压、目录大小、删除与回收站 |
| 设置 | 47 | 账户密码、MFA、临时登录、API应用、邮件/Telegram通知、面板TLS/域名/端口/IPv6、目录、标题页脚、安全入口/Basic Auth、时间同步与时区等 |
| 防火墙与SSH | 12 | 防火墙状态和端口、Ping、SSH端口、root登录、密码/公钥认证等 |
| 计划任务 | 11 | 创建编辑删除、周期与目标、启停、手动执行、日志与备份相关任务 |
| 系统 | 13 | CPU/内存/负载/网络/磁盘、环境、历史、监控设置、重启/关机、更新 |
| 插件 | 15 | 软件列表、安装卸载、首页展示、配置页、插件上传/导入、动作调用与扩展菜单；新版使用版本化权限契约 |
| 软件页面 | 1 | 软件管理界面及插件导航 |
| 日志 | 5 | 面板日志查询清理、系统审计文件与内容 |
| 监控页面 | 2 | 监控界面与相关读取 |
| 任务 | 5 | 数量、队列、执行日志、进度、移除 |
| 终端事件与插件 | 不计入上述HTTP统计 | 本地/远程SSH或PTY、认证WebSocket、窗口尺寸、交互输入输出、超时与断线清理 |

## 全部插件

分类用于迁移管理，不代表旧仓库中的 type 字段已规范化。每个插件都要登记安装/版本选择、状态、配置、特有动作、备份恢复/升级卸载及所需外部服务。缺少的能力应明确声明，不能伪造支持。

| 分类 | 目录标识与名称 |
| --- | --- |
| 网站、PHP、安全与统计 | `openresty`（OpenResty）；`php`（PHP）；`php-apt`（PHP[APT]）；`php-yum`（PHP[YUM]）；`php-guard`（PHP守护）；`phpmyadmin`（phpMyAdmin）；`op_waf`（OP防火墙）；`op_load_balance`（OP负载均衡）；`webstats`（网站统计）；`acme_pandominassl_apply`（ACME泛域名SSL）；`xhprof`（xhprof）；`tamper_proof_py`（网站防篡改程序PY） |
| 数据库与缓存 | `mysql`（MySQL）；`mysql-community`（MySQL[Tar]）；`mariadb`（MariaDB）；`postgresql`（PostgreSQL）；`mongodb`（MongoDB）；`redis`（Redis）；`valkey`（valkey）；`memcached`（Memcached）；`pgadmin`（pgadmin）；`data_query`（数据管理） |
| 搜索与推荐 | `sphinx`（sphinx）；`manticoresearch`（manticoresearch）；`gorse`（Gorse） |
| 容器、代理与网络 | `docker`（Docker）；`haproxy`（haproxy）；`keepalived`（keepalived）；`lvs`（LVS）；`varnish`（varnish）；`doh`（DoH）；`mosquitto`（mosquitto）；`mtproxy`（mtproxy）；`xui`（xui） |
| 监控与排障 | `prometheus`（prometheus）；`grafana`（grafana）；`loki`（loki）；`zabbix`（Zabbix）；`zabbix_agent`（Zabbix Agent）；`nezha`（哪吒监控）；`simpleping`（SimplePing）；`dynamic-tracking`（动态追踪）；`task_manager`（任务管理器） |
| 存储、传输、备份与迁移 | `backup_ftp`（FTP存储空间）；`gdrive`（谷歌云网盘）；`msonedrive`（OneDrive）；`alist`（Alist）；`cloudreve`（cloudreve）；`rsyncd`（rsyncd）；`pureftp`（PureFtpd）；`migration_api`（一键迁移API） |
| Git、LDAP与邮件 | `gitea`（Gitea）；`gogs`（Gogs）；`ldap`（LDAP）；`phpldapadmin`（phpLDAPadmin）；`lam`（LAM）；`imail`（邮件服务） |
| 系统维护 | `swap`（swap）；`sys-opt`（系统优化）；`system_safe`（系统加固）；`fail2ban`（fail2ban）；`clean`（日志清理）；`supervisor`（supervisor） |
| 其他应用与自动化 | `ollama`（ollama）；`cryptocurrency_trade`（量化交易）；`dztasks`（dztasks）；`tgbot`（tgbot）；`tgclient`（tgclient）；`webhook`（WebHook）；`webssh`（SSH终端） |

## 特别容易漏掉的行为

- MySQL 不只有创建数据库：现有入口还包括用户/访问权限、备份导入、外部导入、binlog、主从配置、SSH连接与同步、表优化修复、运行参数与版本切换。
- PHP 不只有启停：扩展安装卸载、禁用函数、PHP/FPM配置、超时/上传限制、session、慢日志、phpinfo、CLI版本及不同安装来源均要登记。
- Docker 不只有启停容器：镜像拉取/导入导出、仓库登录、创建/删除、日志、exec和端口等均在范围内。
- 证书涵盖上传/复用、自动签发、DNS API与泛域名、续期日志、HTTPS跳转；第三方DNS凭据不由核心安装器默认索取。
- 备份包含本地、FTP、Google Drive、OneDrive和迁移能力；保留第三方服务的认证与错误处理。
- 系统功能包含SSH、防火墙、时间同步、优化和加固；低侵入通过显式启用与变更记录实现，不能靠删掉这些功能实现。
- 旧版本选择、共存能力和平台限制独立登记：info.json的版本列表与磁盘脚本目录有差异，不能只看其中一个就宣称兼容全部版本。

## 迁移完成标准

阶段分配见 [DEVELOPMENT-PLAN.md](DEVELOPMENT-PLAN.md)，逐项状态见 [migration-tracker.json](migration-tracker.json)。三个阶段最终覆盖全部现有能力，不用阶段安排授权删减。

为每个功能建立：旧入口 → 用户行为 → 新界面/API/MCP/Agent动作 → 支持版本/平台 → 所需凭据/服务 → 验证用例 → 当前状态。

AI Native 与原生 MCP 纳入新版基础范围，设计见 [AI-NATIVE-MCP.md](AI-NATIVE-MCP.md)。235 个核心路由所代表的能力及全部 70 个插件的有效动作都要登记 AI 接入方式；交互终端等特殊能力需专门设计会话工具，不能简单把原始 HTTP 或任意 root Shell 暴露给 AI。

新版增加主页共享便签，支持人/MCP客户端读取、编辑、归档、恢复和版本历史，详见 [UI-SPEC.md](UI-SPEC.md)。这是新增能力，不计入旧版235个路由的统计；面板采用精简底部导航，不内置AI聊天。

状态采用待实现、开发中、待验证、已通过、明确阻塞。不能把有按钮、安装包存在、未执行或依赖缺失写成通过。现有缺陷和无效入口单独记录，迁移目标保留用户能力，不保留认证漏洞或错误行为。

核心功能、全部插件及其有效动作完成逐项验收前，不宣称“现有功能全部迁移完成”。如果某项受第三方或平台限制无法完成，应显式报出阻塞，不能静默弃用。

接口名称和Python执行协议可以由新架构重新定义；功能兼容、数据可迁移和用户操作结果是验收目标。对用户已有第三方自定义插件，额外提供规范迁移文档或范围明确的兼容层，不能承诺任意旧root脚本直接安全运行。
