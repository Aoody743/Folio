# 第一阶段验收

2026-10-13（Asia/Singapore）：第一阶段退出标准通过，进入第二阶段。第一阶段只完成基础核心，不代表网站、数据库与70插件已经迁移。

## 实际结果

| 检查 | 结果与证据 |
| --- | --- |
| 固定云端工具链 | Go1.27.2官方SHA-256校验、默认checksum DB模块校验、Node24与npm锁定安装；setup-cloud.sh完整重跑通过 |
| Go正常与race | `go test -p1 ./...`、`go test -race -p1 ./...`通过；CLI、Unix Agent、核心SQLite、HTTP、持久任务、官方MCP SDK |
| 前端 | Next生产构建／TypeScript／静态导出通过；真实Astryx与中文Provider；4项API客户端测试通过 |
| HTTP/MCP黑盒 | 实际dev.3二进制14项测试全部通过，包括两种MCP传输、OAuth、权限／撤销、TOTP、便签冲突历史与任务／重启 |
| 最终浏览器 | 实际dev.4完整流程通过：登录、四卡、五底栏、五主题、320/390/768/1440视口、人／MCP／API真实来源、SSE、历史、并发合并、离线草稿与重试、Markdown XSS、键盘/Escape |
| 任务 | 真文件写入、备份／SHA256验证、SQLite任务重启持久化；真实子进程的失败、取消、超时、输出上限、进程组清理与4并发／有界队列测试 |
| 发行包 | 实际dev.4 Linux amd64、arm64构建与校验；amd64真实包的HTTP、初始化状态、完整Next资源/CSP、运行锁、退出、重复安装和卸载保留数据通过 |
| 干净VM | 实际dev.3 Debian13 amd64、Linux6.12.107、systemd257、QEMU TCG：干净安装→普通powerlinux用户服务→健康→重复安装→真实登录/便签→服务重启持久化→卸载保留数据通过 |
| 修改范围 | VM所选Shell、locale、hosts、resolv.conf、profile、motd、cron文件前后SHA256相同；核心安装只使用PowerLinux自有用户、目录与服务 |
| 性能基线 | 完整实际dev.3黑盒中的20并发/200状态请求p50=8.77ms、p95=38.14ms，健康p95=4.56ms，RSS100,752KiB（98.39MiB） |
| 可重复启动 | 当前普通用户云端运行时重复启动、SIGTERM与重启通过；安装与启动配置草稿保存，尚未发布／新任务恢复验证 |

Go业务在dev.3与dev.4之间未改变；dev.4只修正API客户端的界面来源标识，最终实际包与浏览器均已再验收。

最终amd64包：`powerlinux-0.1.0-dev.4-linux-amd64.tar.gz`，SHA256：`5cffe27deb4058d7ac79932978c84c2e807abe5a7a6f0ea094db756ea08f9114`。产物留在云端`/workspace/.cache/rewrite-release/0.1.0-dev.4`，BUILDINFO明确标记未提交开发源为dirty；不是正式发布包。

## 测量条件与修复记录

HTTP使用Go net/http连接复用，100次预热后计时200次真实请求，20并发。当前云工作区CPU配额4核／32GiB；这不是2核／2GiB标准宿主，也不是长期稳定性验收。测试时QEMU运行。独立诊断的46.85ms与完整suite的38.14ms为不同负载下的观测，不混作同一次结果。

初始Python计时包括每次构建默认HTTPS证书上下文；独立无HTTP实验确认其开销，测量已纠正，目标与20/200并发条件保持。第一次VM安装调用超过150秒，后续确认服务健康、重复安装退出0；安全清理该试验后，最终dev.3干净安装在原时限下全部通过，初始失败未抹除。

主要真实修复包括：Next `_next`嵌入、便签/历史/审计同事务、MCP结构化错误、HTTPS Cookie与回环代理边界、SSE撤销／关闭、任务并发上限、父Token有效期／权限／受众与子树撤销、轮询写入节流、合并后自动保存，以及API客户端来源标签。

## 后续范围

OpenAPI自动产物、完整Provider注册／嵌套schema及跨动作幂等在第二阶段扩展，支撑真实运维写操作；第一阶段便签修订、初始化与重复安装已具有对应并发／自然幂等保障。

特权Agent生产部署及具体系统管理动作、23核心插件及其特有功能属于第二阶段。剩余47插件、实际跨版本升级／失败回滚／宿主重启、Ubuntu、arm64实际运行、迁移和标准宿主／长期性能属于后续门禁，均不在此宣布通过。

可重复命令见[tests/rewrite/README.md](../../tests/rewrite/README.md)与[发行说明](../../scripts/rewrite/README.md)。机器证据：[VM](evidence/phase1-vm.json)、[独立HTTP诊断](evidence/phase1-http-diagnostic.json)。实际全套日志与UI截图在云端`/workspace/powerlinux/test-artifacts`及`/workspace/powerlinux/qa-artifacts`，不含运行秘密。
