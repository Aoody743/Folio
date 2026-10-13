#!/usr/bin/env python3
"""Semantic acceptance registry; old route/action counts are trace indexes only."""
from pathlib import Path
import ast
import json

ROOT = Path(__file__).resolve().parents[3]
INVENTORY = json.loads((ROOT / "docs/rewrite/feature-inventory.json").read_text())
TRACKER = json.loads((ROOT / "docs/rewrite/migration-tracker.json").read_text())
P2 = [p for p in TRACKER["plugins"] if p["phase"] == 2]
CAPS = {}


def cap(identifier, behavior, checks, plugins=(), sources=(), extra_entries=()):
    assert identifier not in CAPS
    CAPS[identifier] = {"id": identifier, "behavior": behavior, "status": "planned", "plugins": list(plugins),
        "old_sources": list(sources), "extra_entries": list(extra_entries), "route_ids": [],
        "new_actions": [], "ui_entry": None, "acceptance_checks": list(checks), "evidence": [],
        "conditions": ["Actual service/transport, no canned runtime responses", "Human/API/MCP share identity/resources/results/audit", "Mutation failure and recovery are verified"]}


def route_groups(file, definitions):
    routes = [r for r in TRACKER["core_routes"] if r["old"]["file"] == file]
    assigned = set()
    for identifier, behavior, names, checks in definitions:
        cap(identifier, behavior, checks, sources=[file])
        for route in routes:
            if route["old"]["function"] in names.split():
                assert route["id"] not in assigned
                assigned.add(route["id"])
                CAPS[identifier]["route_ids"].append(route["id"])
    missing = [r["old"]["function"] for r in routes if r["id"] not in assigned]
    assert not missing, (file, missing)


route_groups("web/admin/files/files.py", [
 ("fs.browse", "浏览、分页、递归搜索文件与目录并保留真实元数据", "index get_dir get_dir_size", ["Nested+hidden+Unicode fixtures; pagination/search contents correct", "No symlink/traversal escape; denied roots absent", "Directory size from actual files"]),
 ("fs.read_edit", "编码读取与编辑、乐观内容冲突、保存历史", "get_body save_body", ["UTF8/GB18030/binary detection; content bytes roundtrip", "expected_hash stale edit conflicts without overwriting", "History survives restart and is restorable"]),
 ("fs.tail", "有界文件尾部读取和日志截断", "get_file_last_body close_logs", ["Real multi-megabyte log tail returns exact final lines", "Truncate only authorized file; no runtime secrets"]),
 ("fs.preflight", "覆盖预检与批量复制/移动", "check_exists_files batch_paste copy_file mv_file set_batch_data", ["Source+destination scope independently enforced", "Overwrite decision and rollback preserve previous files", "Unicode paths; partial batch failure reported"]),
 ("fs.metadata", "文件权限、所有者、目录权限继承", "file_access set_file_access", ["Real stat UID/GID/modes reflect action", "Unprivileged actor denied forbidden owner/permission change", "New descendants inherit declared policy"]),
 ("fs.archive_create", "按原有效格式压缩文件与目录", "zip", ["ZIP/tar/gzip and other legacy valid formats separately registered", "Extracted archive hash matches actual inputs", "No unauthorized source data included"]),
 ("fs.archive_extract", "安全解压各格式与失败恢复", "unzip uncompress", ["Traversal/absolute/symlink/hardlink/device/zipbomb cases denied", "No overwrite outside chosen root", "Valid each-format archive actually expands correct bytes"]),
 ("fs.upload", "真实上传与可恢复分段上传", "upload_file upload_segment", ["Stream larger than64KiB through authenticated endpoints", "Offset/hash mismatch rejected; retry not duplicate", "Abort/restart cleanup and final hash verified"]),
 ("fs.create", "创建文件、目录与作用域内删除", "create_file create_dir delete delete_dir", ["Actual creates/deletes and conflict errors", "Root deletion/escaped path denied", "No protected runtime-dir modification"]),
 ("fs.download", "授权下载与远程下载任务", "download download_file", ["Large binary download exact SHA256", "Remote TLS verified; partial transfer failure no corrupt commit", "URL/redirect policy and target scopes enforced"]),
])
route_groups("web/admin/files/recycle.py", [
 ("fs.recycle", "回收站开关、列表、恢复与永久清理", "get_recycle_bin re_recycle_bin del_recycle_bin recycle_bin close_recycle_bin", ["Actual move to recycle, restart persistence, restore original metadata", "Conflicting restore must not overwrite silently", "Permanent delete scoped; external symlink not followed"]),
])
site_specs = {
 "site.py": [
 ("sites.lifecycle", "站点列表、创建、启停、删除与运行状态", "index list add stop start delete check_web_status", ["Real OpenResty HTTP serves only active sites", "Owned config validated/reloaded; failed config restores prior site", "Deletion preserves requested data"]),
 ("sites.domains", "域名绑定、多域名、描述和到期信息", "add_domain del_domain get_site_domains get_domain set_end_date set_ps", ["Host header/SNI routes correct domains", "Duplicate/invalid/unauthorized domains rejected", "Metadata persistent; no fabricated expiry alerts"]),
 ("sites.document_root", "文档根与运行目录、默认索引", "get_root_dir get_index set_index set_path set_site_run_path", ["Real index and nested document-root paths served", "Root/path scope and symlink boundary enforced", "Atomic migration restores root on failure"]),
 ("sites.limits", "站点限速、访问保护与防盗链", "get_limit_net set_limit_net close_limit_net set_has_pwd close_has_pwd get_security set_security", ["Actual HTTP allowed/denied credentials/referrers", "Configured bandwidth/request limit takes effect", "Disable restores access; secrets redacted"]),
 ("sites.raw_config", "授权配置读取与安全保存", "get_host_conf save_host_conf", ["OpenResty nginx -t actual validation", "Invalid syntax never replaces healthy config", "Cross-site/config-root writes denied"]),
 ("sites.php_runtime", "PHP版本绑定与已安装版本发现", "get_site_php_version set_php_version get_php_version", ["Real PHP response reports selected runtime", "Coexisting valid versions route separately", "Missing runtime denied; prior working config retained"]),
 ("sites.rewrite", "伪静态模板、读取与应用", "get_rewrite_list get_rewrite_conf get_rewrite_tpl set_rewrite set_rewrite_tpl", ["Real rewritten URL returns expected app output", "Invalid template rejected with rollback", "Templates persisted and scope checked"]),
 ("sites.logging_default", "访问日志开关与默认站点", "logs_open get_default_site set_default_site", ["Real traffic logged or disabled as declared", "Unknown Host reaches selected default", "No other site's default/config modified"]),
 ],
 "backup.py": [("sites.backup", "站点备份、列表与删除", "get_backup to_backup del_backup", ["Real files+config backed up with hashes", "Restore to isolated target produces same HTTP content", "No secret export or unrelated files; deletion scoped"])],
 "dir.py": [("sites.bindings", "目录绑定、子目录伪静态与user.ini策略", "get_dir_user_ini set_dir_user_ini get_dir_binding add_dir_bind get_dir_bind_rewrite del_dir_bind", ["Actual domain/subdirectory binding maps correct content", "PHP user.ini behavior validated", "Escaped path or conflicting binding denied"])],
 "logs.py": [("sites.logs", "访问与错误日志读取", "get_logs get_error_logs", ["Generate actual request/error then read exact log", "Bounded tail/stream and site-scoped authorization"])],
 "php.py": [("php.cli", "CLI PHP版本查询与切换", "get_cli_php_version set_cli_php_version", ["Actual CLI php -v matches selected version", "Only owned alternative/wrapper changed; rollback tested"])],
 "proxy.py": [("sites.proxy", "反向代理创建/配置/启停/删除", "get_proxy_list set_proxy set_proxy_status get_proxy_conf save_proxy_conf del_proxy", ["Real upstream service forwards status/body/headers", "Upstream TLS trust retained", "Bad config rollback and own-scope deletion"])],
 "redirect.py": [("sites.redirect", "重定向规则配置与启停", "get_redirect set_redirect set_redirect_status get_redirect_conf save_redirect_conf del_redirect", ["Real HTTP status+Location follows defined rule", "Loop/invalid target preflight; rollback", "Other domains unaffected"])],
 "site_default.py": [("sites.default_document", "默认站点文档", "get_site_doc", ["Actual configured default document inspect/edit workflow"])],
 "site_types.py": [("sites.categories", "站点分类管理", "get_site_types add_site_type remove_site_type modify_site_type_name set_site_type", ["Persisted category CRUD+site assignment", "Deleting category preserves sites and scope"])],
 "ssl.py": [
 ("tls.certificates", "证书上传、保存、复用、解绑和删除", "get_ssl set_ssl close_ssl_conf delete_ssl get_cert_list set_cert_to_site remove_cert", ["Real TLS SNI with trusted localCA validates chain+hostname", "Wrong key/chain/expired cert rejected", "Shared cert unlink does not break other site"]),
 ("tls.dns_credentials", "DNS提供商配置及权限", "get_dnsapi set_dnsapi", ["34-provider union and manualDNS registered individually", "Credential fields validated/encrypted/redacted", "Real provider signing requires actual domain+limited credentials; missing remainsblocked"]),
 ("tls.https_redirect", "HTTP到HTTPS跳转开关", "http_to_https close_to_https", ["Actual HTTP redirect location and HTTPS page", "Disable returns original HTTP behavior"]),
 ],
 "ssl_acme.py": [("tls.acme", "ACME签发、续期、日志和失败恢复", "get_acme_logs create_acme", ["Real ACME server/localtrusted testCA issues certificate via actual challenge", "Renewal changes certificate and reloads working TLS", "Invalid challenge/no credentials never counted as issued"])],
 "ssl_let.py": [("tls.letsencrypt", "公开CA签发流程与签发日志", "get_let_logs create_let", ["Actual staging/publicCA + owned testdomain whenavailable", "No credentials/domain remains externalblocked, not mockedpass"])],
}
for filename, definitions in site_specs.items():
    route_groups("web/admin/site/" + filename, definitions)
route_groups("web/admin/firewall/__init__.py", [
 ("security.firewall", "防火墙状态、端口规则与Ping策略", "index get_list get_www_path set_ping add_accept_port del_accept_port set_fw", ["Actual disposableVM packet connectivity changes as planned", "SSH/API connection guard and timed rollback work", "Disable/remove preserves unrelated rules"]),
 ("security.ssh", "SSH信息、端口、root/密码/公钥认证策略", "get_ssh_info set_ssh_port set_ssh_root_status set_ssh_pass_status set_ssh_pubkey_status", ["Real sshd -t plus real clients validate policy", "Wrong hostkey rejected; correct publickey/password as selected", "Lost connectivity automatically recovers; existing sessions protected"]),
])
route_groups("web/admin/crontab/__init__.py", [
 ("schedules.lifecycle", "计划任务创建、查询、编辑、启停和删除", "index list crontab_del set_cron_status get_data_list get_crond_find modify_crond add", ["Real schedule executes owned target at configured time", "Restart persistence, no duplicate after retries", "Validation rejects invalid schedule/unauthorized target"]),
 ("schedules.run_logs", "任务手动执行、日志和日志清理", "logs del_logs start_task", ["Actual success/failure/cancel states and bounded logs", "Secret values absent; only owned log cleared"]),
])
route_groups("web/admin/logs/__init__.py", [("logs.panel_audit", "面板日志与审计文件查询/清理", "index get_log_list del_panel_logs get_audit_logs_files get_audit_file", ["Real actions/errors create records with correct actor/resource", "Scoped/bounded reads; logclear leaves protected audit policy", "No HTML/credential injection disclosure"])])
route_groups("web/admin/monitor/__init__.py", [("monitor.overview", "实时与历史监控页面", "index api_overview", ["Real host metrics source/timestamps and unavailable handling", "No request-time blocking CPU sampling"])])
route_groups("web/admin/system/system.py", [
 ("monitor.host", "CPU/内存/磁盘/网络/负载与环境信息", "system_total get_env_info network disk_info get_load_average get_disk_io get_cpu_io get_network_io", ["Compare real procfs/system counters and historical ranges", "Queries do not mutate host; response snapshots cached"]),
 ("system.panel_restart", "面板受控重启", "restart", ["Actual service restarts with persisted data/leases and no duplicate jobs"]),
 ("system.host_power", "主机重启/关机", "restart_server shutdown_server", ["Only disposableVM; observe real disconnect/reboot or stop", "Result distinguishes dispatch from verified host state", "Authorized resource only; no shared-host invocation"]),
 ("monitor.retention", "监控启停和历史保留", "set_control", ["Actual samples collected/pruned by declared retention", "Disabled collection stops writes without fake healthy values"]),
])
route_groups("web/admin/system/upgrade.py", [("release.upgrade", "面板受控升级", "update_server", ["Actual different-version package upgrade and data migration", "Bad-health/fault restores former release+data", "Artifact verification mandatory; no upstream fork overwrite"])])
route_groups("web/admin/task/__init__.py", [("tasks.management", "任务计数、列表、执行日志、进度和移除", "task_count list get_exec_log get_task_speed remove_task", ["Real queue/process state; large-output/cancel/timeout/restart recovery", "No fake progress/success; actor/resource and log redaction", "Removal cannot orphan running privileged processes"])])
route_groups("web/admin/plugins/__init__.py", [
 ("software.catalog", "软件分类、配置页与首页展示", "index init index_list list set_index menu file setting", ["Actual installed platform/version/capability states", "Integration version separate from application version", "UI unavailable states truthful; all23 phase2 plugins discoverable"]),
 ("software.lifecycle", "软件安装、卸载与预检查", "init_install install uninstall", ["Trusted recipe+signature/checksum verify; real health", "Plan/lock/commit/failure rollback/upgrade/uninstall data retention", "Only explicitly selected software changes system"]),
 ("plugins.extensions", "插件上传/导入、动作与扩展回调", "update_zip input_zip run callback", ["Versioned declarative contract, no old eval/rootShell wrapper", "Imported artifacts/manifest trust+scope+path checks", "Third-party migration policy explicit; valid old capability preserved"]),
])
route_groups("web/admin/soft/__init__.py", [("software.navigation", "软件页面与插件导航", "index", ["Every supported plugin has real UI entry and return path"])])
route_groups("web/admin/dashboard/dashboard.py", [
 ("dashboard.aggregates", "概览与真实网站/软件/任务等聚合", "index overview_stats", ["Counts match actual persisted resources, not fixtures"]),
 ("settings.login_entry", "自定义安全登录入口", "admin_safe_path", ["Configured entry protects real routes without MFA bypass", "Recovery documented and verified"]),
 ("integrations.webhook", "原有Webhook接口", "webhook", ["Real authenticated event delivery, retry and signature", "No untrusted content becomes command authorization"]),
])
route_groups("web/admin/dashboard/login.py", [
 ("auth.login", "统一登录、会话与MFA验证", "login check_login verifyLogin do_login", ["P1 actual14case evidence retained; apply affected regressions after auth change"]),
 ("auth.captcha", "登录验证码与认证节流策略", "code", ["Actual challenge verification/replay denial", "Does not globally disable panel on anonymous failures"]),
 ("settings.maintenance", "面板关闭与维护状态", "close", ["Only explicitly authorized maintenance closes access", "Authenticated recovery path works"]),
])
setting_specs = {
 "app.py": [("auth.applications", "完整应用授权生命周期与面板API开关", "set_panel_api get_app_list add_app toggle_app_status delete_app", ["Real scope/resource/expiry/revoke and API policy enforced", "Disabled application denied in HTTP/MCP/stream"] )],
 "notify_email.py": [("notify.email", "邮件通知配置、启停与测试发送", "get_notify_email set_notify_email set_notify_email_test set_notify_email_enable", ["Real localSMTP server receives actual message and TLS/auth handling", "Actual externalSMTP requires secure credentials; remainsblocked ifmissing", "Secrets absent from logs/outputs"] )],
 "notify_tgbot.py": [("notify.telegram", "Telegram通知配置、启停与测试", "get_notify_tgbot set_notify_tgbot set_notify_tgbot_test set_notify_tgbot_enable", ["Live Telegram API with actual bot/ownedchat whenprovided", "No mock receipt counted; retry/redaction tested locally"] )],
 "panel_bookmark.py": [("settings.panel_links", "多个面板链接与快捷信息管理", "add_panel_info get_panel_list del_panel_info set_panel_info", ["Persisted bookmark CRUD and safe URL navigation", "Credential fields encrypted/redacted ifpresent"] )],
 "panel_ssl.py": [("settings.panel_tls", "面板TLS、本地证书、域名和关闭", "get_panel_ssl save_panel_ssl del_panel_ssl set_panel_local_ssl close_panel_ssl set_panel_domain", ["Actual verifiedHTTPS+Securecookies+MCPauth origin/audience", "Bad cert/key/domain cannot lockout; backup/recovery", "No TLS verification bypass"] )],
 "secondary_verifiy.py": [("auth.mfa", "MFA配置生命周期", "get_auth_secret set_auth_secret", ["P1 actualTOTP/replay/bypassregression retained", "No enabled secret ordinary read or API alternate login"] )],
 "setting.py": [
 ("settings.branding", "面板名称、页脚与账户名称", "index set_webname set_footer_text set_name", ["Persistent visible UI changes with real identity linkage", "Escaped markup and no account-loss onrename"]),
 ("settings.network", "面板IP、端口与IPv6", "set_ip set_port set_ipv6_status", ["Real bind behavior in disposableVM", "Invalid/conflicting bind preflight and connection-safe rollback"]),
 ("settings.paths", "默认网站与备份目录", "set_backup_dir set_www_dir", ["Actual authorized roots and path permission checks", "Existing resources/data preserved after moving defaults"]),
 ("settings.auth_frontdoor", "安全入口、BasicAuth与响应状态", "set_admin_path set_basic_auth set_status_code", ["Actual browser/MCP auth still coherent", "MFA remains required; no locked-out unsafe bypass"]),
 ("settings.debug_maintenance", "受控诊断开关与面板维护", "open_debug close_panel", ["No remote debugger/code-execution exposure", "Recovery and scoped authorization"]),
 ("auth.password", "账户密码修改", "set_password", ["P1 credential/sessionrevocation retained; no password/secretlog"]),
 ],
 "temp_login.py": [("auth.temporary_login", "临时登录链接、撤销与日志", "get_temp_login set_temp_login remove_temp_login get_temp_login_logs", ["Short-lived single-use scoped grants, valid MFA policy", "Replay/expiry/revoke denied; real audit identity", "No global anonymous lockout or unlimited privilege"] )],
 "timezone.py": [("system.time", "时区查询修改与时间同步", "get_timezone_list sync_date set_timezone", ["Actual disposableVM timedatectl/clock source change", "Never adjust shared-host clock; audit+restore"] )],
}
for filename, definitions in setting_specs.items():
    route_groups("web/admin/setting/" + filename, definitions)

# Shared acceptance mechanics never replace plugin-specific application behavior.
for plugin in P2:
    name = plugin['name']
    cap('software.' + name + '.lifecycle', plugin['title'] + '真实软件生命周期', [
        'Trusted artifact/repository provenance, actual app version and supported distro/arch verified separately from integration version',
        'Plan is readonly; apply acquires instance lock, validates config, starts real process and verifies actual health',
        'Install/repeat/start/stop/restart/reload/config/upgrade/uninstall retain intended data; malformed config and bad health roll back',
        'Unsupported/EOL source remains explicitly unavailable with migration path; version-directory discrepancy never silently dropped',
        'Non-owner instance rejected; no arbitrary executable, root path or privileged shell payload',
    ], plugins=[name], sources=['plugins/' + name + '/index.py', 'plugins/' + name + '/info.json'])


def plugin_cap(identifier, behavior, checks, names):
    cap(identifier, behavior, checks, plugins=names,
        sources=['plugins/' + name + '/index.py' for name in names])

plugin_cap('openresty.configuration', '真实OpenResty配置、模块、限流、RTMP及站点运行', [
 'Executable identifies OpenResty, nginx is not substituted',
 'Validate/reload configs, generate real access/error logs and exercise Lua endpoint',
 'Advertised RTMP/module variant exercised with actual compatible client; unsupported variant remains pending',
 'Site/PHP/TLS integration uses the same managed instance, no extra untracked nginx service',
], ['openresty'])
for name, source in [('php','upstream source'), ('php-apt','signed APT packages'), ('php-yum','signed RPM packages')]:
    plugin_cap(name + '.runtime', 'PHP多版本、扩展、FPM、配置和CLI（' + source + '）', [
        'Actual php -v/FPM request reports version and source; coexisting runtimes route independently',
        'Install/enable/disable actual extension; php -m and HTTP extension behavior verified',
        'PHP ini, disabled functions, memory/upload/session limits and FPM pools take effect',
        'Pool socket/user/process configuration and CLI switch validated without globally rewriting unrelated runtime',
        'Backup/restore config+extension choices and malformed config rollback',
    ], [name])
plugin_cap('php.guard', 'PHP守护监测与故障自动恢复', ['Kill owned FPM in disposableVM; guard observes real failure and recovers actual process', 'Disable guard prevents automatic restart; logs/states truthful'], ['php-guard'])
plugin_cap('phpmyadmin.application', 'phpMyAdmin真实数据库登录和管理界面', ['Actual app assets/version served through managed PHP/OpenResty', 'Actual DB login, query and schema workflow; anonymous/wrong credentials denied', 'Update rollback preserves app config and DB contents'], ['phpmyadmin'])
plugin_cap('pgadmin.application', 'pgAdmin真实PostgreSQL管理界面', ['Actual pgAdmin app/version, browser login and real PG connection', 'Query/schema management and saved connection persistence', 'Database credentials not exposed to other panel actors'], ['pgadmin'])
for name in ['mysql','mysql-community','mariadb']:
    plugin_cap(name + '.database', 'SQL数据库、用户权限和配置管理', [
      'Actual server product/version identifies selected MySQL/MariaDB source; no substitution',
      'Create/list/drop DB, manage least-privilege users/password/network access and read/write authorization',
      'Real tables/data/import/export/repair supported by selected engine; config changes validated/reversible',
      'Root/password rotation does not leak secrets or silently break stored connections',
      'DB-scoped credential cannot access another DB, server files, arbitrary grants or global admin operations',
    ], [name])
    plugin_cap(name + '.backup', 'SQL真实备份、压缩下载、恢复和二进制日志', [
      'Stream database larger than64KiB to backup/export with actual bytes and checksum',
      'Restore into isolated instance and compare schema+rows; nonempty target conflict preflight',
      'Binlog list/retention and supported point-in-time restore operate on real logs',
      'Backup credentials protected; cancellation/failure cleans partial artifact',
    ], [name])
    plugin_cap(name + '.replication', 'SQL主从复制、SSH全量同步和复制诊断', [
      'Two actual engine instances; scoped replication user/setup/status agree with engine',
      'Insert on source appears on replica; pause/disconnect/resume and resync recover correctly',
      'SSH full sync validates host key, transfers real snapshot and restores replication',
      'Replica readonly policy and replication errors are visible; no fabricated lag/state',
    ], [name])
plugin_cap('postgresql.database', 'PostgreSQL库、角色、权限、HBA和配置', ['Real PG database/role CRUD with least privilege', 'HBA allow/deny observed through real connections; syntax validation and rollback', 'Config/password changes effective; database-scoped role cannot access another DB'], ['postgresql'])
plugin_cap('postgresql.backup', 'PostgreSQL备份、导出与恢复', ['Actual pg_dump compatible-format stream and checksum', 'Restore schema+rows into isolated real PG; conflict/failure/cancel recovery', 'Credentials redacted and retained data protected'], ['postgresql'])
plugin_cap('postgresql.replication', 'PostgreSQL复制、SSH全量同步和慢查询诊断', ['Two real PG instances, actual WAL replication and data agreement', 'Strict-hostkey SSH base/full sync, interruption and reconnect recovery', 'Actual server replication/slow-query diagnostic data; no placeholders'], ['postgresql'])
plugin_cap('mongodb.database', 'MongoDB数据库、用户、集合和文档管理', ['Actual MongoDB product/version and real authenticated connection', 'DB/user/collection/document CRUD; ObjectId and filters preserved', 'Engine least privilege enforces DB boundaries; authentication/key configuration validated'], ['mongodb'])
plugin_cap('mongodb.backup_replica', 'MongoDB备份恢复、复制集和同步', ['Actual dump/export and isolated restore compare BSON/documents', 'Actual multi-member replica set; insert replication and primary election', 'Sync interruption recovery and auth/keyfile protection; not a single-node pretend replica'], ['mongodb'])
for name in ['redis','valkey']:
    plugin_cap(name + '.cache', '键值服务配置、类型、TTL、认证与持久化', ['Actual product/version, network/auth/config validation', 'All supported key types and TTL operations via real server', 'Save/restart persistence matches configured RDB/AOF policy', 'Resource isolation uses actual engine ACL/instance boundary'], [name])
    plugin_cap(name + '.replication', '键值服务复制与集群诊断', ['Two actual instances replicate writes and status matches engine', 'Replica-write refusal, failover/reconnect and advertised cluster behavior', 'Configuration rollback and no leaked replication credentials'], [name])
plugin_cap('memcached.cache', 'Memcached配置、缓存值、TTL和运行统计', ['Actual daemon/version, get/set/delete/expiry and stats', 'Configured memory/connections/listen settings effective after restart', 'No claim of disk persistence for in-memory Memcached'], ['memcached'])
plugin_cap('docker.images', 'Docker仓库凭据、镜像拉取和导入导出', ['Actual isolated daemon, pull trusted digest and inspect actual image', 'Image export/import bytes verified; owned deletion and registry credentials protected', 'API user never receives shared host Docker socket access'], ['docker'])
plugin_cap('docker.containers', 'Docker容器创建、端口、启停、日志和exec', ['Create owned container with real ports/volumes and HTTP response', 'Start/stop/restart/log stream and real interactive exec, exit/cancel cleanup', 'Other owner containers/resources denied; no unrestricted privileged/host-mount payload'], ['docker'])
plugin_cap('ftp.users', 'FTP用户、权限、目录、配额和服务端口', ['Actual Pure-FTPd login and binary transfer; wrong login denied', 'User confined to authorized directory and quota actually enforced', 'Password/listen/policy updates validated; data retained on uninstall'], ['pureftp'])
plugin_cap('rsync.modules', 'rsync模块、认证、导入导出和同步', ['Actual daemon/client transfer and checksum', 'Module readonly/auth/path restrictions; delete/exclude policies observed', 'Interrupted transfer/retry and credential rotation recovery'], ['rsyncd'])
plugin_cap('rsync.schedules', 'rsync计划任务与lsyncd实时同步', ['Actual scheduled transfer and lsyncd filesystem event replication', 'Exclude rules, retry/stop and real logs/state', 'No hostwide cron/shell modification'], ['rsyncd'])
plugin_cap('backup_ftp.remote', 'FTP远程备份目录、传输、删除和恢复', ['Real FTP server with owned user; list/mkdir/upload/download/delete', 'Site/database backup uploaded then restored with byte/data equivalence', 'Authentication/transfer interruption preserves valid backups, no false success'], ['backup_ftp'])
plugin_cap('acme.domains', 'ACME域名、账户、提供商与配置模板', ['34-provider union + manual DNS separately accounted', 'Actual account/domain CRUD and validated credential fields', 'Templates/config encrypted and secrets never returned in logs/commands'], ['acme_pandominassl_apply'])
plugin_cap('acme.hooks_sync', 'ACME签发续期钩子及Cloudflare/DNSPod域名同步', ['Actual ACME challenge issues/renews certificate with trusted local CA', 'Hook executes only trusted managed action and deploys real certificate', 'Real external provider list/change/sync requires owned domain + limited credentials; no mock counted as verified', 'Failed challenge/renewal keeps working prior certificate'], ['acme_pandominassl_apply'])
plugin_cap('supervisor.programs', 'Supervisor进程组、用户、配置、启停和日志', ['Actual supervisord owns configured programs and reports actual PID/state', 'Config validate/update; real start/stop/restart, stdout/stderr and crash/retry', 'Trusted executable/user/path policies enforced; no cross-owner management'], ['supervisor'])

# Dynamic script dispatch and class methods were absent from the old action index.
def dynamic_cap(identifier, behavior, script, names, checks, plugin='data_query'):
    path = 'plugins/' + plugin + '/' + script
    parsed = ast.parse((ROOT / path).read_text())
    functions = {node.name: node.lineno for node in ast.walk(parsed) if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))}
    expected = names.split()
    assert set(expected) <= functions.keys(), (path, set(expected) - functions.keys())
    cap(identifier, behavior, checks, plugins=[plugin], sources=[path],
        extra_entries=[{'file':path, 'function':name, 'line':functions[name], 'dispatch':'script/class'} for name in expected])

common_query_checks = ['Actual engine response, paging/error handling and resource-restricted connection', 'Same result/identity via browser, REST and MCP; unsupported operation never returns fake success']
dynamic_cap('query.redis', 'Redis数据库/键列表、类型、TTL与写删/清空', 'nosql_redis.py', 'get_list get_dbkey_list set_kv del_val batch_del_val clear_flushdb', common_query_checks + ['Verify strings/list/set/zset/hash and TTL on real Redis', 'Replica write refusal and destructive-operation scopes; instance-wide FLUSH never DB-scope bypass'])
dynamic_cap('query.memcached', 'Memcached slab/items、分页、值和TTL管理', 'nosql_memcached.py', 'get_items get_key_list del_val set_kv clear', common_query_checks + ['Actual slab/items stats and values/expiry', 'Clear scoped to dedicated authorized instance'])
dynamic_cap('query.mongodb', 'MongoDB库/集合、文档过滤、ObjectId与CRUD', 'nosql_mongodb.py', 'get_db_list get_collections_list get_data_list set_kv del_val batch_del_val clear_flushdb del_by_id', common_query_checks + ['Actual ObjectId delete and BSON semantics', 'Document query pagination and database boundary enforced by engine role'])
dynamic_cap('query.sql_data', 'SQL服务、库、表、记录查询和表详情', 'sql_mysql.py', 'get_server_list get_db_list get_table_list get_data_list get_table_info', common_query_checks + ['Real schemas/rows, parameterized values and identifiers', 'DB-scoped role cannot read server files/other DB or mint admin privileges'])
dynamic_cap('query.sql_diagnostics', 'SQL进程、状态、统计、TopSQL、网络和连接数', 'sql_mysql.py', 'get_proccess_list get_status_list get_stats_list get_topn_list get_net_list get_conn_count', common_query_checks + ['Generate actual concurrent SQL workload; diagnostics correspond to real server counters'])
dynamic_cap('query.sql_locks', 'SQL锁、死锁诊断与终止阻塞会话', 'sql_mysql.py', 'get_lock_sql kill_lock_pid kill_all_lock get_deadlock_info', common_query_checks + ['Create actual blocked transaction/deadlock, identify owner and terminate authorized blocker', 'Query-only scope cannot kill foreign sessions'])
dynamic_cap('query.sql_indexes_replication', 'SQL冗余索引、外键与复制诊断', 'sql_mysql.py', 'get_redundant_indexes redundant_indexes_cmd get_fpk_info get_slave_status', common_query_checks + ['Create real redundant/FK indexes and source/replica; findings match engine', 'Index mutation privileged and validated, not raw arbitrary command execution'])
dynamic_cap('terminal.ssh_profiles', '保存SSH主机、认证凭据与快捷命令', 'index.py', 'add_cmd del_cmd get_cmd_list get_server_by_host get_server_list del_server add_server', ['Actual SSH profile CRUD with password/key/passphrase stored by SecretID', 'Strict hostkey verification; changed/wrong key denied', 'Saved commands belong to authorized host/profile and are inert until explicit execution'], plugin='webssh')
cap('terminal.local_pty', '真实本地PTY交互、窗口尺寸与进程退出', ['Interactive shell sees isatty and real dimensions; input/output order and exit code exact', 'Separate terminal UID/GID from panel API/data owner, supplementary groups dropped', 'PTY-only token cannot read panel runtime/database/encryption keys or modify managed agent policy', 'Bounded sessions/frames/output, cancellation/timeout/revoke kills real owned process group'], sources=['web/admin/__init__.py','web/utils/ssh/ssh_local.py'], extra_entries=[{'file':'web/admin/__init__.py','event':'webssh','dispatch':'socketio'}])
cap('terminal.remote_ssh', '真实SSH终端、PTY、尺寸和生命周期', ['Real sshd password/privatekey/passphrase connection with known hostkey', 'Wrong hostkey and wrong credential denied; server restart disconnect state truthful', 'Resize/input/output/reconnect/replay without duplicated commands; revoke closes remote session'], plugins=['webssh'], sources=['web/admin/__init__.py','web/utils/ssh/ssh_terminal.py'], extra_entries=[{'file':'web/admin/__init__.py','event':'webssh_websocketio','dispatch':'socketio'}])
cap('terminal.shared_transports', '网页WebSocket与MCP使用同一终端会话', ['Human opens real session; MCP attaches to exact same session and sees retained ordered output', 'Both can send permitted input/resize, exclusive policy and actor audit correct', 'Cross-actor unauthorized attach denied; CSRF/Origin/auth on WebSocket upgrade enforced', 'Disconnect/revoke/lease expiry/cancel cleans actual SSH/PTY resources'], plugins=['webssh'], sources=['web/utils/ssh/ssh_terminal.py','web/utils/ssh/ssh_local.py'])

all_routes = TRACKER['core_routes']
route_to_cap = {rid: c['id'] for c in CAPS.values() for rid in c['route_ids']}
assert len(route_to_cap) == len(all_routes) == 235, (len(route_to_cap), len(all_routes))
assert len(P2) == 23
plugin_by_name = {p['name']: p for p in P2}
for c in CAPS.values():
    assert set(c['plugins']) <= plugin_by_name.keys(), (c['id'], c['plugins'])
registry = {
 'schema_version':1,
 'purpose':'Independent semantic acceptance registry. Entries stay planned until actual implementation/test evidence; route/action string counts are trace indexes only.',
 'evidence_policy':{'verified':'Actual named application/transport with reproducible assertions and retained evidence', 'blocked':'Required real external credentials/domain/source/platform unavailable; local simulation is not provider verification', 'unrun':'Implemented or planned but not actually executed', 'EOL_versions':'Explicit supported-source/platform decision and migration path; no fictional install success'},
 'trace_summary':{'old_http_routes':len(all_routes), 'phase2_plugins':len(P2), 'semantic_acceptance_groups':len(CAPS), 'all_routes_assigned_once':True, 'dynamic_dispatch_accounted':True},
 'capabilities':list(CAPS.values()),
 'route_index':[{'id':r['id'], 'old':r['old'], 'planned_phase':r['phase'], 'existing_tracker_status':r['status'], 'capability':route_to_cap[r['id']]} for r in all_routes],
 'plugin_index':[{'id':p['id'], 'name':p['name'], 'title':p['title'], 'integration_metadata_versions':p['old_metadata_versions'], 'version_directories':p['old_version_directories'], 'old_action_strings_trace_only':p['old_declared_actions'], 'capabilities':[c['id'] for c in CAPS.values() if p['name'] in c['plugins']]} for p in P2],
}
out = Path(__file__).parent / 'acceptance-matrix.json'
out.write_text(json.dumps(registry,ensure_ascii=False,indent=2) + '\n')
print(json.dumps(registry['trace_summary'],ensure_ascii=False))
