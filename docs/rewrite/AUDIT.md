# PowerLinux 首轮审计

审计日期：2026-10-12。仓库：Aoody743/PowerLinux，main。

审计提交：`a0b700753f6242d40dd60b40a4533960b58a3632`。只读 Git 查询确认远端 main 与本地提交一致。

## 结论

项目具备可运行的管理面板和较大的插件功能集合，但不宜直接继承现有安全边界、安装器和插件执行机制来做下一代产品。首要风险是未登录输入进入日志页形成脚本执行链，其次是登录与会话缺陷、更新来源错位和下载信任链缺失。高权限操作与 Web 进程耦合会放大这些问题。

重写有理由，但应先建立功能清单、紧急安全修复和可验证的新架构。语言替换本身不会修复认证缺陷、子进程等待、错误任务状态和系统配置被覆盖的问题。

## 范围和证据边界

- 核心 Flask 请求链路、认证、会话、日志、文件操作、插件入口、任务执行器、升级入口。
- Debian/Ubuntu 安装行为，通用安装/更新脚本，Redis/OpenResty/MySQL 等插件安装机制的代表性抽查。
- 目录统计：70 个插件目录；排除虚拟环境和 Git 元数据后，255 个 Python 文件、395 个 Shell 文件、150 个 JavaScript 文件、18 个 Lua 文件。这是文件清单统计，不代表每个文件都完成了逐行审计。
- 当前云端环境解析出的 78 个 Python 包版本，逐一查询 PyPI 对应版本的安全公告元数据；78 次请求成功。
- 9 项隔离复现：直接提取原函数，使用内存数据库适配器、合成身份和无害输入；日志渲染使用仓库自带 jQuery 与原始渲染函数，在离线 Chromium 中验证。
- 未对公网部署发起攻击，未往实际日志数据库写入脚本，未执行高权限安装器、危险系统命令或插件安装；未修改仓库源代码、测试和依赖声明。
- 没有覆盖全部插件、全部发行版，也没有做生产流量压测。动态复现不等于完整公网利用测试；下述发现明确区分触发前提。

此前云端的 6 项功能烟雾检查通过，只证明核心流程可运行，并不能说明认证、权限和更新机制安全。

## 按优先级排列的发现

P0 表示需要立即处理的发布风险；P1 表示新版本必须解决的高影响缺陷；P2 表示架构或维护问题。优先级同时考虑攻击条件和维护影响，不是 CVSS 评分。

| 编号 | 优先级 | 发现 | 证据和验证 |
| --- | --- | --- | --- |
| F01 | P0 | 未登录用户的登录输入可以形成存储型 XSS | 登录日志来源、原日志渲染函数及离线浏览器执行已验证 |
| F02 | P0 | 会话签名密钥由网卡标识生成 | 原 SECRET_KEY 表达式及隔离 Flask 验签已验证；攻击需获知/推导密钥和有效用户名 |
| F03 | P0 | 快捷登录绕过启用的两步验证 | 原快捷登录函数在合成数据库中验证 |
| F04 | P0 | 未登录用户可用连续错误登录关闭整个面板 | 5 次失败在内存数据库中设置 admin_close=yes |
| F05 | P0 | 安装/更新指向另一个仓库，并关闭下载 TLS 验证 | README、Shell 更新器和 Web 更新器均有直接代码证据 |
| F06 | P1 | 过期会话会被重新续期，下次检查重新通过 | 同一原函数对同一过期会话先返回 False、后返回 True |
| F07 | P1 | 密码使用无盐 MD5，初始密码保存在本地明文文件 | 用户初始化和密码校验代码直接证据 |
| F08 | P1 | Web、插件和系统操作缺乏权限隔离 | API 装饰器统一放行，插件 eval、Shell 拼接、高权限服务启动机制 |
| F09 | P1 | 命令输出可能阻塞执行器，超时不负责清理子进程 | 256 KiB 标准输出在隔离进程中超过 2 秒 watchdog，进程组被测试清理 |
| F10 | P1 | 命令失败仍被标记为任务成功 | 合成失败命令最终记录 status=1 |
| F11 | P1 | 安装侵入系统且更新缺少事务与可靠回滚 | 全局 Shell、locale、防火墙、包清理、cron、覆盖代码等静态证据 |
| F12 | P1 | 当前环境依赖有安全公告命中 | cryptography 41.0.7 与 pyOpenSSL 23.2.0；不是已经验证可远程利用 |
| F13 | P2 | 请求内采样和插件子进程造成额外延迟 | CPU 原采集函数本地约 1 秒；插件同步 run 和线程 join |
| F14 | P2 | main 分支未被现有 CI 触发配置覆盖，缺乏核心断言 | 已有工作流触发 master/dev 或 run_test，主要安装与启动服务 |
| F15 | P2 | 数据、工作目录、UI 和插件契约耦合，难以稳定迁移 | CWD 派生路径、字符串命令、混合响应、拼接 HTML、版本漂移 |

### F01：日志页的存储型 XSS

代码链路：

1. [login.py](/workspace/powerlinux/server/PowerLinux/web/admin/dashboard/login.py:203) 将未登录用户提交的用户名直接拼进错误登录日志。
2. [mw.py](/workspace/powerlinux/server/PowerLinux/web/core/mw.py:983) 格式化后传给数据库日志写入。
3. [thisdb/logs.py](/workspace/powerlinux/server/PowerLinux/web/thisdb/logs.py:21) 原样写入日志字段。
4. [logs.js](/workspace/powerlinux/server/PowerLinux/web/static/app/logs.js:178) 将日志字段直接拼进表格并调用 `.html()`。

复现中，合成用户名仅在离线页面设置一个 DOM 标记，无网络请求或数据外传。使用仓库原 jQuery 和 getLogs 函数时，浏览器确实执行了该标记。未登录来源、持久化代码和危险渲染位置都已确认；未进行真实数据库注入或线上攻击。

影响：管理员查看受污染日志时，脚本在面板页面上下文执行，可借管理员会话调用敏感接口。HttpOnly Cookie 无法阻止脚本代用户发起同源请求。默认根权限部署会进一步放大影响。

修复：日志改成结构化纯文本，前端使用文本节点；若确实需要有限格式，使用明确的安全渲染方案并禁止可执行标记；部署 CSP 作为额外防线。为“未登录输入→日志→管理员查看”建立真实浏览器回归测试。

### F02、F06：会话密钥与过期处理

[admin/__init__.py](/workspace/powerlinux/server/PowerLinux/web/admin/__init__.py:58) 的签名密钥是 `uuid.UUID(int=uuid.getnode()).hex[-12:]`。通常等于 48 位网卡标识的十六进制表示，而不是独立生成的高熵秘密。

隔离验证：用原表达式建立签名器，构造合成已知用户的会话，原 isLogined 接受该会话。它验证了“获知派生密钥及有效用户名后可以伪造会话”的条件性风险；不能据此宣称任意互联网用户都已掌握真实密钥。未读取或输出真实认证密钥。

[common.py](/workspace/powerlinux/server/PowerLinux/web/admin/common.py:29) 在过期时改写 overdue 后返回 False，保留 login 和 username；下一次检查返回 True。请求结束后的会话写回可能使过期状态重新有效。

修复：使用 CSPRNG 生成独立会话秘密，限制访问并支持轮换；过期立即清除认证状态，绝对过期与空闲过期分开；密码修改、退出、禁用账号、MFA 变化均撤销会话。新架构优先服务端会话记录，Cookie 只存高熵不透明标识。配置 Secure、HttpOnly、SameSite 和统一 CSRF 防护。

### F03：快捷登录不经过 MFA

[dashboard.py](/workspace/powerlinux/server/PowerLinux/web/admin/dashboard/dashboard.py:65) 的 GET 快捷登录路径只核对用户名、密码 MD5 和客户端时间，随后直接建立登录状态，没有执行普通登录路径的两步验证检查。

隔离数据库声明 MFA 已启用，原函数仍建立登录状态，甚至没有读取该设置。攻击条件是掌握有效用户名和密码；该问题绕过第二因素，不等同于无密码登录。

时间限制也只拒绝太旧的时间，不拒绝未来时间。载荷包含密码且 Base64 不提供加密；URL 可能进入访问日志、浏览器历史或其他日志系统。

修复：取消携带密码的 URL 登录；所有登录统一进入认证状态机。若保留快捷登录，使用服务端签发的短期、一次性、不透明令牌，并明确 MFA 策略及服务端过期校验。

### F04：失败登录关闭全局面板

[login.py](/workspace/powerlinux/server/PowerLinux/web/admin/dashboard/login.py:180) 使用全局缓存键 login_cache_limit，并在多条失败分支设置持久化 admin_close=yes。未知用户名也触发同样逻辑，不需要猜中管理员账号。

隔离复现中，5 次失败触发整个面板关闭。默认 requestCheck 随后将请求导向关闭页面。该实现将限流变成远程拒绝服务入口；多进程内存缓存还导致限流状态不一致。

修复：按来源、账户及全局负载多层限速，返回限速状态并保持面板可用；不将认证失败写成全局关闭配置；建立恢复路径与审计事件。

### F05：fork 后更新来源仍属于另一个仓库

- [README](/workspace/powerlinux/server/PowerLinux/README.md:34) 的安装/更新/卸载命令指向 AndyXeCM/PowerLinux。
- [install.sh](/workspace/powerlinux/server/PowerLinux/scripts/install.sh:16) 和 [update.sh](/workspace/powerlinux/server/PowerLinux/scripts/update.sh:6) 仍配置相同 owner。
- [Web 更新器](/workspace/powerlinux/server/PowerLinux/web/utils/system/update.py:22) 查询该仓库的 Release，并下载其代码。
- [update.sh](/workspace/powerlinux/server/PowerLinux/scripts/update.sh:209) 用 `--insecure` 下载分支归档，再覆盖 /www/server/mdserver-web。
- [Web 更新器](/workspace/powerlinux/server/PowerLinux/web/utils/system/update.py:202) 用 `--no-check-certificate` 下载归档。
- [Redis 安装器](/workspace/powerlinux/server/PowerLinux/plugins/redis/install.sh:33) 和抽查的 OpenResty 构建脚本也关闭下载 TLS 校验。

影响：用户安装的可能不是当前 fork；升级可能覆盖本人的改动。未经验证的远程代码在高权限执行环境下成为供应链入口。这里没有证据表明第三方仓库或下载源已经遭到入侵。

修复：建立本仓库唯一的发布来源；使用固定版本产物、完整 TLS 验证、来自可信发布元数据的哈希/签名；先验证、再解包、再切换版本。校验和与产物若来自同一不可信通道，不能独立建立信任。发布需支持回滚并保留用户数据。

### F07：口令保存与随机数

[user.py](/workspace/powerlinux/server/PowerLinux/web/thisdb/user.py:18) 使用无盐 MD5 保存密码，将生成的初始密码写到 data/default.pl；[mw.py](/workspace/powerlinux/server/PowerLinux/web/core/mw.py:151) 生成这些字符串使用普通 Random。

MD5 不适合口令存储，获得数据库后可快速离线猜测。明文默认密码文件增大备份、文件接口和插件读文件造成的泄露面。本审计没有输出密码文件内容，也没有证明真实密码已经泄露。

修复：Argon2id，使用加密安全随机源；首次启动通过本地控制台或一次性初始化令牌设定密码，初始化完成后撤销令牌；迁移旧账户时优先要求重设密码，不能把旧 MD5 当作新认证协议的长期凭据。

### F08：插件执行与权限边界

[plugin.py](/workspace/powerlinux/server/PowerLinux/web/utils/plugin.py:878) 拼接并 eval 调用表达式，调用参数来自插件 HTTP 接口。隔离验证在 args 中计算一个无害 lambda，说明输入被解释为 Python 表达式。

[plugin.py](/workspace/powerlinux/server/PowerLinux/web/utils/plugin.py:846) 与 install/uninstall 用未结构化的用户参数构造 Shell 字符串；[文件权限接口](/workspace/powerlinux/server/PowerLinux/web/utils/file.py:736) 也把 access/user 等参数拼到系统命令中。API 装饰器只做统一登录/API 密钥校验，没有资源和动作级权限范围。

这些接口需要登录或有效 API 凭据；管理员本来就可能具备终端能力，因此不能把它们全部称为“未登录 RCE”。真正的问题是缺少可约束的插件、API Token 和 Web 权限边界，认证失陷后影响范围很大。默认 systemd 模板未指定普通用户，SysV 启动也没有降权步骤。

代码没有统一 CSRF Token 校验；Socket.IO 配置允许所有 Origin。浏览器 Cookie 策略会影响跨站攻击是否成立，本轮没有将 CSRF/跨站 WebSocket 风险列为已经完成浏览器利用的独立漏洞。

修复：普通用户运行控制面；独立 Agent 通过受权限控制的 Unix Socket 接收类型化动作；建立动作白名单、授权、审计、超时和参数验证。执行外部程序使用 argv；第三方插件使用独立进程和能力声明，不能继承任意 root Shell。

### F09、F10：执行器和任务状态不可靠

[mw.py](/workspace/powerlinux/server/PowerLinux/web/core/mw.py:31) 先轮询子进程退出，之后才 communicate 读取 stdout/stderr；子进程输出填满管道时可能阻塞，父进程却仍等待其退出。超时分支抛异常，没有终止/回收进程组。

隔离命令只输出 256 KiB 文本，超过 2 秒 watchdog 仍未返回；测试负责终止整个隔离进程组。此处未运行任何安装任务。

[panel_task.py](/workspace/powerlinux/server/PowerLinux/panel_task.py:90) 忽略 execShell 的错误结果，随后直接将任务标为成功。原函数在合成失败场景下仍给出 status=1。命令封装也不返回退出码，难以让调用者正确判定成功。

修复：并发流式消费输出，保留退出码；有界日志、超时、取消、进程组清理；任务建立 queued/running/succeeded/failed/canceled 状态机，持久化结果和重试策略。数据库抢占和资源锁必须原子化，进程恢复不能盲目重跑具有副作用的操作。

### F11：系统侵入和启动副作用

- [Debian 安装器](/workspace/powerlinux/server/PowerLinux/scripts/install/debian.sh:16) 替换系统 /bin/sh。
- 安装器修改 locale，运行 apt autoremove，安装大量构建/插件依赖。
- [防火墙处理](/workspace/powerlinux/server/PowerLinux/scripts/install/debian.sh:105) 启用/安装防火墙并添加规则。
- [通用安装器](/workspace/powerlinux/server/PowerLinux/scripts/install.sh:154) 改写系统 motd。
- [setup.init](/workspace/powerlinux/server/PowerLinux/web/admin/setup/__init__.py:24) 在导入应用时注册初始化脚本、ACME cron 和自动更新任务；不仅是显式安装阶段。
- 更新器直接覆盖程序目录，无完整事务、升级迁移门槛和经过验证的回滚策略。
- Redis 卸载删除软件目录，删除数据是否符合用户预期没有独立策略。

默认 systemd 模板还含有需要重新验证的配置问题，例如将 Shell 风格 `cd web && ...` 直接写入 ExecStart；相关初始化调用当前有注释，不能断言所有部署都由这个模板运行。

修复：核心只管理自己的目录和服务；声明每种能力需要的系统修改，生成变更计划、备份、回滚与卸载清单。核心启动不得触发包安装、cron 改写或自更新。软件卸载与数据删除分别处理；用户自有服务先以只读导入模式接管。

### F12：依赖安全公告与复现性

当前云端环境的 cryptography 41.0.7 和 pyOpenSSL 23.2.0 在 PyPI 元数据中存在未撤回公告，包括 GHSA 记录。完整原始查询结果保存在 [dependency-advisories.json](dependency-advisories.json)。公告记录可能包含别名或重复信息，不将其数量当作独立漏洞数量。

这表示版本匹配，不能单凭扫描断言相关 TLS/加密路径可以通过面板远程利用。项目固定 pyOpenSSL==23.2.0，它的依赖范围限制 cryptography 版本；其他大量依赖只有最低版本，安装结果又容易漂移。

本轮没有升级依赖或改写 requirements.txt。临时维护版应调整版本组合并验证 TLS、SSH、证书与登录路径；新版本建立锁定、SBOM 和漏洞扫描，避免把旧环境约束带入新实现。

### F13–F15：性能、CI 和可维护性

[system/main.py](/workspace/powerlinux/server/PowerLinux/web/utils/system/main.py:180) 默认 cpu_percent(interval=1)；原函数本地测试约一秒，部分系统统计 API 在请求内调用它。该测量是函数延迟，不是完整 HTTP p95。新版应后台采样并缓存，响应只读快照。

插件状态查询同步启动 Python 子进程并 join 线程；缺少统一超时会令慢插件影响整个列表。文件列表、配置写入、任务执行与请求过程混杂，速度问题不只是 Python 解释器。

现有 6 个工作流主要验证系统安装和服务启动，OpenResty 工作流触发 master/dev，其余工作流触发 run_test；main 不在这些触发集合中。未找到覆盖核心安全链路、API 契约、任务故障和迁移回滚的自动化测试。仓库存在插件测试，不能据此称“完全没有测试”。

前端依赖 jQuery/全局函数、字符串 HTML 和插件自带页面；响应结构、版本号与路径规则不一致。路径依赖当前工作目录和 /www 布局，数据迁移与云端开发成本高。

## 建议的立即处理顺序

1. 修复日志渲染；移除密码 URL 登录；重新生成会话秘密并撤销旧会话；修复过期处理和全局关闭逻辑。
2. 统一当前 fork 的安装和发布来源，恢复下载校验；自动更新在可靠回滚建成前改成显式操作。
3. 修复命令执行器和错误任务状态；收紧插件与 API 凭据权限。
4. 调整维护版依赖，补上 main 分支的安全/功能 CI。
5. 建立新版架构和迁移 PoC，再逐个替代旧功能。不要在新版尚无恢复能力时删掉旧实现。

## 复现与原始结果

- [verify_findings.py](verify_findings.py)：隔离函数复现，不访问实际 API 或实际账户。
- [browser_verify.py](browser_verify.py)：离线浏览器，所有网络请求被阻断；使用本仓库 jQuery。
- [verification-results.json](verification-results.json)：9 项结果及采样时延。
- [verification-output.txt](verification-output.txt)：本次执行记录。
- [dependency-advisories.json](dependency-advisories.json)：78 个版本的 PyPI 查询结果。

运行方法：

```bash
cd /workspace/powerlinux/server/PowerLinux
.venv/bin/python /workspace/powerlinux/audit/verify_findings.py
```

浏览器步骤使用环境现有的系统 Python、Playwright 与 Chromium。复现脚本有意确认原实现中的缺陷；它不是新版本应该长期保留的“通过即可发布”测试。修复后，应改成断言安全行为的回归测试。
