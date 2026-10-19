# phpMyAdmin/pgAdmin、ACME/DNS 与应用入口验收记录

验收日期：2026-10-19 至 2026-10-20。beta2 目标系统按用户确认限定为 Ubuntu 24.04 amd64。状态区分真实 Ubuntu 主机验收、本地协议测试和待实现/待外部条件项目。

## 十项功能结果

| # | 功能 | 结果 | 验收证据与边界 |
| --- | --- | --- | --- |
| 1 | Node.js 运行时 | 通过（Ubuntu 24.04 amd64） | 官方 Node.js 24.22.0 归档按固定 SHA-256 校验后安装；Node 应用保持运行。该项不是 Debian 或 ARM 支持声明。 |
| 2 | Go 运行时 | 通过（Ubuntu 24.04 amd64） | Ubuntu 签名仓库包安装 Go 1.22.2；`go version` 健康检查通过。 |
| 3 | 源码归档导入及部署/更新 | 通过（Ubuntu 24.04 amd64） | Node 应用从归档部署 v1、重启、更新至 v2 并再次重启；HTTP 返回 `qa-node-archive-v2-ok`，应用 revision 11 且运行中。以系统 `nobody` 身份运行的 `npm ci` 集成用例通过。Go 归档应用以独立非 root 用户构建并运行，`powerlinux-app-qa-go.service` active，HTTP 返回 `QA Go runtime is running`；运行时版本 `go1.22.2 linux/amd64`。 |
| 4 | 公开 HTTPS Git 导入 | 通过（Ubuntu 24.04 amd64） | 导入并部署 `heroku/node-js-getting-started` 的 `main`，记录精确 commit `8b8dec11fcf027522a857e0c831ee01b7124f673`；HTTP 200。首次部署发现受管构建环境没有代理/CA 配置，已将经过校验的代理与 CA 传入构建任务，qa.11 重试通过。升级后该应用仍运行。 |
| 5 | 固定 SSH 主机指纹 Git 导入 | 通过（Ubuntu 24.04 amd64） | 错误 SHA-256 指纹被拒绝且应用来源 revision 不变；正确指纹后导入并部署，记录 commit `71575f2f31443c02cd7e6579cde82b290f9c6cd1`，HTTP 返回 `qa-node-pinned-ssh-ok`。 |
| 6 | DNS 凭据生命周期 | 通过（API 行为/Ubuntu 24.04 amd64） | 用一次性虚构凭据完成创建、轮换、旧 revision 冲突拒绝、列表脱敏、删除和删除后缺席检查。未连接真实 DNS 账号；凭据值未写入本记录。 |
| 7 | DNS-01 | 协议与恢复测试通过；真实供应商验收延期 | 在 Ubuntu root/systemd VM 运行手工 DNS pending/重启/确认/取消、Knot 与 ISPConfig 本地 API fixture、精确 TXT 清理和丢失清理回执恢复测试。按用户要求，真实 DNS zone/公开 CA 验收后续进行。 |
| 8 | HTTP-01 | 协议测试通过；公开 CA 验收延期 | 在 Ubuntu root/systemd VM 运行手工 HTTP 上传/重启/按请求确认，以及本地 CA JWS/CSR/并发网站读取测试。按用户要求，不在本轮使用公开 CA staging/production 域名。 |
| 9 | phpMyAdmin | 未实现；beta2 保持隐藏 | Ubuntu 24.04 签名仓库有候选包，但产品缺少受管 PHP-FPM/HTTPS 站点适配器，不能安装后安全发布。数据库 canary 与恢复验收未执行。 |
| 10 | pgAdmin | 未实现；beta2 保持隐藏 | 官方 apt 仓库与密钥存在，但产品缺少受控第三方仓库注册/密钥校验流程和隔离服务适配器。PostgreSQL/HTTPS 集成验收未执行。 |

## 通用回归与构建

- `GOTOOLCHAIN=local GOMODCACHE=/workspace/Folio/.cache/rewrite/go-mod GOCACHE=/workspace/work/go-build-cache GOPATH=/workspace/work/gopath /workspace/work/go1.27.2/bin/go test -mod=readonly ./...`：通过。
- `frontend`：`npm run typecheck` 通过；`npm test` 通过，84/84。
- `make rewrite-build GO_BIN=/workspace/work/go1.27.2/bin/go REWRITE_VERSION=0.1.0-qa.11 REWRITE_ARCHITECTURES=amd64 REWRITE_OUTPUT=/workspace/work/folio-qa/package-selected-qa11`：通过，含前端生产构建及 amd64 包装。
- Ubuntu Git 验收 VM 升级至 qa.11 成功；健康检查版本为 `0.1.0-qa.11`，Node 归档及 SSH Git 应用升级后均保持运行。
- 验收发行归档 SHA-256：`97b4cb029e82549d657598d27d1e11ec5e80393ff69ce45bef9ee18607bb790e`。
- `hosted_apps_integration` root VM 用例：Node/npm 非特权构建与项目切换通过。
- root-only ACME/DNS 系统测试在 Ubuntu 24.04 amd64 VM 执行通过；普通容器跳过的项目没有被算作目标系统证据。

## 计划验收结论

beta2 只开放 Ubuntu 24.04 amd64 已验收的应用与证书功能。原十项计划尚未全部关闭：phpMyAdmin/pgAdmin 未实现；真实 DNS provider 与公开 CA canary 按用户要求延期。当前证据不能外推到其他发行版或架构，也不能用于宣称上述两种数据库管理工具或任何 DNS provider 已正式支持。
