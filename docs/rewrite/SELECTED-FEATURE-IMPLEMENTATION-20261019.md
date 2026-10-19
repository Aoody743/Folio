# phpMyAdmin/pgAdmin、ACME/DNS 与应用入口执行记录

更新时间：2026-10-20。本文记录范围计划的代码改动与验收边界。beta2 的系统目标按用户确认收敛为 Ubuntu 24.04 amd64。逐项目标系统证据见[验收记录](./SELECTED-FEATURE-ACCEPTANCE-20261019.md)；源码测试不代表生产支持承诺。

## 已完成的本地实现

- “软件 → 应用”入口现已出现在首发界面。新增通用 Node.js 模板；Node.js 和 Next.js 项目必须有根 package.json、npm 锁文件和 scripts.start，构建统一使用 npm ci，有 scripts.build 时才执行该构建脚本。Next.js 模板包含 npm v3 锁文件。
- Go 项目归档和 Git 导入现在都要求根 go.mod 与 go.sum；模板提供空 go.sum。Node.js 构建先把源码安全复制到受限 staging 目录，再以应用专属非特权身份安装依赖和构建；成功后才切换目录，失败保留当前项目。Git 导入记录仓库地址、分支和精确 commit；仓库 URL 不允许嵌入凭据，私有仓库仍明确不支持。
- Node.js 的受管服务以 npm run start 启动，并设置 HOST=127.0.0.1、HOSTNAME=127.0.0.1 和受管 PORT。服务启动后会轮询固定回环 upstream；在 20 秒内无 HTTP 响应时停止服务并报告失败。通用 Node.js 项目必须让启动脚本使用这些环境变量。
- ACME/DNS 的证书入口和相关动作现已出现在首发界面。代码已有加密 DNS 凭据、不可变凭据版本、持久 ACME 订单、逐订单 TXT 所有权回执和精确清理；下面列出的“可用”只表示代码适配器存在，不表示真实供应商账号或公开 CA 签发已通过。
- beta2 的 Ubuntu 首装器默认启用软件和网站/证书 Agent 模块，使已验收的应用与证书入口可直接使用；升级时保留现存 Agent 策略，且可用 `--no-managed-features` 跳过首装模块。Git、Node.js、Go 的目录验证状态和支持平台限定为 Ubuntu 24.04 amd64。
- phpMyAdmin 和 pgAdmin 暂不开放：目录已删除旧版本选择并将默认来源与能力标为 unavailable，因此不会出现在可安装软件列表。Ubuntu 24.04 的签名仓库有 phpMyAdmin 候选包，但当前产品没有将它绑定到受管 PHP-FPM 和 HTTPS 站点的生命周期；pgAdmin 官方 apt 仓库及其签名密钥存在，但当前包管理器尚无受控第三方仓库注册流程，也没有隔离运行时适配器。

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

- beta2 的 Git、Node.js、Go 目录条目仅声明 Ubuntu 24.04 amd64；不声明 Debian、ARM 或其他发行版/架构支持。
- Go 1.22.2 运行时及 Go 归档应用构建、非 root systemd 服务和 HTTP 验收已通过。公开 HTTPS Git 应用与固定 SSH 指纹应用均成功部署；错误 SSH pin 拒绝且不修改应用来源。私有 Git 认证仍未实现。
- DNS 协议测试使用本地 API/CA fixture；真实 DNS 供应商账号、隔离 zone、公开 CA staging/production 签发和续期尚未执行。全部外部 provider 仍未验收。
- phpMyAdmin/pgAdmin 因产品级来源集成和受管 Web 生命周期适配器缺失而不可安装；数据库 canary、HTTPS 反代和恢复测试未执行。beta2 保留这一门禁。

## 源码、集成与目标主机验证

- `go test -mod=readonly ./...`：通过。
- `frontend` 的 `npm run typecheck` 与 `npm test`：通过，84/84。
- `make rewrite-build`：qa.11 amd64 构建、前端类型检查和生产构建通过；该包已在 Ubuntu Git 验收 VM 安装并通过健康检查。
- root VM 的 `hosted_apps_integration` 测试：Node/npm 锁文件构建和项目切换通过；测试 fixture 位于 `/tmp` 的直接子目录，确保 `nobody` 可遍历其父路径。
- Ubuntu 24.04 amd64 VM：Node.js 24.22.0、Go 1.22.2 安装通过；Node 归档应用升级及重启保持运行；Go 归档应用构建和服务通过；公开 HTTPS 与固定 SSH pin Git 应用部署通过，错误 pin 被拒绝；DNS 凭据 CRUD 与脱敏通过；HTTP-01/DNS-01 本地协议、重启及清理恢复测试通过。
- 更完整的逐项结果、发行包哈希和外部验收限制见[验收记录](./SELECTED-FEATURE-ACCEPTANCE-20261019.md)。
