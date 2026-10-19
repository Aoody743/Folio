# phpMyAdmin/pgAdmin、ACME/DNS 与应用入口执行记录

更新时间：2026-10-19。本文记录范围计划的代码改动、本地验证和仍需目标主机/外部账号完成的验收；源码测试不代表生产支持承诺。

## 已完成的本地实现

- “软件 → 应用”入口现已出现在首发界面。新增通用 Node.js 模板；Node.js 和 Next.js 项目必须有根 package.json、npm 锁文件和 scripts.start，构建统一使用 npm ci，有 scripts.build 时才执行该构建脚本。Next.js 模板包含 npm v3 锁文件。
- Go 项目归档和 Git 导入现在都要求根 go.mod 与 go.sum；模板提供空 go.sum。Node.js 构建先把源码安全复制到受限 staging 目录，再以应用专属非特权身份安装依赖和构建；成功后才切换目录，失败保留当前项目。Git 导入记录仓库地址、分支和精确 commit；仓库 URL 不允许嵌入凭据，私有仓库仍明确不支持。
- Node.js 的受管服务以 npm run start 启动，并设置 HOST=127.0.0.1、HOSTNAME=127.0.0.1 和受管 PORT。服务启动后会轮询固定回环 upstream；在 20 秒内无 HTTP 响应时停止服务并报告失败。通用 Node.js 项目必须让启动脚本使用这些环境变量。
- ACME/DNS 的证书入口和相关动作现已出现在首发界面。代码已有加密 DNS 凭据、不可变凭据版本、持久 ACME 订单、逐订单 TXT 所有权回执和精确清理；下面列出的“可用”只表示代码适配器存在，不表示真实供应商账号或公开 CA 签发已通过。
- phpMyAdmin 和 pgAdmin 已接入可安装软件目录。phpMyAdmin 使用官方 5.2.3 发布包的固定 SHA-256、Debian 12/13 的签名 PHP CLI/扩展包、受管回环监听和 cookie 登录；安装前要求存在唯一的受管 MySQL/MariaDB 目标，或明确指定实例。pgAdmin 使用固定哈希校验的官方签名密钥和 Debian 12/13 amd64 APT 源，只安装 `pgadmin4-server`，不安装会接管 Apache 的 `pgadmin4-web`；首次安装创建管理员账号，密码不返回到普通计划/实例响应。两者默认只监听回环地址，需通过网站反向代理发布。
- 两个工具的安装/启停要求真实 systemd，并由受管服务执行；本轮代码测试通过，但运行容器没有 systemd，目标主机安装、登录、数据库连接、反向代理、重启恢复和升级仍未验收。目录中的旧动作仍标记为未一对一迁移；这不冒充旧插件 API 全量兼容。

## DNS 提供商状态

凭据字段以 API 输入名列出。所有下列“适配器已实现”的提供商仍需在 QA zone 用真实账号完成 TXT 创建、挑战、清理、重试和续期验证；测试替身不算供应商验收。

| 提供商 ID | 驱动 | 凭据字段 | 当前状态 |
| --- | --- | --- | --- |
| dns_cf | Lego | CF_Key, CF_Email, CF_Token, CF_Zone_API_Token | 适配器已实现；外部未验收 |
| dns_dp | Lego | DP_Id, DP_Key | 适配器已实现；外部未验收 |
| dns_dpi | Native | DPI_Id, DPI_Key, DPI_AuthField | 精确 TXT 回执已实现；外部未验收 |
| dns_gd | Lego | GD_Key, GD_Secret | 适配器已实现；外部未验收 |
| dns_pdns | Lego | PDNS_Url, PDNS_ServerId, PDNS_Token, PDNS_Ttl | 适配器已实现；外部未验收 |
| dns_lua | Lego | LUA_Key, LUA_Email | 适配器已实现；外部未验收 |
| dns_me | Lego | ME_Key, ME_Secret | 适配器已实现；外部未验收 |
| dns_aws | Lego | AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, AWS_REGION, AWS_HOSTED_ZONE_ID | 适配器已实现；外部未验收 |
| dns_ali | Lego | Ali_Key, Ali_Secret | 适配器已实现；外部未验收 |
| dns_ispconfig | Native | ISPC_User, ISPC_Password, ISPC_Api, ISPC_Api_Insecure | HTTPS/目标校验与精确 TXT 回执已实现；外部未验收 |
| dns_ad | Lego | AD_API_KEY | 适配器已实现；外部未验收 |
| dns_linode_v4 | Lego | LINODE_V4_API_KEY | 适配器已实现；外部未验收 |
| dns_freedns | — | FREEDNS_User, FREEDNS_Password | 不可用；客户端无受审适配器 |
| dns_cyon | — | CY_Username, CY_Password, CY_OTP_Secret | 不可用；未验证 OTP 工作流 |
| dns_gandi_livedns | Lego | GANDI_LIVEDNS_TOKEN, GANDI_PERSONAL_ACCESS_TOKEN | 适配器已实现；外部未验收 |
| dns_knot | Native | KNOT_SERVER, KNOT_KEY, KNOT_ZONE | 精确 TXT 回执已实现；外部未验收 |
| dns_dgon | Lego | DO_API_KEY | 适配器已实现；外部未验收 |
| dns_cloudns | Lego | CLOUDNS_SUB_AUTH_ID, CLOUDNS_AUTH_ID, CLOUDNS_AUTH_PASSWORD | 适配器已实现；外部未验收 |
| dns_namesilo | Lego | Namesilo_Key | 适配器已实现；外部未验收 |
| dns_azure | Lego | AZUREDNS_SUBSCRIPTIONID, AZUREDNS_TENANTID, AZUREDNS_APPID, AZUREDNS_CLIENTSECRET, AZUREDNS_RESOURCEGROUP | 适配器已实现；外部未验收 |
| dns_selectel | Lego | SL_Key | 适配器已实现；外部未验收 |
| dns_zonomi | Lego | ZM_Key | 适配器已实现；外部未验收 |
| dns_kinghost | — | KINGHOST_Username, KINGHOST_Password | 不可用；无受审原生适配器 |
| dns_zilore | Native | Zilore_Key | 精确 TXT 回执已实现；外部未验收 |
| dns_gcloud | Lego | GCE_SERVICE_ACCOUNT_JSON, GCE_PROJECT, CLOUDSDK_ACTIVE_CONFIG_NAME | 适配器已实现；外部未验收 |
| dns_mydnsjp | Lego | MYDNSJP_MasterID, MYDNSJP_Password | 适配器已实现；外部未验收 |
| dns_doapi | Lego | DO_LETOKEN | 适配器已实现；外部未验收 |
| dns_online | Native | ONLINE_API_KEY | 精确 TXT 回执已实现；外部未验收 |
| dns_cn | Lego | CN_User, CN_Password | 适配器已实现；外部未验收 |
| dns_ultra | Lego | ULTRA_USR, ULTRA_PWD | 适配器已实现；外部未验收 |
| dns_hetzner | Lego | HETZNER_Token | 适配器已实现；外部未验收 |
| dns_ddnss | — | DDNSS_Token | 不可用；未验证动态 DNS TXT API |
| dns_huaweicloud | Lego | Huawei_AK, Huawei_SK, Huawei_Region, HUAWEICLOUD_Username, HUAWEICLOUD_Password, HUAWEICLOUD_DomainName | 适配器已实现；外部未验收 |
| dns_tencent | Lego | Tencent_SecretId, Tencent_SecretKey | 适配器已实现；外部未验收 |

## 尚未通过的目标环境验收

当前执行容器报告 Debian 13.6，但 PID 1 是 tail、systemd 状态为 offline，Node/npm 来自 Codex 工具运行时而不是 /usr/bin/npm。这不足以证明 Debian 13 主机的可信软件安装、systemd 服务启动/重启、反向代理或卸载行为；本轮也没有 Ubuntu 24.04 干净主机、真实 DNS 供应商账号、测试 zone 或 ACME staging/production 域名。

因此以下内容保持“未验收”，不得标成 release-ready：

- Debian 13 amd64 和 Ubuntu 24.04 amd64 上 Git/OpenSSH、Node.js/npm、Go 的签名包冷安装、升级、重复安装、状态和卸载。
- Node.js/Next.js/Go 在目标主机上的归档和 Git 创建、构建、失败恢复、服务重启、站点反代及删除全链路。私有 Git 认证仍未实现。
- 所有 30 个已实现 DNS 适配器的真实 API 行为、DNS-01 staging/公开 CA 申请和续期；HTTP-01 公开 CA canary 也未运行。
- Debian 12/13 上 phpMyAdmin/pgAdmin 的冷安装、管理员登录、真实 MySQL/PostgreSQL canary、HTTPS 反代、备份保留、服务重启恢复和升级；代码实现已开放，但目标主机验收仍待完成。

## 本地验证

- /workspace/tools/go1.27.2/bin/go test ./...：通过。
- cd frontend && npm run typecheck && npm test：通过，84 项测试全部通过。
- phpMyAdmin/pgAdmin 定向单测覆盖可信 catalog 声明、回环配置、首个管理员口令校验、实例预检、响应标识和敏感字段脱敏；这些测试不替代 systemd/真实包运行验收。
- Next.js 模板执行 npm ci --dry-run --ignore-scripts --offline：锁文件与依赖清单一致。
- 无依赖 Node.js 模板执行 npm ci --dry-run --ignore-scripts --offline：锁文件格式和清单一致。
- go test -tags hosted_apps_integration ... 的真实非特权 npm 构建用例在当前非 root 容器中按设计跳过；目标主机实测仍待安排。
- ACME/DNS 协议测试通过，但不连接外部供应商或 CA。
