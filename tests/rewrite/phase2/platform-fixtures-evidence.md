2026-10-12：第二阶段实际测试目标准备情况

Debian13 amd64目标复用阶段一的真实QEMU TCG guest。PID1为systemd，SSH仅转发宿主127.0.0.1:19022，校验预先固定的主机密钥。仅操作该自有guest，未安装宿主软件包或修改宿主用户、网络、系统服务。

MariaDB、PostgreSQL、Redis、Memcached通过官方签名Debian trixie仓库下载。58个.deb再次与已验证Packages索引的SHA256逐个比较后，通过SSH传到guest，核完整bundle SHA256，再离线APT实际安装。第一次在/run解包因tmpfs容量失败；第二次磁盘解包后发现该最小镜像没有APT archives目录。修正QA临时目录后安装完整exit0，没有跳过TLS、InRelease签名或包校验。

实际协议检查结果：MariaDB SELECT VERSION()返回11.8.6-MariaDB-0+deb13u1；以postgres系统用户查询PostgreSQL返回17.11；Redis PING返回PONG（包8.0.2）；Memcached TCP VERSION返回1.6.38。四个systemd服务均active。证据保存在/workspace/tools/qa-vm/guest/phase2-db-ready.log；全部包/version/hash索引为phase2-db-packages.json。预装服务仅用于数据库适配器真实数据测试，不代表PowerLinux软件安装/升级/回滚或23插件生命周期通过。适配器连接及权限测试尚未执行。

Ubuntu24.04.5 LTS amd64目标已真正启动到systemd。官方noble cloud image manifest及其GPG签名均下载；使用Ubuntu官方HTTPS archive的ubuntu-keyring_2026.08.18_all.deb内cloudimage keyring验证签名。VALIDSIG指纹为D2EB44626FDDC30B513D5BB71A5D6C4C7DB87C81，keyring包SHA256为fec10bd81d9ce809a5c11c6227a367611dd0e2589afebb41d46aefb350be8f40。镜像下载首次被截断，Range续传后整文件SHA256=6a81c37564db9b1ee84e141922625e1d7c5b389b99bb3c572e0243607d5bb4d2，与签名manifest一致；625612288字节。

Ubuntu使用独立qcow2 overlay与私有NoCloud seed，预置QA公钥和固定SSH主机密钥。密钥、seed、user-data均只保存在checkout外私有目录，未进入仓库或输出日志。启动脚本tests/rewrite/vm/start-ubuntu-vm.sh以TCG2vCPU/1GiB执行；host只开放loopback19023 SSH。第一次两次SSH coldboot连接超时保留为尚未就绪的结果；在sshd实际socket与cloud-init阶段完成后，严格主机密钥的真实SSH确认PID1systemd、ubuntu24.04、ssh active、virtualization qemu。随后cloud-init status --wait返回done，curl/tar/sha256sum/getent/useradd均真实存在。readiness.log、cloud-init-readiness.log在/workspace/tools/qa-ubuntu/guest。该guest尚未安装PowerLinux，安装/Agent/PTY/升级测试仍待新的完整发行包。

宿主是x86_64且没有/dev/kvm。这里的amd64 VM也使用TCG；ARM64 QEMU只能形成CPU模拟的真实Linux guest，不能证明原生ARM硬件性能/执行。原生ARM64 runner尚未提供或执行。

远程HTTPS下载正向路径有明确环境限制：平台HTTPS代理访问官方cloud镜像URL为200；同宿主curl --noproxy '*'访问立即失败Could not resolve host。生产fs_remote_download采用直连DNS/IP固定并拒绝private/link-local目标。不能将本地私网模拟下载或关闭SSRF/TLS检查计作正向验收；需要可用真实直连网络/DNS或明确可信的代理策略。

2026-10-13恢复后重新验证：Debian与Ubuntu guest以保留的镜像和严格固定主机密钥重新启动，未沿用旧PID。Debian四个数据库/cache systemd服务均active，Ubuntu cloud-init完成、SSH active。SQL密码保持，Redis临时ACL重新应用，四个SSH loopback端口已真正重新连接。旧日志先保存后再启动新serial记录，分别在guest目录 restart-readiness-20261013-r1.log。此项是平台与目标服务就绪，不是新版Agent集成验收。

MariaDB普通数据库账户的GRANT已显式转义数据库名中的下划线：`qa\_scope\_public`。反引号不能取消MySQL/MariaDB数据库GRANT里的`_`通配符。另建qa_scope_pattern账户保留未转义grant，专供PowerLinux注册应拒绝的负面测试。实际查询mysql.db元数据确认两个grant的差异，没有查询其它应用数据库。首次QA断言因mariadb --batch输出再次转义反斜线失败；改为--raw后验证通过，两轮日志均保留。PostgreSQL另有仅用于注册拒绝的pg_read_server_files成员账户，未读取主机文件或执行服务端程序。凭据只在checkout外0700目录/0600 JSON文件。

MongoDB适配器另备真实官方mongo容器，immutable digest为mongo@sha256:d0d926f94df099bff534b7ee5b5986458131a22489dfff8664509af0c1e2ca9c，仅绑定127.0.0.1:44429。实际MongoDB8.0.32认证查询返回两行QA数据。正常身份仅readWrite于qa_scope_public；负面身份在正确authentication DB认证成功并拥有readAnyDatabase，未查询私有DB。私有密码通过stdin写入受限named volume，未放在Docker明文环境变量、argv、Git或日志。health-20261013-r1.log记录实际查询。这是实际数据库驱动目标，不能当作VM或软件安装/升级生命周期的证据。

Systemops防火墙验证器准备了真实Debian kernel nft JSON。nftables1.1.3及三个依赖来自已验签Debian APT并逐包核SHA，在guest私有目录抽取执行，未启用全局nft服务。仅创建table inet powerlinux-systemops-fixture，包含established/related、loopback、四个保留端口和CIDR8080四条accept规则；nft-table-actual-20261013-r2.json真实返回7个对象、4条规则。EXIT trap移除该表，实际查询确认已不存在。r1只是语法错误且未创建table，r1/r2均保留。此项验证实际nft JSON格式，完整PowerLinux Agent防火墙与重启持久恢复仍待新包。

Ubuntu另建独立powerlinux-qa-ssh账户（UID1000）作为终端SSH目标，不是panel/API身份。自有guest原OpenSSH守护进程在固定真实主机密钥下，已通过该账户的真实ed25519公钥认证和服务器PTY分配；工作目录是独立/var/lib/powerlinux-qa-ssh。未修改sshd配置。私钥只保存于checkout外私有目录，root授权公钥用SSH stdin传输，生成的随机密码哈希不输出。日志为ssh-terminal-fixture-20261013-r1.log。PowerLinux独立terminal UID、Agent SO_PEERCRED、REST/MCP/WebSocket共享会话等验收仍待dev8实际发行包，不能从该SSH准备结果推断已通过。
