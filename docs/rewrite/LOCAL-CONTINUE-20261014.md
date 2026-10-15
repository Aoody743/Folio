# 本地 Codex 接续：2026-10-14

本文件保留 2026-10-14 转交本地 Codex 的历史快照。2026-10-15 用户已将本地更新推回 `refine` 并恢复云端开发，当前状态以 [PROGRESS](PROGRESS.md) 为准；DEVELOPMENT-PLAN 的功能范围和阶段退出标准继续有效。

仓库：https://github.com/Aoody743/PowerLinux ，分支 `refine`。不要修改 `main`，不要覆盖本地已有工作。建议从远端 `refine` 建立 `refine-local` 工作分支。

## 本地接续批次补充

本地 `refine-local` 已继续修复安装恢复并实现跨 Web 实例迁移。最新实现、独立审查及验证边界见 [PROGRESS 本地批次](PROGRESS.md#本地第二阶段接续2026-10-14) 和 [本地源码证据](evidence/phase2-local-20261014/source-checks.json)。下方 `92752b9` 草稿描述保留为交接时快照；固定包与浏览器仍按实际后续证据确认，不由源码测试推断。

## 已交付与验证

- `0552448` 是安装恢复草稿之前的源码检查点。该批全量 Go race（13 个测试包）、Go vet、36 项前端测试、TypeScript、Next.js 静态导出、Linux amd64/arm64 构建和实际 amd64 私有 prefix 安装检查通过。
- dev.12 实际 Debian API/Agent 已完成 MariaDB、PostgreSQL 新实例安装、建库、权限档位、真实 dump/restore、精确二进制和 BIGINT 数据恢复、保留数据卸载/重装与最后清理。见 [实际数据库证据](evidence/phase2-dev12-database-live-integration-20261013.json)。
- Ubuntu dev.12 的产品自启设置及实际 reboot 检查通过：API、Agent、OpenResty、PHP 服务自动运行，策略和便签连续。
- Debian 上固定 sites 测试二进制的 24 组 root 用例通过，含高级 Web/PHP 配置精确恢复、WAL、SIGKILL、备份流和真实 PHP CLI。网站 Check/Reload 使用隔离测试运行时，不能替代真实 OpenResty/FPM 功能语义。见 [root 网站证据](evidence/phase2-sites-journal-dev12-20261013.json)。
- Redis/Memcached 只读 live status 通过；旧 fixture 的认证失配单列，未为测试修改共享 Redis ACL。

## 本次保留的未完成工作

安装恢复草稿已经纳入接续检查点：`internal/plugins/install_journal.go`、`install_journal_test.go` 和 lifecycle/runtime/privileged/provider 的接入。它包含私有 journal、unit nonce/create-only proof、inspect、stop_preserve 和受限 continue。

接续前仅完成以下检查：

```sh
go test -p 1 -race -timeout 90s ./internal/plugins \
  -run '^(TestInstallJournal|TestInstallUnit|TestInstallRecovery)' -count=1
python3 -m py_compile tests/rewrite/phase2/test_sites_workflow_browser.py
git diff --check
```

这些检查通过，**草稿尚未经过完整回归、固定包构建、真实 systemd 故障恢复或独立安全审查**。不要把它当作已完成能力或直接部署到日常主机。先审查所有变更，再完成有意义的错误、并发和中断验证。

`tests/rewrite/phase2/test_sites_workflow_browser.py` 已保存，但只做了语法检查，未执行真实浏览器流程。需要独立 QA VM、可恢复的 managed 站点及 checkout 外 0600 私有 fixture；不要用 mock 或直接修改真实站点来冒充通过。

## 下一步优先级

1. 完成安装 journal 与受控恢复，特别是首次安装 boot/receipt 失败、崩溃、取消、归属证明、CAS、重复请求和秘密不进入结果；未知/旧残留不得自动接管，continue 不重跑包安装、DDL 或密码初始化。
2. 完成网站跨 Web 实例迁移、真实 Web/PHP 功能链与新分类/系统工作流的浏览器验证。
3. 补齐数据库凭据轮换、外部导入/隔离恢复、复制/binlog/SSH 同步、Valkey/主从及 MongoDB 原子归属方案。
4. 完成 Docker 私仓凭据与 digest pull、受授权镜像列表、导入导出/exec；补 FTP/rsync/Supervisor 等插件细节。
5. 逐条完成第二阶段 23 个插件和验收矩阵。真实外部 DNS/CA、平台或凭据缺失如实记录阻塞。第二阶段退出后再进入第三阶段。

## 固定约束和测试边界

- Next.js/React/TypeScript + Meta Astryx；Go/SQLite API 与受控 root Agent；静态前端嵌入 Go。部署核心不需要常驻 Node/Python/模型。
- 精简底部导航、主题与共享便签；AI 通过原生 HTTP/stdio MCP 操作。面板不内置聊天或模型选择器。
- 网页、REST、MCP 共用授权、资源、任务、幂等、审计和恢复。完整旧有效功能保留在迁移范围，不能通过删除能力来标记完成。
- 137 组 matrix 和 migration-tracker 的初始 planned 状态不是完成率；依据真实对应证据更新。Source、协议 fixture、root 内核检查、实际安装包、真实服务和浏览器的证据范围分别说明。
- 工具链与依赖版本以 go.mod、package-lock.json 和 setup/build 脚本为准。不要假定云端 `/workspace/.tools` 或 `/workspace/tools` 会出现在本地。
- 云端 QEMU 镜像、私有凭据、转发端口、构建缓存、发行 tar 和原始私有日志不随 Git 克隆。重新建立隔离测试环境，不能使用文档中的旧端口假定连接成功。
- 云端 dev.11 的 QA MariaDB 曾因自启缺陷留下无最终 receipt 的 active/disabled 实例。这是云端残留，不是本地已有服务；没有 journal 证明时不能自动接管或删除。本地禁止直接在日常宿主全局安装插件、修改 SSH/防火墙或覆盖既有服务。

每批完成实现、相应测试与修复后提交到接续分支，更新 PROGRESS 和证据。阶段二仍在进行中，第三阶段未开始。
