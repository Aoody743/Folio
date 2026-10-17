# 给本地 Codex 的接续 prompt

复制下方内容到本地 Codex。仓库与分支已直接写入；先读交接文档，再实际接续开发。

```text
继续开发 Folio（折纸面板）的全局重构，不要重新从零规划。

GitHub 仓库：https://github.com/Aoody743/Folio
当前分支：https://github.com/Aoody743/Folio/tree/main

请获取 main 的最新代码：没有 checkout 时运行 git clone --branch main https://github.com/Aoody743/Folio.git；已有 checkout 时先检查并保留本地修改，再 fetch 并更新 main。不要覆盖未提交的用户修改。

首先完整阅读 docs/rewrite/HANDOFF.md、HANDOFF-VALIDATION.md、PROGRESS.md、DEVELOPMENT-PLAN.md、SCOPE-CORRECTIONS.md，随后阅读 UI-SPEC.md、TECH-STACK.md、AI-NATIVE-MCP.md、IMPLEMENTATION-CONTRACT.md、PHASE2-CONTRACT.md，以及 tests/rewrite/phase2/README.md、acceptance-matrix.json 和 docs/rewrite/migration-tracker.json。这些文件是接续依据；云端 /workspace 的 VM、凭据、产物不会随 Git 迁移，请在本地隔离 Linux 环境重建必要条件。

目标：Next.js + Meta Astryx（https://astryx.atmeta.com/）前端，轻量 Go 后端/SQLite/独立 root Agent，静态前端嵌入单个 Go 发行二进制，Linux 安装脚本。面板精简、快速、低系统修改度；四卡首页、底部五项导航、五套配色；主页便签由人和外部 AI 共用。原生 MCP 支持 HTTP 与 stdio，与 UI 共享真实权限、任务、结果、审计。不要内置 AI 聊天、模型选择或强制模型运行时。保留原项目全部有效功能与 70 个插件。

当前第一阶段已完成并验收，第二阶段开发中，第三阶段尚未开始。最近固定 dev.10 包的真实验证与当前尚未封包复验的后续源码必须区分；部分矩阵状态仍待回填。不得把实现、mock、跳过、交叉构建或历史包通过说成新源码实际验收。

当前用户明确要求先完成代码实现，不运行测试、构建、服务启动、浏览器验收、VM 验收或真实请求；这些工作等目标环境可用后单独进行，不得伪造通过结果。

请使用 subagents 分工，按三个阶段顺序推进：先复现当前分支构建/测试，修复问题；完成第二阶段全部退出标准、测试与修复后才开始第三阶段。以交接文档建议顺序优先闭合 ACME SubmitPrepared 原子提交、网站持久事务/SIGKILL 恢复、数据库工具与 PG 权限修复、托管服务真正 reboot 自启，以及新备份下载复验，再补其余核心能力。第三阶段完成剩余 47 插件、旧版数据迁移、平台/性能/长期运行/安全与正式发行。

严格使用 go.mod 的 Go 1.27.2、Node24、npm ci 和现有 lockfile；运行适当的 Go/race/前端/安装器测试，生成新版本固定发行包后进行实际浏览器、HTTP/MCP、隔离 Linux VM 服务生命周期检查。不要覆盖旧固定包或削弱原失败断言，不要运行旧 Python 全局安装器实现新版功能，不要把 root 服务/防火墙/SSH测试运行在我的普通工作主机。

进度与证据持续提交到 `main`（或用户明确指定的接续分支），每次标明实现、实际通过、失败、跳过与待外部验证。保持 TLS/包签名/哈希校验和普通 API/特权主机执行器权限隔离，不提交凭据。无需重复询问已明确的技术栈和总体范围；遇到前提缺失先完成独立工作并准确记录。最终交付标准仍为三个阶段全部完成并通过验收，代码推送 GitHub，安装/恢复/使用/MCP 文档与实际行为一致。

先报告你从交接文件确认的当前状态和第一个可验收闭环，然后开始实施，不要只回复计划。
```
