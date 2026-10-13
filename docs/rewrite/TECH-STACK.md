# PowerLinux 全新重构：技术栈建议

更新：2026-10-13（Asia/Singapore）。按最新要求，采用全新重构路线，前端使用 Next.js 与 Meta Astryx Design System；旧版紧急修复不作为本次新版开发的前置阶段。

**范围补充：用户要求现有面板的功能新版也全部具备。235 个核心 HTTP 路由声明和全部 70 个插件目录均进入迁移范围，具体行为见 [全量功能清单](FEATURE-PARITY.md)。第一阶段只是交付顺序，不代表总范围缩减。**

## 建议方案

**Next.js + React 19 + TypeScript + Astryx；Go + net/http/chi + 官方 MCP Go SDK；SQLite；安装脚本下载预编译 Linux 发行包。**

**AI Native 是基础能力：Go 原生 MCP 服务支持本地 stdio 与远程 Streamable HTTP，AI 与网页共用业务动作、权限、任务和审计。详细设计见 [AI Native 与 MCP](AI-NATIVE-MCP.md)。**

**UI方向已确定：精简信息面板、底部五项导航、分层卡片、多配色、人/AI共享便签；AI在外部通过MCP接入，面板不内置聊天或模型界面。制作与交互规范见 [UI-SPEC.md](UI-SPEC.md)。本轮只制作文档，不实施UI原型。**

默认将 Next.js 静态导出，Go 提供静态界面、API、认证和任务服务。生产用户的服务器不需要 Node.js、npm、Go 编译器或前端构建工具。宿主系统操作由同一发行包中的独立 Agent 模式处理，按能力启用。

这保留 Next.js 的开发方式和组件生态，并降低生产常驻服务数量。静态界面仍可以通过 API 实时展示主机数据、操作软件和查看任务进度。

## 前端与 Astryx 的实际兼容信息

已读取 Meta 官方 GitHub 仓库及已发布 npm 包信息：

- Astryx 官方说明当前仍处于 Beta。
- `@astryxdesign/core` 要求 React/React DOM >=19，以及 StyleX peer dependency。
- 本次查询 npm latest：Astryx core 0.6.8、Next.js 16.4.0；这是查询结果，不代表已经验证此组合的生产构建。
- 已发布 Astryx 包包含 reset.css、astryx.css，并在 README 中提供 Next.js 的 CSS/Theme/LinkProvider 接入示例。检查下载包时校验了 npm integrity。
- 常规消费预编译 CSS/JS 不需要配置额外 Babel/PostCSS 构建插件；依赖仍按官方要求安装。
- 使用 `@astryxdesign/cli` 查询组件、模板和 API。不要把未安装 scoped CLI 时的裸 `npx astryx` 当作官方 CLI。

建议初始依赖：Next.js、React 19、TypeScript、`@astryxdesign/core`、`@astryxdesign/theme-neutral`、`@stylexjs/stylex`，开发依赖使用 scoped Astryx CLI。

包管理器建议 pnpm，提交锁文件并使用 frozen-lockfile 安装。兼容 PoC 通过后再固定具体版本；Beta 组件升级单独验证，避免自动跟随 latest。

界面共用 Astryx 的导航、主题、表单、按钮、表格、弹窗和反馈机制；自己建立主机、软件、网站、文件、任务等业务页面。通过 token/CSS Modules 定制品牌，初版无需额外叠加一整套 UI 系统。

API 类型由 OpenAPI 生成 TypeScript；数据缓存与失效可采用 TanStack Query。复杂图表、终端和编辑器按页面延迟加载，不在首屏打包全部功能。图表方案在首个监控页面依据大小和交互需求选择；Astryx 官方当前的图表包只有 canary 标记，不当作稳定基础依赖。

## Next.js 的生产部署选择

| 模式 | 生产常驻组件 | 能力与限制 | 适用判断 |
| --- | --- | --- | --- |
| 静态导出 `output: 'export'` | Go API，按需启用独立 Agent | 页面构建期生成，运行时数据通过 Go API 获取；无运行时 SSR、Server Actions、服务端 Cookie API等 | 轻量 Linux 面板默认方案 |
| `output: 'standalone'` | Node.js Next 服务、Go API、按需 Agent | 可使用运行时 SSR/Server Actions 等完整 Next 服务能力；增加 Node 运行时、进程和升级维护 | 明确需要 Next 服务端功能时采用 |

静态导出模式下：

- Go 负责认证、会话、权限和敏感数据；前端路由或隐藏菜单不能替代后端鉴权。
- `/api/v1`、任务 SSE 和终端 WebSocket 与页面同源，由 Go 路由统一处理，部署不依赖 Next rewrites 或额外代理。
- 服务、网站、任务等运行时未知 ID，使用固定详情页面与客户端参数或内部视图导航；不能依赖构建时未知的 Next 动态路由。
- 使用运行时浏览器 API 的组件放在正确的 client boundary，静态构建涉及 search params 时处理 Suspense。
- 静态页面可以包含构建期 Server Components；这里限制的是请求期动态服务端能力。
- 页面资源公开可下载，主机数据和操作由 API 鉴权。敏感信息不得写入构建产物。

管理面板通常不依赖 SEO 和请求期 HTML 个性化，因此静态界面加实时 API 是合适默认。仍需实际构建 PoC，验证 Astryx Provider、页面导航、静态详情路由和资源路径。

## Go 后端

| 部分 | 选型 | 用途 |
| --- | --- | --- |
| HTTP | Go net/http + chi | 少量基础设施，标准库兼容的中间件与路由 |
| 数据 | SQLite + 显式 migration | 配置、账户、任务、审计；不额外部署数据库服务 |
| 指标 | gopsutil / 必要的 Linux 原生读取 | 后台周期采样，接口读取缓存快照 |
| 长任务 | 内置持久化执行器 | 有界并发、资源锁、退出码、进度、取消和恢复 |
| 实时数据 | SSE；终端用 WebSocket | 任务日志/进度和交互终端分开处理 |
| 系统操作 | 独立 Agent，Unix Socket | 类型化动作、权限校验和审计，高权限不进入 Web 请求处理器 |
| API 契约 | OpenAPI | Go/前端共享契约、生成客户端及接口测试 |
| AI Native | 官方 MCP Go SDK，stdio/Streamable HTTP | 工具、资源、流程模板；与 REST 共用动作和授权层 |

Redis、消息队列和微服务不作为初始必需组件。数据量或多节点需求出现后，通过明确容量和一致性要求决定是否引入。

SQLite 使用 WAL、适当 busy timeout 和短事务。驱动的纯 Go/CGO 选择通过多架构发行 PoC 确认，不能只因“单文件”需求忽略内存、编译和平台代价。

外部命令使用 argv，流式读取输出，保留退出码并设置超时/取消；禁止把插件参数直接拼成任意 Shell。插件生命周期接口统一，但特有能力通过 manifest 显式声明。

API 进程使用专门普通用户。Agent 可以是同一个可执行文件的独立子命令和服务，必要时授予更高权限。它只接受受控动作，不提供无限制 root exec。

## 根据现有完整功能补充的组件

| 现有能力 | 前端/协议 | 后端实现方向 |
| --- | --- | --- |
| 本地与远程终端 | xterm.js，认证 WebSocket，按需加载 | Linux PTY/SSH适配器，会话权限、窗口尺寸、断线和撤销 |
| 文件编辑与上传下载 | CodeMirror 6，分段/流式传输；大列表虚拟化 | 受控文件根、权限所有者、回收站、归档任务、大小与超时限制 |
| 历史与实时监控 | 轻量图表，例如 uPlot；API缓存与SSE | 后台CPU/网络/磁盘采样和历史存储，读取快照 |
| 网站、PHP、代理与WAF | Astryx表单、配置编辑、任务反馈 | OpenResty/PHP/插件资源适配器；语法校验、备份、原子写入和reload |
| 多种数据库与主从同步 | 统一数据库界面，任务/日志流 | 对应Go驱动或固定argv调用官方工具；备份导入、binlog、主从与SSH同步分别实现 |
| TLS、ACME与DNS API | 证书和任务界面 | Go ACME库及DNS提供商适配器；逐个核对当前提供商能力 |
| Docker管理 | 容器、镜像、仓库、日志与终端界面 | Docker API；拉取/导入导出/exec等保留，Socket权限明确隔离 |
| FTP/Google Drive/OneDrive与迁移 | 存储连接和迁移界面 | 对应协议/HTTP/OAuth适配器，凭据安全存储及后台传输任务 |
| 计划任务、通知、SSH与防火墙 | 统一调度/配置/反馈 | 自有调度器、SMTP/Telegram适配器、声明式宿主动作与恢复保护 |
| 其他现有软件与应用插件 | 统一入口、生命周期和特有能力页面 | manifest、独立运行进程、官方服务API或固定工具适配；全部插件登记迁移状态 |

以上组件均可由客户端页面与 Go API 组合实现，没有从现有功能中发现必须使用 Next 请求期 SSR 的能力。这个判断是架构可行性分析，不是已经完成全量功能验证。

功能兼容以用户行为和操作结果验收，新版统一定义 API 和插件协议；保留能力不意味着保留 eval、未经验证的root脚本、登录漏洞或错误结果。

MySQL/PHP/OpenResty/Docker/Ollama等受管服务仍采用各自的软件和运行时，Go负责安装、配置、调度和管理。少数应用插件需要Python/Node等运行时，应作为对应插件的按需依赖；不能承诺启用全部插件后的整机只运行Go、或所有软件合计仍占极少内存。

全部管理能力进入范围，第三方软件由用户选择时安装。核心安装器部署面板、Agent与功能目录，从而保持基础安装轻量。防火墙/SSH/系统优化等修改能力在使用时按权限和变更策略执行。

## 发行与安装脚本

云端 CI 先构建静态前端，再构建/打包 Go；可以将静态资源 go:embed 进入发行二进制。初期支持 Linux amd64/arm64，选定 Debian/Ubuntu 验收；扩大发行版支持必须补充测试。

`install.sh` 的职责：

1. 检测 OS、架构、权限、当前安装和磁盘条件。
2. 获取指定版本的可信发行元数据，下载对应预编译包；完整 TLS 校验及产物签名/哈希验证。
3. 创建自有用户、程序/配置/状态目录，保留已有用户数据。
4. 在支持的系统上安装自有 systemd 服务；不支持的 init 系统明确检测和说明，不能仅因能下载二进制就宣称安装成功。
5. 执行初始化及健康检查，通过短期初始化方式创建管理员，避免共享默认密码。
6. 失败时停止本次创建的进程并回滚本次变更，输出具体原因。

目标机器不拉源码、不执行 npm install、不在用户机器编译整套工程。Go、Node 和 pnpm 只在开发/构建环境中存在。

默认不替换系统 Shell，不改 locale/motd，不启用防火墙，不重写 root cron。网站、数据库、防火墙等功能按需声明系统修改和数据路径。卸载程序默认保留用户数据；删除数据另行操作。

更新使用验证后的版本目录和原子切换，数据库 migration 配套备份/回滚策略。降低系统修改度不能意味着取消必要的升级和身份验证措施。

## 为什么这套方案有利于轻量和响应

- 生产无常驻 Node 前端进程，静态文件可缓存/压缩，核心使用少量 Go 进程。
- 监控接口读取后台快照，避免每个请求等待一秒采样。
- 查询不依赖“启动一个插件解释器再等待它退出”。
- 长操作作为任务提交，UI 快速收到任务标识；实际操作的完成时间单独展示。
- 前端按路由加载图表/编辑器/终端，控制首屏 JS 与渲染量。轻量同时关注宿主资源和浏览器资源。

没有实际新实现的基准测试，目前不能承诺具体内存占用或倍数提升。初版可沿用 2 vCPU/2 GiB 基准机及缓存只读 API p95<100ms 的目标，再用固定并发、数据量和采样频率测量；任务提交速度不替代安装/备份耗时。

## 三阶段开发安排

当前以 [DEVELOPMENT-PLAN.md](DEVELOPMENT-PLAN.md) 为执行计划：

1. 基础可用版：UI、五套主题、共享便签、认证/任务/审计、真实状态、原生MCP和核心安装包。
2. 核心管理版：文件/终端、网站/PHP/TLS、数据库/Docker、系统管理及23个核心插件，网页与MCP同步验收。
3. 全功能正式版：其余47插件、全量能力核对、旧版迁移、性能/宿主矩阵、可靠升级/回滚和发布。

后续直到全量清单完成：每个旧功能和有效插件动作都有新界面/API/MCP/Agent映射及验收记录；版本、平台、外部服务和凭据限制明确登记。有阻塞时不能将项目标为“全功能重构完成”，也不能静默删除功能。

旧版仅作为功能与迁移参考。本次没有执行旧版修复，也没有建立新版工程或安装新版工具链；技术栈文档是待实施的建议方案。

## 信息来源

- [Astryx 官方仓库](https://github.com/facebook/astryx)
- [Astryx 核心包的 Next.js 示例](https://github.com/facebook/astryx/blob/main/packages/core/README.md)
- [Astryx npm 包](https://www.npmjs.com/package/@astryxdesign/core)
- [Next.js 静态导出](https://nextjs.org/docs/app/guides/static-exports)
- [Next.js standalone 输出](https://nextjs.org/docs/app/api-reference/config/next-config-js/output)

官网页面的直接网络访问受限；本轮使用官方 GitHub 文档与 npm 已发布包进行核查。Next 文档通过其官方源码仓库读取。未验证完整组合构建。
