# refine 交接源码验证

日期：2026-10-13（Asia/Singapore）。本记录是当前开发源码的收尾检查，不是第二/三阶段退出验收，也不是正式发行说明。

环境：普通用户 Linux 云工作区，Go 1.27.2、Node24，CPU 配额4核、32 GiB内存。原生 root/systemd/供应商检查的前提与证据分别保留。

## 实际执行结果

| 检查 | 结果 | 可携带证据 |
| --- | --- | --- |
| 全量 Go 普通测试，`go test -p 1 ./...` | 13 个有测试包通过；另 2 个包无测试 | [go-normal](evidence/handoff-20261013/go-normal.txt) |
| 全量 Go race，`CGO_ENABLED=1 go test -p 1 -race -count=1 ./...` | 13 个有测试包通过；root VM 条件用例在普通用户下跳过，不能当成 native 通过 | [go-race](evidence/handoff-20261013/go-race.txt) |
| 嵌套 MySQL 驱动 race | 通过 | [mysql-race](evidence/handoff-20261013/mysql-race.txt) |
| 前端 TypeScript | 通过 | [frontend-typecheck](evidence/handoff-20261013/frontend-typecheck.txt) |
| 前端测试 | 17/17 通过，包括新增 SQL/Redis 精度与 HTTP 序列化；部分使用协议 mock，不代表实际数据库/浏览器验收 | [frontend-tests](evidence/handoff-20261013/frontend-tests.txt) |
| 安装器测试 | 31 项执行：29 通过、2 个 root VM 用例跳过 | [installer](evidence/handoff-20261013/installer.txt) |
| Next.js 生产静态导出 + CGO-free Linux amd64/arm64 构建 | 通过 | [build](evidence/handoff-20261013/build.txt) |
| `0.1.0-handoff.1` 实际 amd64 包 | 独立 prefix 安装/重复安装、HTTP health/session、嵌入静态脚本/CSP、运行锁、SIGTERM、重装、保留数据卸载通过；非 systemd VM 检查 | [package-smoke](evidence/handoff-20261013/package-smoke.txt) |
| 新网站备份追加 race 检查 | Range 边界和双 scope/精确资源两项通过；三个 root 用例跳过（含原恢复权限组）。新的真实 root FD/Unix→HTTP/MCP 两组尚待在隔离 VM 执行 | [backup-stream](evidence/handoff-20261013/backup-stream.txt) |
| Go vet、模块校验 | 通过 | [vet](evidence/handoff-20261013/vet.txt)、[mod-verify](evidence/handoff-20261013/mod-verify.txt) |

构建在工作树尚未提交时完成；BUILDINFO 明确 `working_tree_dirty=true`，基线 commit 为 `a0b700753f6242d40dd60b40a4533960b58a3632`。当前代码的可追踪文件摘要另存于 [handoff-source-manifest](evidence/handoff-20261013/handoff-source-manifest.json)；构建后增补仅测试/交接材料不改变产品行为。包未上传 GitHub Release，也不随源码提交到 Git。

`0.1.0-handoff.1` 两架构 tar 的 SHA256 已实际校验：

```text
2d3f02ca60f89ab8f88135fa2159d99848ffffcd93426f3d8420788d377ffc9e  powerlinux-0.1.0-handoff.1-linux-amd64.tar.gz
ab2e665ccc8b46ebe31caca7515b7018a79a72c7773a6f1cdc3e09ec0285866e  powerlinux-0.1.0-handoff.1-linux-arm64.tar.gz
```

arm64 是交叉构建，没有实际运行通过声明。重复构建的 commit/工具链/时间记录会变化，不能要求从提交后的源码重建相同 tar 摘要。

## 未执行与未完成

- 新源码的 managed MariaDB/PostgreSQL 生命周期与 Web/PHP 真正 reboot 自启仍待新固定包复验，历史真实失败保留。
- 新备份 root FD/实际 Unix/HTTP/MCP 核查已写测试但未在 VM 执行；备份网页下载按钮尚未接。
- 网站完整持久 WAL/启动恢复未接入；ACME producer 未使用新的 SubmitPrepared。停在源码可构建状态，没有把未集成原型放入运行模块。
- 系统操作 native VM harness 已准备，本轮交接没有启动它；不新增系统操作通过记录。Ubuntu 最后完成的是 dev.10 升级六组，原策略、API/Agent 健康及便签已由 QA 交回。数据库 guest 部分安装失败状态仍需独立重建/复验，不作为干净 fixture 随 Git 交接。
- 全部 23 核心插件、137 组语义验收、第三阶段 47 插件、迁移、外部供应商、原生 arm64、多平台、性能与长期运行仍未达到完整目标。详见 [HANDOFF](HANDOFF.md)。

Git 提交排除了 node_modules、Next 导出/缓存、私有数据、VM磁盘、二进制、原始凭据。第一方差异空白检查通过；`third_party/mysql/LICENSE` 保留上游原样空白，其第38行尾空格不是产品代码问题。此前日志仅引用原云路径者表示来源，不能在本地视为可读取文件；本目录追加的精选记录可以直接从 Git 获取。
