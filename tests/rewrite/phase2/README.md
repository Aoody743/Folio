第二阶段以真实用户能力验收，`acceptance-matrix.json` 保留235条旧路由的一对一主归属、23插件的版本来源差异、动态脚本与Socket.IO来源，共137组。生成器读取原源码和迁移索引，并断言无遗漏、无重复。动作字符串仅作为溯源索引，不代表实现或通过的功能数。

运行 `python3 tests/rewrite/phase2/build_matrix.py` 可重建初始 planned 清单。实际证据应另行保留，包含真实发行包版本/SHA、命令、时间、断言和日志路径；不覆盖生成器的待验收基线。`planned`、`implemented`、`verified`、`unrun`、`blocked` 必须区别记录。任何 canned 状态、替代产品或 mock 外部凭据都不能计作真实插件通过。

首组测试覆盖已组合入 CLI 的文件模块：REST/MCP同一权限引擎、哈希冲突、文本编码与历史、真实复制移动、权限、回收恢复、安全压缩解压、大文件 HTTP 流与有界 MCP 分块、撤销及审计。文件测试通过不等于其他插件或 UI 已迁移。

dev.7实际包（SHA见files-dev7-evidence.json）通过原12组文件API及原浏览器流程，但强化检查发现保存后实际44字节仍显示0B。dev.8真实包的当前14组文件API为12组通过、回收删除与默认设置两组失败；浏览器通过字节数、冲突草稿、手动合并、断线重试及历史恢复，随后4,000行CodeMirror输入挂起。原失败及快照保留在files-dev8-evidence.json。修复后的dev.9不可变包已完整通过14组API及原强化浏览器全部流程，包括SQLite默认回收设置、批量预检、4,000行真实输入、单次UTF8 PUT→GB18030实际80,000字节与78.1KiB显示、历史恢复新human版本、5MiB中断续传/下载SHA及四种响应宽度；证据在files-dev9-evidence.json。实际跨filesystem EXDEV、API/Agent跨端与额外原生归档格式仍需各自证据，不由上述14组自动计为通过。

`test_databases.py`已对dev.8实际发行包及MariaDB/PostgreSQL/MongoDB/Redis/Memcached目标完整执行10组，全部通过，凭据仅由checkout外0600 JSON提供。首次审计查询使用超出契约的limit=200，修正为两页各100条，保留200条覆盖及原失败日志；证据在databases-dev8-evidence.json。此套只验证真实驱动、数据操作与身份权限，不算软件安装、备份恢复、复制或Oracle MySQL通过。

`vm/test_agent_install.py`已对dev.8实际包在Ubuntu24.04 systemd QEMU VM完成干净安装、重复安装、四项vendor/FIFO单元拒绝、普通API/独立terminal/root Agent身份、只读策略检查、真实SO_PEERCRED拒绝、全局文件保留与便签/重启持久性。`test_terminal_vm.py`三组全部通过，验证真实PTY、OpenSSH、REST/HTTP MCP/stdio/WebSocket同会话、分享、CSRF/Origin和撤销。SSH输入回显误匹配及另一处审计分页的测试问题已修正，原日志保留；证据在agent-terminal-dev8-evidence.json。升级失败、手动回滚、重启兼容和23插件生命周期仍待后续真实包测试。

sites-native-evidence.json保留严格本地HTTPS ACME/manual DNS协议、实际root文件事务和真实PHP CLI的独立测试结果。本地CA与Check/Reload回调不是外部CA、DNS供应商或实际OpenResty/FPM进程验收。后续dev.9原生测试二进制的四项root证书/配置/保留内容事务也全部通过，artifact/log在/workspace/tools/qa-ubuntu/guest/sites-native-dev9-r2-artifact-20261013.json；同样保持该边界，不与dev.8生产包证据混用。137组生成矩阵维持planned初始清单，另存的真实证据不会自动提升所有能力。

`test_file_editor_regressions.py`另对dev.9实际包执行三项增补检查，全部真实失败：GB18030小文本写盘正确，但保存触发的自动读取仍按UTF8并报错；成功保存和显式放弃后，已解决的草稿切换提示仍然显示。原完整浏览器通过记录不变；新增失败截图、日志及未执行的后续断言记录在file-editor-dev9-regressions-evidence.json，等待下一不可变发行包按相同断言复测。

Debian13实际kernel测试二进制已通过两项EXDEV组：确认/tmp与/dev/shm设备号不同及rename真实EXDEV，验证树UID/GID/mode、回收恢复、危险链接失败回滚、64MiB复制中途取消和lease排空，独立证据在files-kernel-dev10-evidence.json。这是原生Service/kernel证明，不能代替发行包REST/MCP跨端集成。共享的非0UID、主GID0临时用户也真实触发terminal和sysops纯策略/constructor拒绝，开启root选项仍拒绝且未创建state目录；两项通过和统一删除后的absence证明在gid0-profiles-dev10-evidence.json，不算发行包权限门禁已复测。

sites-native-dev10-evidence.json另记录Debian实际root测试二进制的两项通过：默认网站切换要求双方版本并在Check/Reload失败时共同恢复配置与receipt；网站备份恢复在umask0077下保留0550嵌套目录、0640 CSS、真实字节及隔离UID/GID。Check/Reload仍为fixture，尚不能作为实际OpenResty默认站点或HTTP静态资源证明。

sites-native-dns-dev10-r2-evidence.json记录三项独立原生DNS协议测试的实际结果：真实HTTPS ACME+签名TXT私有证书导入、独立context取消清理通过；LostCleanup重启恢复失败，报告Knot缺少显式update endpoints。已保留失败日志并交Root修复，未改门槛。DNS/CA均为隔离可信协议fixture，不代表公开DNS供应商或外部证书发行验证。

Root定位r2为fixture给ISPConfig提供空trusted allowlist、constructor正确拒绝；修正仅配置非空provider列表的r4二进制实际重跑原三项，全部通过，并加入凭据revision固定、公开order.input无秘密、Close后New真实重读account/receipt断言。sites-native-dns-dev10-r4-evidence.json另存实际证明，原r2失败保留；未作为最终发行包或外部DNS验证。

本地与远程终端必须验证真实 PTY/SSH/WebSocket/MCP同会话。生产本地终端采用独立系统 UID/GID，不能访问面板私有运行目录、SQLite、加密密钥或 Agent 策略；同 UID 测试配置不能计作该隔离通过。SSH始终校验服务端主机密钥。

数据库适配器测试使用真实独立服务。预装 Debian MariaDB/PostgreSQL/Redis/Memcached只提供协议与数据测试目标，不算软件生命周期通过。MySQL、MongoDB、OpenResty必须是真实产品。普通数据库令牌使用实际数据库最小权限和结构化查询；进程终止、复制、配置、备份或任意 SQL 控制台要求单独的实例管理权限。

真实Debian13与Ubuntu24.04 QEMU VM可复用 `tests/rewrite/vm` 工具，Ubuntu基础Agent/终端生产闭环已执行。升级/失败恢复、服务账户/特权Agent、防火墙/SSH及重启只能操作隔离VM，并由Root串行分配写入时段。共享宿主的shell、用户、Docker权限、网络、系统服务均不得作为面板操作目标。TCG VM性能与宿主HTTP20并发/200请求性能分别报告；ARM交叉构建或TCG不能替代native arm64。真实外部DNS/公开CA/SMTP/Telegram凭据未提供时保留阻塞，不标记完成。预装服务/SSH/nft fixture在platform-fixtures-evidence.md，仅作为协议或测试准备证据。

Debian已部署实际dev.9/v2 Agent软件测试基线，正常bootstrap初始化新私有账号、API与普通UID Unix Agent health及真实便签全部通过。由于P1随机密码未保存，已授权把完整旧数据同filesystem原地rename保留inode/owner/mode/SHA并加入保护路径，未改SQLite、密码或MFA；不称为旧账号连续升级通过。新dev.9数据会连续用于下一包升级。debian-agent-dev9-baseline-evidence.json保留证明和旧数据复位计划。软件源暂经固定官方Debian HTTPS与strictSSH loopback镜像传输，实际stock APT签名/哈希检查通过；原源配置完整备份，传输与恢复前置条件明确记录，不能当作已完成任何插件生命周期。

`vm/test_agent_upgrade.py`已对实际dev.10包在Ubuntu24.04独占VM完整通过六组：真实flock拒绝且DB/备份/inode不变、candidate-only实际端口冲突的pre-Agent原策略恢复、SIGKILL后的durable journal与真实旧健康恢复、post-Agent身份拒绝保留最新策略/ledger并停止Agent标记恢复、成功升级，以及手动回滚恢复升级前便签快照。最后已重新实际升级到dev.10，核对部署二进制SHA与API/普通UID Agent版本/策略health，pending/marker/drop-in均清除。候选包和health字节未改；首轮QA脚本读私有VERSION失败，修正仅调用原二进制version后按原断言全部重跑，原失败及恢复保留。正式证据在agent-upgrade-dev10-evidence.json，首轮分类在agent-upgrade-dev10-r1-evidence.json；Ubuntu已交给release串行系统操作验收，不代表托管软件重启失败已修复或整个第二阶段通过。

`test_agent_files_vm.py`已对Debian真实dev.9 API+Root Agent完整执行四组，全部通过：两端复制移动的字节/UID/GID/冲突与预授权、0770/0500/0440树往返及危险链接无发布、显式files:host与私有路径边界、3MiB真实HTTP Agent上传/full/HEAD/Range及有界MCP、外部actor历史、回收恢复、Agent重启和撤销。首轮在Type=simple重启返回后立即读取遇到尚未ready的404；修正等待真实普通UID Agent health后按原全部断言重跑通过，原日志与独立稍后数据正确证明保留。r1/r2自有prefix、trash和令牌均已清理，证据在agent-files-dev9-evidence.json；不代表软件生命周期或全部阶段完成。

实际dev.10不可变包（archive SHA01a5fd1a…81aa8，完整manifest JSON SHA94f99ba2…1a46f）已按原门槛完整通过14组文件API和强化浏览器，并按dev.9失败时相同脚本/SHA通过三项增补编辑器回归：小文本GB18030保存后的自动读取及重开正确，成功保存和显式放弃后已解决的草稿提示消失。实际日志与截图分别在files-dev10-evidence.json和file-editor-dev10-regressions-evidence.json；不代表其他模块或整个第二阶段通过。

sites-recovery-dev10-evidence.json另记录Debian真正root执行的六组独立测试全部通过：严格本地HTTPS CA签发后下载中断恢复、不重复finalize、TSIG DNS取消/清理和重启持久恢复，以及网站late-metadata删除与retained-purge的真实文件事务恢复。临时隔离worker清理及前后生产版本/策略/服务状态保持已核对；这仍是受信协议/文件fixture证明，不能计为外部DNS/CA或实际OpenResty服务通过。

Ubuntu升级测试资源已安全扩盘：保留完整原overlay，离线真实qcow2检查通过，虚拟盘增加8GiB，由stock cloud-init在实际重启时扩分区/文件系统；原SSH主机密钥、API加密密钥、策略、配置、账号和便签连续性全部保留。此次实际重启发现两个托管OpenResty/PHP unit为disabled并未自动启动，API/Agent正常。产品source已确认只start未enable；真实typed start仅用于恢复下一组测试前置条件，原失败保留在ubuntu-resource-reboot-dev9-evidence.json，等待dev.11修复后重新实际重启验收。
