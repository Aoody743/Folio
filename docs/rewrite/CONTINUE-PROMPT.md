# 给本地 Codex 的接续 prompt

复制下方内容到本地 Codex。仓库与分支已直接写入；先读交接文档，再实际接续开发。

```text
继续开发 Folio（折纸面板）的全局重构，不要重新从零规划。

GitHub 仓库：https://github.com/Aoody743/Folio
当前分支：https://github.com/Aoody743/Folio/tree/main

请获取 main 的最新代码：没有 checkout 时运行 git clone --branch main https://github.com/Aoody743/Folio.git；已有 checkout 时先检查并保留本地修改，再 fetch 并更新 main。不要覆盖未提交的用户修改。

首先完整阅读 docs/rewrite/LAUNCH-READINESS-20261018.md、HANDOFF.md、PHASE2-CLOSEOUT-20261015.md、HANDOFF-VALIDATION.md、PROGRESS.md、DEVELOPMENT-PLAN.md 和 SCOPE-CORRECTIONS.md，随后阅读 UI-SPEC.md、TECH-STACK.md、AI-NATIVE-MCP.md、IMPLEMENTATION-CONTRACT.md、PHASE2-CONTRACT.md，以及 tests/rewrite/phase2/README.md、acceptance-matrix.json 和 docs/rewrite/migration-tracker.json。这些文件是接续依据；云端 /workspace 的 VM、凭据、产物不会随 Git 迁移，请在本地隔离 Linux 环境重建必要条件。当前唯一接续基线是 `main`；`handoff/pre-closeout-wip-20261016` 是供比对的旧 WIP 快照，不要从它发布或整体合并。

目标：Next.js + Meta Astryx（https://astryx.atmeta.com/）前端，轻量 Go 后端/SQLite/独立 root Agent，静态前端嵌入单个 Go 发行二进制，Linux 安装脚本。面板精简、快速、低系统修改度；四卡首页、底部五项导航、五套配色；主页便签由人和外部 AI 共用。原生 MCP 支持 HTTP 与 stdio，与 UI 共享真实权限、任务、结果、审计。不要内置 AI 聊天、模型选择或强制模型运行时。当前核心范围与暂缓项按上线前差距清单执行；不要把旧版 70 插件清单当成本轮首发阻塞。

第一阶段已完成并验收，第二阶段核心版仍未完成，第三阶段延后。首发目标是全新 Debian 13 amd64 与 Ubuntu 24.04 amd64；当前主线没有完成这两个目标系统的最终原生验收和发布冻结。不得把实现、mock、跳过、交叉构建或历史包通过说成当前源码实际验收。

本轮目标是完成核心版上线前的实现与验收。按上线前差距清单执行适当源码测试、构建，并在隔离的 Debian/Ubuntu amd64 VM 完成原生服务验收；若环境或凭据缺失，继续无依赖工作并将该项准确标为未验收/受阻，不得伪造通过。

依照上线前差距清单依序完成 G0–G5：先固定当前 `main` 源码和候选包，再关闭 PostgreSQL 慢日志、双系统网站栈/数据库、网页与 MCP 权限/任务/审计，以及生命周期/恢复证据，最后冻结发布。Next.js/Go 托管是可选扩展；若决定首发对外提供，先补齐它的原生和权限验收。47 个第三阶段插件、旧版数据迁移及清单中列明的暂缓项不阻塞当前核心版。

严格使用 go.mod 的 Go 1.27.2、Node 24、npm ci 和现有 lockfile；运行适当的 Go/race/前端/安装器测试，生成新版本固定候选包后进行实际浏览器、HTTP/MCP、隔离 Linux VM 服务生命周期检查。不要覆盖旧固定包或削弱原失败断言，不要运行旧 Python 全局安装器实现新版功能，不要把 root 服务/防火墙/SSH 测试运行在日常工作主机。

进度与证据持续提交到 `main`（或用户明确指定的接续分支），每次标明实现、实际通过、失败、跳过与待外部验证。保持 TLS/包签名/哈希校验和普通 API/特权主机执行器权限隔离，不提交凭据。无需重复询问已明确的技术栈和总体范围；遇到前提缺失先完成独立工作并准确记录。当前交付标准是核心版 G0–G5 全部通过并有可复现证据；第三阶段按用户以后重新确定的范围规划，不阻塞本轮首发。

先报告你从交接文件确认的当前状态和第一个可验收闭环，然后开始实施，不要只回复计划。
```
