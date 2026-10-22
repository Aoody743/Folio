#!/usr/bin/env bash
# Folio rewrite installer. No compilation or third-party service installs.
set -Eeuo pipefail
umask 077

# BEGIN GENERATED INSTALLER UI
# Kept identical in the core and bootstrap by sync-installer-ui.py.
ui_language=${FOLIO_LANG:-${LC_ALL:-${LC_MESSAGES:-${LANG:-en}}}}
ui_language_explicit=0
[[ -z ${FOLIO_LANG:-} ]] || ui_language_explicit=1
ui_assume_yes=0
ui_args=("$@")
for ((ui_index=0; ui_index<${#ui_args[@]}; ui_index++)); do
  case "${ui_args[$ui_index]}" in
    --lang)
      (( ui_index + 1 < ${#ui_args[@]} )) || { printf 'Missing --lang value / 缺少语言参数\n' >&2; exit 2; }
      ui_language_explicit=1
      ui_index=$((ui_index + 1)); ui_language=${ui_args[$ui_index]} ;;
    --yes|--non-interactive) ui_assume_yes=1 ;;
    --no-color) export NO_COLOR=1 ;;
  esac
done
case "$ui_language" in
  zh|zh-CN|zh_CN|zh_CN.*|zh-CN.*|zh_TW*|zh-Hant*) ui_language=zh-CN ;;
  en|en-*|en_*|C|C.*|POSIX) ui_language=en ;;
  *)
    if [[ " ${ui_args[*]} " == *' --lang '* || -n ${FOLIO_LANG:-} ]]; then
      printf 'Use --lang zh-CN or --lang en / 请使用 --lang zh-CN 或 --lang en\n' >&2; exit 2
    fi
    ui_language=en ;;
esac
ui_started=$SECONDS
ui_log=
ui_log_reported=0
ui_curl_progress=(--silent --show-error)
if [[ -t 2 && ${TERM:-dumb} != dumb && -z ${NO_COLOR:-} ]]; then ui_curl_progress=(--progress-bar --show-error); fi
# Only presentation messages enter this private log, never command arguments,
# environment variables, policy contents, bootstrap tokens or passwords.
ui_copy() {
  local ui_key=$1
  if [[ "$ui_language" == zh-CN ]]; then
    case "$ui_key" in
      '%s (elapsed %ss)\n') ui_key='%s（已用时 %s 秒）\n' ;;
      '--agent-policy requires --enable-agent') ui_key='--agent-policy 需要 --enable-agent' ;;
      '--attestation requires GitHub CLI') ui_key='--证明需要 GitHub CLI' ;;
      '--enable-agent requires an explicit --agent-policy') ui_key='--enable-agent 需要显式的 --agent-policy' ;;
      '--enable-source-build requires --enable-software') ui_key='--enable-source-build 需要 --enable-software' ;;
      'A different artifact is already installed under this version') ui_key='该版本下已经安装了不同的工件' ;;
      'A new Folio home is ready. The panel will listen on localhost by default.\n') ui_key='即将首次安装 Folio，面板默认只监听本机地址。\n' ;;
      'A server or local maintenance command still holds the production data lock') ui_key='服务器或本地维护命令仍然持有生产数据锁' ;;
      'A trusted SHA256 pin is required (--sha256)') ui_key='需要可信的 SHA256 引脚 (--sha256)' ;;
      'API user/group must not have root identity') ui_key='API 用户/组不得具有 root 身份' ;;
      'Agent deployment policy failed static validation') ui_key='代理部署策略静态验证失败' ;;
      'Agent module state must not be a symbolic link') ui_key='代理模块状态不能是符号链接' ;;
      'Agent policy ancestors must be real root-owned directories') ui_key='代理策略祖先必须是真正的 root 拥有的目录' ;;
      'Agent policy ancestors must not permit untrusted replacement') ui_key='代理策略祖先不得允许不受信任的替换' ;;
      'Agent policy must be an absolute root-owned regular file') ui_key='代理策略必须是绝对 root 拥有的常规文件' ;;
      'Agent policy must be private (0600 or 0400)') ui_key='代理策略必须是私有的（0600 或 0400）' ;;
      'Agent service did not become ready with candidate version/policy') ui_key='代理服务尚未准备好候选版本/策略' ;;
      'Agent transactions require the coreutils sync command') ui_key='代理事务需要 coreutilssync 命令' ;;
      'Another installer is active') ui_key='另一个安装程序处于活动状态' ;;
      'Archive SHA256 does not match the trusted pin') ui_key='存档 SHA256 与受信任的 PIN 不匹配' ;;
      'Archive does not exist') ui_key='存档不存在' ;;
      'Binary and package version differ') ui_key='二进制版本和软件包版本不同' ;;
      'Cancelled before making changes.\n') ui_key='已取消，尚未进行变更。\n' ;;
      'Checking this server') ui_key='检查这台服务器' ;;
      'Choose one operation') ui_key='选择一项操作' ;;
      'Compiler UID/GID must be distinct from API, runtime and terminal accounts') ui_key='编译器 UID/GID 必须与 API、运行时和终端帐户不同' ;;
      'Continue? [Y/n] ') ui_key='继续吗？[Y/n] ' ;;
      'Current release must be a managed symbolic link') ui_key='当前版本必须是托管符号链接' ;;
      'Deployment flags apply only to install/upgrade') ui_key='部署标志仅适用于安装/升级' ;;
      'Deployment restore failed; recovery journal retained in %s. Services remain stopped.\n') ui_key='部署恢复失败；恢复日志保留在 %s 中。服务仍然停止。\n' ;;
      'Downloaded release archive failed its SHA-256 check.') ui_key='下载的版本存档未通过 SHA-256 检查。' ;;
      'Downloading and verifying the package') ui_key='下载并校验安装包' ;;
      'Duplicate archive member: %s') ui_key='重复的存档成员：%s' ;;
      'Existing %s account has unmanaged home/shell') ui_key='现有 %s 帐户具有非托管主目录/shell' ;;
      'Existing permissions and module settings will be preserved.\n') ui_key='本次不会更改已有权限和模块配置。\n' ;;
      'Existing powerlinux user has unexpected home; refusing to reuse') ui_key='现有的powerlinux用户有意想不到的家；拒绝重复使用' ;;
      'Existing powerlinux user is interactive; refusing to reuse') ui_key='现有powerlinux用户交互；拒绝重复使用' ;;
      'Existing unit is unmanaged: %s') ui_key='现有单元不受管理：%s' ;;
      'Existing unit must be a root-owned regular file: %s') ui_key='现有单元必须是 root 拥有的常规文件：%s' ;;
      'Existing unit permits untrusted modification: %s') ui_key='现有单元允许不受信任的修改：%s' ;;
      'Existing unit resolves to an unmanaged location: %s') ui_key='现有单元解析为非托管位置：%s' ;;
      'Failed to preserve current data (check disk capacity). Recovery journal retained; inspect %s before retrying.\n') ui_key='无法保存当前数据（检查磁盘容量）。保留恢复日志；重试之前检查 %s。\n' ;;
      'Folio and compatibility binaries differ') ui_key='Folio 和兼容性二进制文件不同' ;;
      'Folio is ready. The service health check passed.\n') ui_key='Folio 已准备好，服务健康检查已通过。\n' ;;
      'For a fresh localhost installation, keep this SSH tunnel open on your computer:\n') ui_key='首次按默认配置安装时，请在你的电脑上运行以下 SSH 隧道并保持连接：\n' ;;
      'For first setup, read the private bootstrap-token file in a trusted server console. Do not put its contents in URLs or logs.\n') ui_key='首次设置时，请在可信的服务器控制台读取私有 bootstrap-token 文件。不要将令牌放进 URL 或日志。\n' ;;
      'Hello, I am Folio. I will guide you through this installation.\n') ui_key='你好，我是 Folio。接下来带你完成安装。\n' ;;
      'I found an existing installation. Configuration and data will be preserved during this upgrade.\n') ui_key='发现已有安装。这次将进行升级，并保留现有配置和数据。\n' ;;
      'Incomplete release package') ui_key='发布包不完整' ;;
      'Installation failed; restoring the prior release and data snapshot.\n') ui_key='安装未能完成，正在恢复上一版本和数据快照。\n' ;;
      'Installation/configuration directory permits untrusted replacement: %s') ui_key='安装/配置目录允许不受信任的替换：%s' ;;
      'Installation/configuration paths must be root-owned real directories: %s') ui_key='安装/配置路径必须是 root 拥有的真实目录：%s' ;;
      'Installed Folio %s. Preserved data: %s\n') ui_key='已安装 Folio %s。数据保留在：%s\n' ;;
      'Installing with a recovery snapshot') ui_key='保留恢复快照并安装' ;;
      'Interrupted installation detected. Recovery is being performed; rerun the requested command afterward') ui_key='检测到安装中断。正在执行恢复；之后重新运行请求的命令' ;;
      'Interrupted-install prior release is missing') ui_key='中断安装，缺少先前版本' ;;
      'Invalid current-release link') ui_key='当前版本链接无效' ;;
      'Invalid interrupted-install journal; inspect backups before continuing') ui_key='无效的中断安装日志；在继续之前检查备份' ;;
      'Invalid interrupted-install prior release') ui_key='先前版本的中断安装无效' ;;
      'Invalid packaged version') ui_key='打包版本无效' ;;
      'Invalid rollback snapshot') ui_key='回滚快照无效' ;;
      'Invalid version') ui_key='版本无效' ;;
      'Latest Agent host ownership/state was preserved; an incompatible Agent remains stopped for recovery.\n') ui_key='保留最新的代理主机所有权/状态；不兼容的代理保持停止状态以进行恢复。\n' ;;
      'Linux is required') ui_key='需要 Linux' ;;
      'Managed metadata must not be a symbolic link: %s') ui_key='托管元数据不得是符号链接：%s' ;;
      'Managed paths changed unexpectedly') ui_key='托管路径意外更改' ;;
      'Managed paths must not be symbolic links') ui_key='托管路径不得是符号链接' ;;
      'Missing value for %s') ui_key='参数 %s 缺少值' ;;
      'Missing value for --agent-policy') ui_key='--agent-policy 缺少值' ;;
      'Missing value for --prefix') ui_key='--prefix 缺少值' ;;
      'Module account must have a non-root UID/GID distinct from the API') ui_key='模块帐户必须具有与 API 不同的非根 UID/GID' ;;
      'Module deployment requires --enable-agent') ui_key='模块部署需要--enable-agent' ;;
      'Module directory is a symbolic link: %s') ui_key='模块目录是一个符号链接：%s' ;;
      'Module home has an unmanaged owner/type: %s') ui_key='模块主目录有一个非托管所有者/类型：%s' ;;
      'Module home is a symbolic link: %s') ui_key='模块主目录是一个符号链接：%s' ;;
      'No answer received. Nothing has been changed.\n') ui_key='未收到回答，尚未进行变更。\n' ;;
      'No managed installation found') ui_key='找不到托管安装' ;;
      'No prior release exists in snapshot') ui_key='快照中不存在先前版本' ;;
      'No rollback snapshot exists') ui_key='不存在回滚快照' ;;
      'No user or service was changed. Start: %s/current/powerlinux serve --server-config '"'"''"'"' --data-dir %s --listen 127.0.0.1:9000\n') ui_key='没有更改用户或服务。开始：%s/current/powerlinux 服务 --server-config '"'"''"'"' --data-dir %s --listen 127.0.0.1:9000\n' ;;
      'Operation: %s | Installation: %s | Data: %s\n') ui_key='操作：%s | 安装目录：%s | 数据目录：%s\n' ;;
      'Please answer Y or N.\n') ui_key='请输入 Y 或 N。\n' ;;
      'Prefix must be an absolute path without spaces or shell metacharacters') ui_key='前缀必须是绝对路径，不含空格或 shell 元字符' ;;
      'Prefix must not be a symbolic link') ui_key='前缀不能是符号链接' ;;
      'Preparing the local start command') ui_key='准备本地启动命令' ;;
      'Preserved Agent policy is incompatible with the candidate binary') ui_key='保留的代理策略与候选二进制文件不兼容' ;;
      'Preserved failure evidence: %s\n') ui_key='保留的失败证据：%s\n' ;;
      'Prior API recovery failed its real health check. Recovery journal retained in %s; services remain stopped.\n') ui_key='之前的 API 恢复未通过其真正的运行状况检查。恢复日志保留在 %s 中；服务仍然停止。\n' ;;
      'Prior Folio command differs from its compatibility binary') ui_key='之前的 Folio 命令与其兼容性二进制文件不同' ;;
      'Prior Folio command lacks integrity metadata') ui_key='之前的 Folio 命令缺乏完整性元数据' ;;
      'Prior Folio command was modified') ui_key='修改了之前的 Folio 命令' ;;
      'Prior release binary was modified') ui_key='先前版本的二进制文件已被修改' ;;
      'Prior release lacks integrity metadata') ui_key='之前的版本缺乏完整性元数据' ;;
      'Private data metadata requires the production data lock') ui_key='私有数据元数据需要生产数据锁' ;;
      'Private installation summary: %s\n') ui_key='私有安装摘要：%s\n' ;;
      'Privileged Agent deployment is only available in production; prefix mode changes no users/services') ui_key='特权代理部署仅在生产中可用；前缀模式不改变任何用户/服务' ;;
      'Privileged Agent remains stopped and needs recovery: %s.\nLatest authoritative host state is retained at %s; deployment snapshots remain at %s.\n') ui_key='特权代理仍处于停止状态并需要恢复：%s。最新的权威主机状态保留在%s；部署快照保留在 %s。\n' ;;
      'Production data/lock must not be symbolic links') ui_key='生产数据/锁不得是符号链接' ;;
      'Production installation requires root; use --prefix for local testing') ui_key='生产安装需要root；使用 --prefix 进行本地测试' ;;
      'Production installation requires systemd') ui_key='生产安装需要 systemd' ;;
      'Ready for your next step') ui_key='准备开始使用' ;;
      'Refusing to modify unmanaged directory %s') ui_key='拒绝修改非托管目录 %s' ;;
      'Refusing to use unmanaged directory %s') ui_key='拒绝使用非托管目录 %s' ;;
      'Refusing unmanaged Agent state directory') ui_key='拒绝非托管代理状态目录' ;;
      'Refusing unmanaged module directory: %s') ui_key='拒绝非托管模块目录：%s' ;;
      'Refusing unmanaged module home: %s') ui_key='拒绝非托管模块主目录：%s' ;;
      'Release SHA256SUMS.txt contains an invalid package checksum line.') ui_key='版本 SHA256SUMS.txt 包含无效的包校验和行。' ;;
      'Release SHA256SUMS.txt does not contain exactly one package checksum.') ui_key='版本 SHA256SUMS.txt 不包含一个包校验和。' ;;
      'Release archive contains a non-regular file') ui_key='发布存档包含非常规文件' ;;
      'Release build attestation is invalid') ui_key='发布版本证明无效' ;;
      'Release directories must not be symbolic links') ui_key='发布目录不能是符号链接' ;;
      'Release package is missing %s') ui_key='发布包丢失 %s' ;;
      'Release package is missing its executable') ui_key='发布包缺少可执行文件' ;;
      'Release platform does not match this machine') ui_key='发布平台与本机不匹配' ;;
      'Requested and packaged version differ') ui_key='请求的版本和打包的版本不同' ;;
      'Required command missing: %s') ui_key='缺少所需的命令：%s' ;;
      'Reviewed module selection: Agent=%s Software=%s Websites=%s Terminal=%s Source builds=%s\n') ui_key='本次模块选择（1 启用 / 0 不启用）：Agent=%s 软件=%s 网站=%s 终端=%s 源代码编译=%s\n' ;;
      'Reviewing the installation') ui_key='确认本次安装配置' ;;
      'Rolled back API to %s with its pre-upgrade data; intervening data retained in %s.\n') ui_key='将 API 及其升级前数据回滚到 %s；干预数据保留在 %s 中。\n' ;;
      'Server config is a dangling symbolic link') ui_key='服务器配置是一个悬挂的符号链接' ;;
      'Server lock changed while opening') ui_key='打开时服务器锁已更改' ;;
      'Server lock must be a regular file') ui_key='服务器锁必须是常规文件' ;;
      'Server lock must not be a symbolic link') ui_key='服务器锁不能是符号链接' ;;
      'Service diagnostics: sudo systemctl status powerlinux.service\n') ui_key='查看服务状态：sudo systemctl status powerlinux.service\n' ;;
      'Service health check failed') ui_key='服务健康检查失败' ;;
      'Service uses preserved root-owned %s/server.json; new installs default to loopback.\n') ui_key='服务使用保留的 root 拥有的 %s/server.json；新安装默认为环回。\n' ;;
      'Snapshot restore failed. Snapshot and journal retained in %s; service remains stopped.\n') ui_key='快照恢复失败。快照和日志保留在 %s 中；服务仍然停止。\n' ;;
      'Specify --archive or --version') ui_key='指定 --archive 或 --version' ;;
      'Starting services and checking real health') ui_key='启动服务并验证健康状态' ;;
      'Stop the prefix-mode server before modifying its installation') ui_key='在修改其安装之前停止前缀模式服务器' ;;
      'Supported architectures: amd64 and arm64') ui_key='支持的架构：amd64和arm64' ;;
      'Terminal workspace changed while opening') ui_key='打开时终端工作区发生变化' ;;
      'Terminal workspace has unmanaged owner') ui_key='终端工作区具有不受管理的所有者' ;;
      'Terminal workspace must not be a symbolic link') ui_key='终端工作区不能是符号链接' ;;
      'The operation did not finish. Read the messages above before retrying; do not delete recovery snapshots.\n') ui_key='本次操作未完成。请先查看上方原因及恢复状态，再决定是否重试；请勿删除恢复快照。\n' ;;
      'The package is ready. Start the command above, then open http://127.0.0.1:9000.\n') ui_key='安装包已就绪。运行上面的启动命令后，打开 http://127.0.0.1:9000。\n' ;;
      'The previous API release is restored and its health check passed. Review any separate Agent recovery message above.\n') ui_key='上一版本的 API 已恢复，并通过健康检查。Agent 如需单独恢复，请以上方提示为准。\n' ;;
      'The previous local release and data snapshot have been restored.\n') ui_key='上一版本的本地程序和数据快照已恢复。\n' ;;
      'The verified archive did not contain a regular install.sh.') ui_key='已验证的存档不包含常规 install.sh。' ;;
      'Then open http://127.0.0.1:9000 in your browser. Existing access settings remain unchanged.\n') ui_key='然后用浏览器打开 http://127.0.0.1:9000。已有安装继续使用原来的访问配置。\n' ;;
      'This one-command release installer supports Linux amd64.') ui_key='这一单命令发布安装程序支持 Linux amd64。' ;;
      'Unable to create the private data lock') ui_key='无法创建私有数据锁' ;;
      'Unable to restore prior deployment') ui_key='无法恢复之前的部署' ;;
      'Uninstalled service/current command. Data, configuration and release backups are retained:\n  %s\n  %s\n  %s\n') ui_key='已卸载的服务/当前命令。保留数据、配置和版本备份：%s %s %s\n' ;;
      'Unit must not be a symbolic link: %s') ui_key='单元不能是符号链接：%s' ;;
      'Unknown option: %s') ui_key='未知选项：%s' ;;
      'Unmount nested data mount before installation/restore: %s') ui_key='在安装/恢复之前卸载嵌套数据装载：%s' ;;
      'Unmount owned deployment tree before snapshot/restore: %s') ui_key='在快照/恢复之前卸载拥有的部署树：%s' ;;
      'Unsafe or unexpected archive member: %s') ui_key='不安全或意外的存档成员：%s' ;;
      'Use a dedicated installation directory') ui_key='使用专用安装目录' ;;
      'Version %s is already installed; data left unchanged.\n') ui_key='版本 %s 已经安装，无需重复安装；数据保持不变。\n' ;;
      'curl is required for HTTPS downloads') ui_key='HTTPS 下载需要使用curl' ;;
      'systemd is not running; --prefix is available for development') ui_key='systemd 没有运行； --prefix 可用于开发' ;;
    esac
  fi
  printf '%s' "$ui_key"
}
say() {
  local ui_format
  ui_format=$(ui_copy "$1"); shift
  printf -- "$ui_format" "$@"
  if [[ -n "$ui_log" ]]; then printf -- "$ui_format" "$@" >> "$ui_log" || true; fi
}
ui_phase() {
  local ui_number=$1 ui_title=$2
  if [[ ${FOLIO_BOOTSTRAP:-0} == 1 && "$ui_number" -lt 3 ]]; then return 0; fi
  printf '\n[%s/6] ' "$ui_number"
  say '%s (elapsed %ss)\n' "$(ui_copy "$ui_title")" "$((SECONDS-ui_started))"
}
ui_log_location() {
  if [[ -n "$ui_log" && "$ui_log_reported" == 0 ]]; then
    ui_log_reported=1
    say 'Private installation summary: %s\n' "$ui_log"
  fi
  return 0
}
ui_confirm() {
  # A curl | bash invocation reads answers from the controlling terminal,
  # never from the script stream. CI and detached sessions never wait for input.
  if (( !ui_assume_yes )) && [[ -t 1 && -t 2 && -r /dev/tty ]]; then
    local ui_answer
    while true; do
      say 'Continue? [Y/n] '
      if ! IFS= read -r ui_answer < /dev/tty; then
        say 'No answer received. Nothing has been changed.\n'; exit 1
      fi
      case "$ui_answer" in
        ''|y|Y|yes|YES|是) break ;;
        n|N|no|NO|否) say 'Cancelled before making changes.\n'; exit 0 ;;
        *) say 'Please answer Y or N.\n' ;;
      esac
    done
  fi
}
ui_begin() {
  if (( !ui_language_explicit && !ui_assume_yes )) && [[ -t 1 && -t 2 && -r /dev/tty ]]; then
    local ui_choice
    printf '语言 / Language: 1 中文 · 2 English [%s]: ' "$([[ "$ui_language" == zh-CN ]] && printf 1 || printf 2)"
    IFS= read -r ui_choice < /dev/tty || exit 1
    case "$ui_choice" in 1) ui_language=zh-CN ;; 2) ui_language=en ;; '') ;; *) printf 'Use 1 or 2 / 请输入 1 或 2\n' >&2; exit 2 ;; esac
  fi
  ui_log=$(mktemp "${TMPDIR:-/tmp}/folio-install-summary.XXXXXX")
  chmod 0600 "$ui_log"
  if [[ ${FOLIO_BOOTSTRAP:-0} != 1 ]]; then
    say 'Hello, I am Folio. I will guide you through this installation.\n'
  fi
}
# END GENERATED INSTALLER UI

usage() {
  if [[ "$ui_language" == zh-CN ]]; then
    cat <<'ZH'
Folio 安装向导
用法：install.sh --archive 文件 --sha256 可信摘要 [--prefix 绝对路径]
      install.sh --version 版本 --sha256 可信摘要
      install.sh --rollback | --uninstall
选项：--lang zh-CN|en  --yes / --non-interactive  --no-color
正式安装需要 root 和 systemd；--prefix 不创建系统用户或服务。
升级保留配置和数据；卸载保留数据、配置和版本备份。
高级模块：--enable-agent --agent-policy 私有策略文件
--enable-terminal / --enable-software / --enable-sites 需要 Agent。
--enable-source-build 还需要软件模块；源代码编译受策略控制。
--attestation 使用 GitHub CLI 验证构建证明。
请从可信发布渠道获得 SHA256 摘要。
ZH
    return
  fi
  cat <<'EOF'
Usage: install.sh --archive FILE --sha256 TRUSTED_SHA256 [--prefix ABSOLUTE_DIR]
       install.sh --version VERSION --sha256 TRUSTED_SHA256 [--prefix ABSOLUTE_DIR]
       install.sh --rollback [--prefix ABSOLUTE_DIR]
       install.sh --uninstall [--prefix ABSOLUTE_DIR]

Language: --lang zh-CN|en; automation: --yes / --non-interactive; --no-color.

Optional production deployment: --enable-agent --agent-policy ROOT_PRIVATE_JSON
--enable-terminal, --enable-software and --enable-sites create isolated module
users/directories; they require --enable-agent and an explicit trusted policy.
--enable-source-build also requires --enable-software and prepares a separate
compiler identity/cache; source builds remain explicitly controlled by policy.

Production requires root and running systemd; only Folio-owned paths,
user and unit are changed. --prefix is nonprivileged, creates no user/service,
and requires the caller to stop a running instance before update or rollback.
Data is preserved during uninstall. Delete preserved data manually after backup.
Remote packages are fetched from github.com/Aoody743/Folio over HTTPS.
--sha256 must be obtained through a trusted release channel; a same-channel
checksum alone does not authenticate a malicious publisher. --attestation
additionally verifies the GitHub build attestation with an installed gh CLI.
EOF
}
die() { printf 'Folio: ' >&2; say "$@" >&2; printf '\n' >&2; ui_log_location >&2; exit 1; }
archive= version= expected_hash= prefix= mode=install attestation=0
enable_agent=0 enable_terminal=0 enable_software=0 enable_sites=0 enable_source_build=0 agent_policy=
while (($#)); do
  case "$1" in
    --archive|--version|--sha256|--prefix|--agent-policy)
      (($# >= 2)) || die 'Missing value for %s' "$1" ;;
  esac
  case "$1" in
    --lang) shift 2 ;;
    --yes|--non-interactive|--no-color) shift ;;
    --archive) archive=${2:?missing archive}; shift 2 ;;
    --version) version=${2:?missing version}; shift 2 ;;
    --sha256) expected_hash=${2:?missing sha256}; shift 2 ;;
    --prefix) prefix=${2:?missing prefix}; shift 2 ;;
    --rollback) [[ "$mode" == install ]] || die 'Choose one operation'; mode=rollback; shift ;;
    --uninstall) [[ "$mode" == install ]] || die 'Choose one operation'; mode=uninstall; shift ;;
    --attestation) attestation=1; shift ;;
    --enable-agent) enable_agent=1; shift ;;
    --agent-policy) agent_policy=${2:?missing agent policy}; shift 2 ;;
    --enable-terminal) enable_terminal=1; shift ;;
    --enable-software) enable_software=1; shift ;;
    --enable-sites) enable_sites=1; shift ;;
    --enable-source-build) enable_source_build=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die 'Unknown option: %s' "$1" ;;
  esac
done
ui_begin
ui_phase "1" "Checking this server"
if (( enable_terminal || enable_software || enable_sites || enable_source_build )); then (( enable_agent )) || die 'Module deployment requires --enable-agent'; fi
if (( enable_source_build )); then (( enable_software )) || die '--enable-source-build requires --enable-software'; fi
if (( enable_agent )); then [[ -n "$agent_policy" ]] || die '--enable-agent requires an explicit --agent-policy'; fi
[[ -z "$agent_policy" || "$enable_agent" == 1 ]] || die '--agent-policy requires --enable-agent'
[[ "$mode" == install || "$enable_agent" == 0 ]] || die 'Deployment flags apply only to install/upgrade'
for command in sha256sum tar flock realpath cp mv find mktemp cmp; do command -v "$command" >/dev/null || die 'Required command missing: %s' "$command"; done
[[ $(uname -s) == Linux ]] || die 'Linux is required'
case "$(uname -m)" in x86_64) architecture=amd64 ;; aarch64|arm64) architecture=arm64 ;; *) die 'Supported architectures: amd64 and arm64' ;; esac
production=1
if [[ -n "$prefix" ]]; then
  production=0
  (( !enable_agent )) || die 'Privileged Agent deployment is only available in production; prefix mode changes no users/services'
  [[ "$prefix" == /* && "$prefix" =~ ^/[A-Za-z0-9_./-]+$ ]] || die 'Prefix must be an absolute path without spaces or shell metacharacters'
  [[ ! -L "$prefix" ]] || die 'Prefix must not be a symbolic link'
  base=$(realpath -m -- "$prefix")
  [[ "$base" != / && "$base" != /usr && "$base" != /var && "$base" != /opt && "$base" != /home && "$base" != /tmp && "$base" != /workspace ]] || die 'Use a dedicated installation directory'
  data="$base/data"
  config="$base/config"
else
  (( EUID == 0 )) || die 'Production installation requires root; use --prefix for local testing'
  command -v systemctl >/dev/null || die 'Production installation requires systemd'
  [[ -d /run/systemd/system ]] || die 'systemd is not running; --prefix is available for development'
  base=/opt/powerlinux
  data=/var/lib/powerlinux
  config=/etc/powerlinux
fi
unit=/etc/systemd/system/powerlinux.service
agent_unit=/etc/systemd/system/powerlinux-agent.service
agent_state=/var/lib/powerlinux-agent
agent_socket=/run/powerlinux-agent/agent.sock
marker='PowerLinux rewrite installation v1'
stage= transaction= active_transaction=0 previous_target= service_stopped=0
production_data_locked=0
health_binary=
assert_no_nested_mounts() {
  local mount_id mount_parent mount_device mount_root mount_path remainder
  while read -r mount_id mount_parent mount_device mount_root mount_path remainder; do
    [[ "$mount_path" != "$data/"* ]] || die 'Unmount nested data mount before installation/restore: %s' "$mount_path"
  done < /proc/self/mountinfo
}
acquire_production_data_lock() {
  (( production && !production_data_locked )) || return 0
  [[ -d "$data" && ! -L "$data" && ! -L "$data/server.lock" ]] || die 'Production data/lock must not be symbolic links'
  if [[ ! -e "$data/server.lock" ]]; then
    # noclobber uses exclusive creation and refuses an entry created between
    # the preceding check and open. Do not truncate a user-controlled name.
    (set -o noclobber; : > "$data/server.lock") || die 'Unable to create the private data lock'
  fi
  [[ -f "$data/server.lock" && ! -L "$data/server.lock" ]] || die 'Server lock must be a regular file'
  # Read-only open never truncates the lock. Verify the opened inode before
  # flock/chown so permission changes cannot follow a substituted link.
  exec 8<"$data/server.lock"
  local lock_handle="/proc/$$/fd/8"
  [[ -f "$lock_handle" && $(readlink "$lock_handle") == "$data/server.lock" && $(stat -Lc '%d:%i' "$lock_handle") == "$(stat -c '%d:%i' "$data/server.lock")" ]] || die 'Server lock changed while opening'
  flock -n 8 || die 'A server or local maintenance command still holds the production data lock'
  chown powerlinux:powerlinux "$lock_handle"
  chmod 0600 "$lock_handle"
  production_data_locked=1
}
prepare_production_data_metadata() {
  (( production )) || return 0
  (( production_data_locked )) || die 'Private data metadata requires the production data lock'
  chown powerlinux:powerlinux "$data"
  chmod 0700 "$data"
  # The ordinary API user owns data entries. Construct a marker in the
  # root-owned installation directory, then atomically replace its name;
  # never open an API-swappable marker path for privileged truncation/chown.
  local marker_stage
  marker_stage=$(mktemp "$base/.data-marker.XXXXXX")
  printf '%s\n' "$marker" > "$marker_stage"
  chmod 0600 "$marker_stage"
  chown powerlinux:powerlinux "$marker_stage"
  mv -Tf -- "$marker_stage" "$data/.powerlinux-managed"
  printf '%s\n' "$marker" > "$config/.powerlinux-managed"
}
release_production_data_lock() {
  (( production_data_locked )) || return 0
  flock -u 8
  exec 8<&-
  production_data_locked=0
}
cleanup() {
  local result=$?
  trap - EXIT INT TERM
  if (( active_transaction )); then
    say 'Installation failed; restoring the prior release and data snapshot.\n' >&2
    if (( production )); then
      systemctl stop powerlinux.service >/dev/null 2>&1 || true
      [[ ! -f "$agent_unit" ]] || systemctl stop powerlinux-agent.service >/dev/null 2>&1 || true
      acquire_production_data_lock
    fi
    if [[ -d "$transaction/data" ]]; then
      assert_no_nested_mounts
      mkdir -p "$transaction/failed-data"
      if ! cp -a "$data/." "$transaction/failed-data/"; then
        say 'Failed to preserve current data (check disk capacity). Recovery journal retained; inspect %s before retrying.\n' "$transaction" >&2
        [[ -n "$stage" ]] && rm -rf -- "$stage"
        exit 1
      fi
      find "$data" -mindepth 1 -maxdepth 1 ! -name server.lock -exec rm -rf -- {} +
      if ! cp -a "$transaction/data/." "$data/"; then
        say 'Snapshot restore failed. Snapshot and journal retained in %s; service remains stopped.\n' "$transaction" >&2
        [[ -n "$stage" ]] && rm -rf -- "$stage"
        exit 1
      fi
    fi
    if (( production )) && ! restore_deployment "$transaction"; then
      say 'Deployment restore failed; recovery journal retained in %s. Services remain stopped.\n' "$transaction" >&2
      exit 1
    fi
    if [[ -n "$previous_target" ]]; then
      ln -s "$previous_target" "$base/.current-restore-$$"
      mv -Tf -- "$base/.current-restore-$$" "$base/current" || result=1
      if (( production )); then
        # Probe the restored executable's own contract, never the failed
        # candidate's health implementation. Keep the journal if recovery
        # itself is not healthy rather than declaring a completed rollback.
        health_binary="$base/current/powerlinux"
        release_production_data_lock
        if ! systemctl start powerlinux.service || ! health; then
          systemctl stop powerlinux.service >/dev/null 2>&1 || true
          [[ ! -f "$agent_unit" ]] || mark_agent_recovery 'Restored API did not become healthy; deployment recovery is incomplete'
          say 'Prior API recovery failed its real health check. Recovery journal retained in %s; services remain stopped.\n' "$transaction" >&2
          exit 1
        fi
        start_agent_or_preserve_recovery "$base/current/powerlinux" || result=1
        say 'The previous API release is restored and its health check passed. Review any separate Agent recovery message above.\n' >&2
      else
        say 'The previous local release and data snapshot have been restored.\n' >&2
      fi
    else
      rm -f -- "$base/current"
      if (( production )); then
        systemctl disable powerlinux.service >/dev/null 2>&1 || true
        [[ ! -f "$agent_unit" ]] || mark_agent_recovery 'First installation failed; no API release is active'
      fi
    fi
    rm -f -- "$base/pending-transaction"
    [[ -n "$transaction" ]] && say 'Preserved failure evidence: %s\n' "$transaction" >&2
  elif (( service_stopped && production )) && [[ -n "$previous_target" ]]; then
    # A snapshot/pre-switch prerequisite can fail after the old service stops.
    # No data replacement occurred, so restart that unchanged release.
    release_production_data_lock
    [[ ! -f "$agent_unit" ]] || systemctl start powerlinux-agent.service || result=1
    systemctl start powerlinux.service || result=1
  fi
  if (( result != 0 )); then
    say 'The operation did not finish. Read the messages above before retrying; do not delete recovery snapshots.\n' >&2
    ui_log_location >&2
  fi
  [[ -n "$stage" ]] && rm -rf -- "$stage"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

assert_owned_tree_unmounted() {
  local tree=$1 mount_id mount_parent mount_device mount_root mount_path remainder
  while read -r mount_id mount_parent mount_device mount_root mount_path remainder; do
    [[ "$mount_path" != "$tree" && "$mount_path" != "$tree/"* ]] || die 'Unmount owned deployment tree before snapshot/restore: %s' "$mount_path"
  done < /proc/self/mountinfo
}
snapshot_deployment() {
  assert_owned_tree_unmounted "$config"
  cp -a -- "$config" "$transaction/config"
  if [[ -d "$agent_state" ]]; then
    assert_owned_tree_unmounted "$agent_state"
    cp -a -- "$agent_state" "$transaction/agent-state"
  fi
  [[ ! -f "$unit" ]] || cp -a -- "$unit" "$transaction/api-unit"
  [[ ! -f "$agent_unit" ]] || cp -a -- "$agent_unit" "$transaction/agent-unit"
  [[ ! -f "$base/agent-recovery-required" ]] || cp -a -- "$base/agent-recovery-required" "$transaction/agent-recovery-required"
  printf '2\n' > "$transaction/deployment-snapshot"
}
restore_deployment() {
  local restore_from=$1 failure_evidence preserve_agent_policy=1
  # Older phase-one snapshots intentionally contain only the API data/link.
  [[ -f "$restore_from/deployment-snapshot" ]] || return 0
  [[ $(cat "$restore_from/deployment-snapshot") == 2 ]] || return 1
  [[ -d "$restore_from/config" && ! -L "$restore_from/config" ]] || return 1
  # Only a positively recorded pre-start failure permits policy rollback.
  # Once an Agent was attempted, its constructor may already have recovered
  # real host effects. Unknown old journals conservatively retain authority.
  if [[ "${2:-}" != preserve-agent-policy && -f "$restore_from/candidate-agent-state" && $(cat "$restore_from/candidate-agent-state") == not_started ]]; then
    preserve_agent_policy=0
  fi
  failure_evidence=$(mktemp -d "$transaction/deployment-before-restore.XXXXXX")
  assert_owned_tree_unmounted "$config"
  cp -a -- "$config" "$failure_evidence/config"
  find "$config" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
  cp -a -- "$restore_from/config/." "$config/"
  chmod --reference="$restore_from/config" "$config"
  chown --reference="$restore_from/config" "$config"
  # The latest trusted Agent policy accompanies the authoritative host ledger.
  # Reverting it blindly could grant obsolete capabilities or orphan modules
  # installed after the API snapshot. Check old-binary compatibility afterward.
  if (( preserve_agent_policy )) && [[ -f "$failure_evidence/config/agent-policy.json" && ! -L "$failure_evidence/config/agent-policy.json" ]]; then
    cp -a -- "$failure_evidence/config/agent-policy.json" "$config/agent-policy.json"
  fi
  if [[ -d "$agent_state" ]]; then
    assert_owned_tree_unmounted "$agent_state"
    cp -a -- "$agent_state" "$failure_evidence/agent-state"
  fi
  # Receipts/plans describe real host effects. New software, keys or completed
  # recovery cannot be undone by copying old bookkeeping over the current host.
  # Keep the latest root-owned state; snapshots remain for deliberate recovery.
  if [[ -f "$restore_from/api-unit" ]]; then cp -a -- "$restore_from/api-unit" "$unit"; else systemctl disable powerlinux.service >/dev/null 2>&1 || true; rm -f -- "$unit"; fi
  if [[ -f "$restore_from/agent-unit" ]]; then
    cp -a -- "$restore_from/agent-unit" "$agent_unit"
  elif (( !preserve_agent_policy )); then
    if [[ -f "$agent_unit" ]]; then
      systemctl disable powerlinux-agent.service >/dev/null 2>&1 || true
      rm -f -- "$agent_unit"
    fi
  fi
  if (( !preserve_agent_policy )); then
    if [[ -f "$restore_from/agent-recovery-required" ]]; then
      cp -a -- "$restore_from/agent-recovery-required" "$base/agent-recovery-required"
    else
      rm -f -- "$base/agent-recovery-required"
    fi
  fi
  systemctl daemon-reload
}
mark_agent_recovery() {
  local reason=$1
  systemctl stop powerlinux-agent.service >/dev/null 2>&1 || true
  systemctl disable powerlinux-agent.service >/dev/null 2>&1 || true
  printf '%s\n' "$reason" > "$base/agent-recovery-required"
  chmod 0600 "$base/agent-recovery-required"
  say 'Privileged Agent remains stopped and needs recovery: %s.\nLatest authoritative host state is retained at %s; deployment snapshots remain at %s.\n' "$reason" "$agent_state" "$transaction" >&2
}
start_agent_or_preserve_recovery() {
  local selected_binary=$1
  [[ -f "$agent_unit" ]] || return 0
  if ! "$selected_binary" agent --policy "$config/agent-policy.json" --check-policy >/dev/null 2>&1; then
    mark_agent_recovery 'Selected binary is incompatible with the latest trusted Agent policy'
    return 0
  fi
  if ! systemctl start powerlinux-agent.service; then
    mark_agent_recovery 'Agent startup failed; use a compatible version to inspect preserved host transactions'
    return 0
  fi
  if ! agent_health "$selected_binary"; then
    mark_agent_recovery 'Agent did not become ready with the selected policy/version'
    return 0
  fi
  systemctl enable powerlinux-agent.service
  rm -f -- "$base/agent-recovery-required"
}
module_user() {
  local account=$1 home_dir=$2 marker_stage
  if getent passwd "$account" >/dev/null; then
    [[ $(getent passwd "$account" | cut -d: -f6) == "$home_dir" && $(getent passwd "$account" | cut -d: -f7) =~ /(nologin|false)$ ]] || die 'Existing %s account has unmanaged home/shell' "$account"
  else
    useradd --system --user-group --home-dir "$home_dir" --no-create-home --shell /usr/sbin/nologin "$account"
  fi
  [[ $(id -u "$account") != 0 && $(id -g "$account") != 0 && $(id -u "$account") != "$(id -u powerlinux)" && $(id -g "$account") != "$(id -g powerlinux)" ]] || die 'Module account must have a non-root UID/GID distinct from the API'
  [[ ! -L "$home_dir" ]] || die 'Module home is a symbolic link: %s' "$home_dir"
  if [[ -e "$home_dir" ]]; then
    [[ -d "$home_dir" && ( $(stat -c '%u' "$home_dir") == 0 || $(stat -c '%u' "$home_dir") == "$(id -u "$account")" ) ]] || die 'Module home has an unmanaged owner/type: %s' "$home_dir"
  fi
  if [[ -d "$home_dir" && -n $(find "$home_dir" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
    [[ -f "$home_dir/.powerlinux-managed" && ! -L "$home_dir/.powerlinux-managed" && $(cat "$home_dir/.powerlinux-managed") == "$marker" ]] || die 'Refusing unmanaged module home: %s' "$home_dir"
  fi
  mkdir -p "$home_dir"
  chmod 0700 "$home_dir"
  chown "$account:$account" "$home_dir"
  # The module user owns its home and can rename its entries. Create metadata
  # in the root-private staging directory, then atomically replace the name;
  # never truncate/chown a user-swappable marker through a symlink.
  marker_stage=$(mktemp "$stage/.module-marker.XXXXXX")
  printf '%s\n' "$marker" > "$marker_stage"
  chown "$account:$account" "$marker_stage"
  mv -Tf -- "$marker_stage" "$home_dir/.powerlinux-managed"
}
owned_module_directory() {
  local path=$1 permissions=$2
  [[ ! -L "$path" ]] || die 'Module directory is a symbolic link: %s' "$path"
  if [[ -d "$path" && -n $(find "$path" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
    [[ -f "$path/.powerlinux-managed" && ! -L "$path/.powerlinux-managed" && $(cat "$path/.powerlinux-managed") == "$marker" && $(stat -c '%u' "$path") == 0 ]] || die 'Refusing unmanaged module directory: %s' "$path"
  fi
  mkdir -p "$path"
  chown root:root "$path"
  chmod "$permissions" "$path"
  printf '%s\n' "$marker" > "$path/.powerlinux-managed"
}
require_agent_policy_source() {
  local parent permissions
  [[ "$agent_policy" == /* && -f "$agent_policy" && ! -L "$agent_policy" && $(stat -c '%u' "$agent_policy") == 0 ]] || die 'Agent policy must be an absolute root-owned regular file'
  [[ $(stat -c '%a' "$agent_policy") == 600 || $(stat -c '%a' "$agent_policy") == 400 ]] || die 'Agent policy must be private (0600 or 0400)'
  # A trusted file's name also needs protection against replacement by a
  # different UID. Root-owned sticky /tmp protects a root-owned child entry.
  parent=$(dirname -- "$agent_policy")
  while :; do
    [[ -d "$parent" && ! -L "$parent" && $(stat -c '%u' "$parent") == 0 ]] || die 'Agent policy ancestors must be real root-owned directories'
    permissions=$(stat -c '%a' "$parent")
    (( (8#$permissions & 0022) == 0 || (8#$permissions & 01000) != 0 )) || die 'Agent policy ancestors must not permit untrusted replacement'
    [[ "$parent" != / ]] || break
    parent=$(dirname -- "$parent")
  done
}
configure_deployment() {
  local server_stage api_unit_stage agent_unit_stage
  chown root:powerlinux "$config"
  chmod 0750 "$config"
  if [[ ! -e "$config/server.json" ]]; then
    [[ ! -L "$config/server.json" ]] || die 'Server config is a dangling symbolic link'
    server_stage=$(mktemp "$config/.server-config.XXXXXX")
    cat > "$server_stage" <<'EOF'
{"version":1,"listen":"127.0.0.1:9000","allowed_origins":[],"secure_cookies":false}
EOF
    chown root:powerlinux "$server_stage"
    chmod 0640 "$server_stage"
    mv -f -- "$server_stage" "$config/server.json"
  fi
  if (( enable_agent )); then
    require_agent_policy_source
    owned_module_directory "$agent_state" 0700
    for module in system software sites terminal; do
      [[ ! -L "$agent_state/$module" ]] || die 'Agent module state must not be a symbolic link'
      mkdir -p "$agent_state/$module"
      chown root:root "$agent_state/$module"
      chmod 0700 "$agent_state/$module"
    done
    if (( enable_terminal )); then
      module_user powerlinux-terminal /var/lib/powerlinux-terminal
      [[ ! -L /var/lib/powerlinux-terminal/workspace ]] || die 'Terminal workspace must not be a symbolic link'
      mkdir -p /var/lib/powerlinux-terminal/workspace
      # Bind permission changes to the opened directory inode, rather than a
      # path that the isolated user could replace between checks and chmod.
      local workspace_fd workspace_handle terminal_uid
      exec {workspace_fd}</var/lib/powerlinux-terminal/workspace
      workspace_handle="/proc/$$/fd/$workspace_fd"
      terminal_uid=$(id -u powerlinux-terminal)
      [[ -d "$workspace_handle" && $(readlink "$workspace_handle") == /var/lib/powerlinux-terminal/workspace ]] || die 'Terminal workspace changed while opening'
      [[ $(stat -Lc '%u' "$workspace_handle") == 0 || $(stat -Lc '%u' "$workspace_handle") == "$terminal_uid" ]] || die 'Terminal workspace has unmanaged owner'
      chmod 0700 "$workspace_handle"
      chown powerlinux-terminal:powerlinux-terminal "$workspace_handle"
      exec {workspace_fd}<&-
    fi
    if (( enable_software )); then
      module_user powerlinux-services /var/lib/powerlinux-services
      owned_module_directory /opt/powerlinux-software 0755
      owned_module_directory /srv/powerlinux-apps 0755
    fi
    if (( enable_source_build )); then
      module_user powerlinux-build /var/lib/powerlinux-build
      # Neither the API nor a running shell/service may share the compiler's
      # identity and alter outputs that will later be promoted to root paths.
      local isolated_account
      for isolated_account in powerlinux powerlinux-services powerlinux-terminal; do
        if getent passwd "$isolated_account" >/dev/null; then
          [[ $(id -u powerlinux-build) != "$(id -u "$isolated_account")" && $(id -g powerlinux-build) != "$(id -g "$isolated_account")" ]] || die 'Compiler UID/GID must be distinct from API, runtime and terminal accounts'
        fi
      done
      owned_module_directory /var/cache/powerlinux-build 0711
    fi
    if (( enable_sites )); then owned_module_directory /srv/powerlinux-sites 0755; fi
    # Copy only the reviewed trusted document; modules and allowed operations
    # are never enabled by inspecting or rewriting arbitrary JSON in shell.
    local policy_stage
    policy_stage=$(mktemp "$config/.agent-policy.XXXXXX")
    cp -- "$agent_policy" "$policy_stage"
    chmod 0600 "$policy_stage"
    chown root:root "$policy_stage"
    "$stage/package/powerlinux" agent --policy "$policy_stage" --check-policy || die 'Agent deployment policy failed static validation'
    mv -f -- "$policy_stage" "$config/agent-policy.json"
    agent_unit_stage=$(mktemp /etc/systemd/system/.powerlinux-agent.XXXXXX)
    cat > "$agent_unit_stage" <<EOF
# Managed by Folio rewrite installer
[Unit]
Description=Folio typed privileged Agent
After=network.target

[Service]
Type=simple
User=root
Group=root
ExecStart=$base/current/powerlinux agent --policy $config/agent-policy.json --socket $agent_socket
UMask=0077
RuntimeDirectory=powerlinux-agent
RuntimeDirectoryMode=0755
RuntimeDirectoryPreserve=no
Restart=on-failure
RestartSec=3
TimeoutStopSec=25
PrivateTmp=true
ProtectHome=true

[Install]
WantedBy=multi-user.target
EOF
    chmod 0644 "$agent_unit_stage"
    mv -f -- "$agent_unit_stage" "$agent_unit"
  fi
}
# Recovery traps need these helpers even when an interrupted journal is found
# before normal install/rollback execution reaches service setup.

health() {
  local attempt response installed_version
  installed_version=$(cat "$base/current/VERSION")
  for ((attempt=0; attempt<30; attempt++)); do
    if systemctl is-active --quiet powerlinux.service; then
      if [[ -f "$config/server.json" ]]; then
        "${health_binary:-$base/current/powerlinux}" healthcheck --server-config "$config/server.json" --expected-version "$installed_version" --timeout 1s >/dev/null 2>&1 && return 0
      else
        # Only legacy phase-one packages lacked the typed healthcheck command
        # and configuration. Their unit is fixed to this verified loopback URL.
        if "${health_binary:-$base/current/powerlinux}" healthcheck --listen 127.0.0.1:9000 --expected-version "$installed_version" --timeout 1s >/dev/null 2>&1; then return 0; fi
        command -v curl >/dev/null || return 1
        response=$(curl --noproxy '*' --fail --silent --max-time 1 http://127.0.0.1:9000/healthz) || response=
        if [[ "$response" == "{\"status\":\"ok\",\"version\":\"$installed_version\",\"ready\":true}" ]]; then return 0; fi
      fi
    fi
    sleep 0.2
  done
  return 1
}
agent_health() {
  local selected_binary=$1 attempt policy_hash selected_version
  command -v runuser >/dev/null || return 1
  policy_hash=$(sha256sum -- "$config/agent-policy.json"); policy_hash=${policy_hash%% *}
  selected_version=$("$selected_binary" version)
  for ((attempt=0; attempt<30; attempt++)); do
    if systemctl is-active --quiet powerlinux-agent.service &&
       runuser -u powerlinux -- "$selected_binary" agent-healthcheck --socket "$agent_socket" \
         --expected-version "$selected_version" --expected-policy-sha256 "$policy_hash" --timeout 1s >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}
stop_service() {
  if (( production )) && [[ -f "$unit" ]]; then
    systemctl stop powerlinux.service
    service_stopped=1
  fi
  if (( production )) && [[ -f "$agent_unit" ]]; then systemctl stop powerlinux-agent.service; fi
  acquire_production_data_lock
  prepare_production_data_metadata
}
start_service() {
  if (( production )); then ui_phase "5" "Starting services and checking real health";
  else ui_phase "5" "Preparing the local start command"; fi
  if (( production )); then
    release_production_data_lock
    systemctl start powerlinux.service
    health || die 'Service health check failed'
    if [[ "$mode" == rollback ]]; then start_agent_or_preserve_recovery "$base/current/powerlinux"; else
      if [[ -f "$agent_unit" ]]; then
        if ! "$base/current/powerlinux" agent --policy "$config/agent-policy.json" --check-policy; then mark_agent_recovery 'Preserved Agent policy is incompatible with candidate binary'; die 'Preserved Agent policy is incompatible with the candidate binary'; fi
        if (( active_transaction )); then
          printf 'attempted\n' > "$transaction/.candidate-agent-state"
          mv -f -- "$transaction/.candidate-agent-state" "$transaction/candidate-agent-state"
          sync -f "$transaction/candidate-agent-state"
        fi
        systemctl start powerlinux-agent.service
        if ! agent_health "$base/current/powerlinux"; then mark_agent_recovery 'Candidate Agent did not become ready'; die 'Agent service did not become ready with candidate version/policy'; fi
        rm -f -- "$base/agent-recovery-required"
      fi
    fi
    service_stopped=0
  fi
}
snapshot() {
  assert_no_nested_mounts
  transaction="$base/backups/$(date -u +%Y%m%dT%H%M%S)-$$"
  mkdir -p "$transaction/data"
  cp -a "$data/." "$transaction/data/"
  rm -f -- "$transaction/data/server.lock"
  printf '%s\n' "$previous_target" > "$transaction/previous-target"
  if (( production )); then snapshot_deployment; fi
  if (( production )); then
    if [[ "$mode" == rollback ]]; then printf 'preserve\n'; else printf 'not_started\n'; fi > "$transaction/candidate-agent-state"
  fi
  active_transaction=1
  printf '%s\n' "backups/$(basename "$transaction")" > "$base/pending-transaction.tmp"
  mv -f -- "$base/pending-transaction.tmp" "$base/pending-transaction"
}

managed() { [[ -f "$base/.powerlinux-install" && ! -L "$base/.powerlinux-install" && $(cat "$base/.powerlinux-install") == "$marker" ]]; }
check_release() {
  local release_dir=$1
  [[ -f "$release_dir/powerlinux.sha256" ]] || die 'Prior release lacks integrity metadata'
  (cd "$release_dir" && sha256sum --check --status powerlinux.sha256) || die 'Prior release binary was modified'
  if [[ -e "$release_dir/folio" ]]; then
    [[ -f "$release_dir/folio.sha256" ]] || die 'Prior Folio command lacks integrity metadata'
    (cd "$release_dir" && sha256sum --check --status folio.sha256) || die 'Prior Folio command was modified'
    cmp -s "$release_dir/powerlinux" "$release_dir/folio" || die 'Prior Folio command differs from its compatibility binary'
  fi
}
[[ ! -L "$base" && ! -L "$data" && ! -L "$config" ]] || die 'Managed paths must not be symbolic links'
for path in "$base/releases" "$base/backups" "$base/.install-lock" "$base/pending-transaction" "$base/rollback-target" "$base/agent-recovery-required" "$data/.powerlinux-managed" "$config/.powerlinux-managed"; do
  [[ ! -L "$path" ]] || die 'Managed metadata must not be a symbolic link: %s' "$path"
done
if [[ -e "$base" ]] && ! managed; then
  [[ "$mode" == install && -d "$base" && -z $(find "$base" -mindepth 1 -maxdepth 1 -print -quit) ]] || die 'Refusing to modify unmanaged directory %s' "$base"
fi
if (( production )); then
  for path in "$base" "$config"; do
    if [[ -e "$path" ]]; then
      [[ -d "$path" && ! -L "$path" && $(stat -c '%u' "$path") == 0 ]] || die 'Installation/configuration paths must be root-owned real directories: %s' "$path"
      directory_permissions=$(stat -c '%a' "$path")
      (( (8#$directory_permissions & 0022) == 0 )) || die 'Installation/configuration directory permits untrusted replacement: %s' "$path"
    fi
  done
  for path in "$data" "$config"; do
    if [[ -d "$path" && -n $(find "$path" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
      [[ -f "$path/.powerlinux-managed" && ! -L "$path/.powerlinux-managed" && $(cat "$path/.powerlinux-managed") == "$marker" ]] || die 'Refusing to use unmanaged directory %s' "$path"
    fi
  done
fi
if [[ "$mode" != install ]]; then managed || die 'No managed installation found'; fi
if (( production )); then
  for owned_unit in "$unit" "$agent_unit"; do
    [[ ! -L "$owned_unit" ]] || die 'Unit must not be a symbolic link: %s' "$owned_unit"
    if [[ -e "$owned_unit" ]]; then
      [[ -f "$owned_unit" && $(stat -c '%u' "$owned_unit") == 0 ]] || die 'Existing unit must be a root-owned regular file: %s' "$owned_unit"
      unit_permissions=$(stat -c '%a' "$owned_unit")
      (( (8#$unit_permissions & 0022) == 0 )) || die 'Existing unit permits untrusted modification: %s' "$owned_unit"
      managed_unit_header=$(head -1 "$owned_unit")
      [[ "$managed_unit_header" == '# Managed by Folio rewrite installer' || "$managed_unit_header" == '# Managed by PowerLinux rewrite installer' ]] || die 'Existing unit is unmanaged: %s' "$owned_unit"
    fi
    # A vendor unit under /usr/lib or /lib is also an existing deployment;
    # do not silently override it merely because our /etc filename is absent.
    if [[ "$owned_unit" == "$unit" || -f "$owned_unit" || "$enable_agent" == 1 ]]; then
      loaded_fragment=$(systemctl show "$(basename "$owned_unit")" --property=FragmentPath --value 2>/dev/null || true)
      [[ -z "$loaded_fragment" || "$loaded_fragment" == "$owned_unit" ]] || die 'Existing unit resolves to an unmanaged location: %s' "$loaded_fragment"
    fi
  done
  if [[ -e "$agent_state" ]]; then
    [[ ! -L "$agent_state" && -d "$agent_state" && $(stat -c '%u:%a' "$agent_state") == 0:700 && -f "$agent_state/.powerlinux-managed" && ! -L "$agent_state/.powerlinux-managed" && $(cat "$agent_state/.powerlinux-managed") == "$marker" ]] || die 'Refusing unmanaged Agent state directory'
  fi
fi

if (( production && enable_agent )); then require_agent_policy_source; fi
if (( production )) && { (( enable_agent )) || [[ -f "$agent_unit" ]]; }; then command -v sync >/dev/null || die 'Agent transactions require the coreutils sync command'; fi

if [[ "$mode" == install ]]; then
  ui_phase "2" "Downloading and verifying the package"
  [[ "$expected_hash" =~ ^[a-fA-F0-9]{64}$ ]] || die 'A trusted SHA256 pin is required (--sha256)'
  [[ -z "$version" || "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$ ]] || die 'Invalid version'
  stage=$(mktemp -d "${TMPDIR:-/tmp}/powerlinux-install.XXXXXX")
  if [[ -z "$archive" ]]; then
    [[ -n "$version" ]] || die 'Specify --archive or --version'
    command -v curl >/dev/null || die 'curl is required for HTTPS downloads'
    archive="$stage/release.tar.gz"
    curl --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 2 \
      --output "$archive" "https://github.com/Aoody743/Folio/releases/download/$version/folio-$version-linux-$architecture.tar.gz"
  fi
  [[ -f "$archive" ]] || die 'Archive does not exist'
  actual_hash=$(sha256sum -- "$archive"); actual_hash=${actual_hash%% *}
  [[ "${actual_hash,,}" == "${expected_hash,,}" ]] || die 'Archive SHA256 does not match the trusted pin'
  if (( attestation )); then
    command -v gh >/dev/null || die '--attestation requires GitHub CLI'
    gh attestation verify "$archive" --repo Aoody743/Folio --signer-workflow Aoody743/Folio/.github/workflows/rewrite-ci.yml || die 'Release build attestation is invalid'
  fi
  # The release format is deliberately flat and permits regular files only.
  # Reject links, devices, absolute/parent paths, duplicate and unknown members.
  declare -A seen=()
  while IFS= read -r member; do
    case "$member" in folio|powerlinux|VERSION|PLATFORM|install.sh|BUILDINFO|README.txt) ;;
      *) die 'Unsafe or unexpected archive member: %s' "$member" ;; esac
    [[ -z "${seen[$member]:-}" ]] || die 'Duplicate archive member: %s' "$member"
    seen[$member]=1
  done < <(tar -tzf "$archive")
  [[ ${#seen[@]} == 6 || ${#seen[@]} == 7 ]] || die 'Incomplete release package'
  for member in VERSION PLATFORM install.sh BUILDINFO README.txt; do [[ -n "${seen[$member]:-}" ]] || die 'Release package is missing %s' "$member"; done
  [[ -n "${seen[folio]:-}${seen[powerlinux]:-}" ]] || die 'Release package is missing its executable'
  while IFS= read -r member; do [[ "$member" == -* ]] || die 'Release archive contains a non-regular file'; done < <(tar -tvzf "$archive")
  mkdir "$stage/package"
  tar --no-same-owner --no-same-permissions -xzf "$archive" -C "$stage/package"
  package_version=$(cat "$stage/package/VERSION")
  [[ "$package_version" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$ ]] || die 'Invalid packaged version'
  [[ -z "$version" || "$version" == "$package_version" ]] || die 'Requested and packaged version differ'
  version=$package_version
  [[ $(cat "$stage/package/PLATFORM") == "linux-$architecture" ]] || die 'Release platform does not match this machine'
  if [[ ! -e "$stage/package/powerlinux" ]]; then cp "$stage/package/folio" "$stage/package/powerlinux"; fi
  if [[ ! -e "$stage/package/folio" ]]; then cp "$stage/package/powerlinux" "$stage/package/folio"; fi
  chmod 0755 "$stage/package/folio" "$stage/package/powerlinux"
  [[ $("$stage/package/powerlinux" version) == "$version" ]] || die 'Binary and package version differ'
  [[ $("$stage/package/folio" version) == "$version" ]] && cmp -s "$stage/package/powerlinux" "$stage/package/folio" || die 'Folio and compatibility binaries differ'
  health_binary="$stage/package/powerlinux"
fi

ui_phase "3" "Reviewing the installation"
if [[ "$mode" == install ]]; then
  if [[ -L "$base/current" ]]; then
    say 'I found an existing installation. Configuration and data will be preserved during this upgrade.\n'
  else
    say 'A new Folio home is ready. The panel will listen on localhost by default.\n'
  fi
fi
say 'Operation: %s | Installation: %s | Data: %s\n' "$mode" "$base" "$data"
if (( enable_agent )); then
  say 'Reviewed module selection: Agent=%s Software=%s Websites=%s Terminal=%s Source builds=%s\n' "$enable_agent" "$enable_software" "$enable_sites" "$enable_terminal" "$enable_source_build"
else
  say 'Existing permissions and module settings will be preserved.\n'
fi
ui_confirm
ui_phase "4" "Installing with a recovery snapshot"
mkdir -p "$base"
exec 9>"$base/.install-lock"
flock -n 9 || die 'Another installer is active'
if ! managed; then printf '%s\n' "$marker" > "$base/.powerlinux-install"; fi
if (( !production )) && [[ -f "$data/server.lock" ]]; then
  [[ ! -L "$data/server.lock" ]] || die 'Server lock must not be a symbolic link'
  exec 8>"$data/server.lock"
  flock -n 8 || die 'Stop the prefix-mode server before modifying its installation'
fi
if [[ -f "$base/pending-transaction" ]]; then
  # The snapshot is completed before this journal is written. A killed upgrade
  # is recovered before a new operation, preserving its post-upgrade data.
  pending=$(cat "$base/pending-transaction")
  [[ "$pending" =~ ^backups/[A-Za-z0-9._-]+$ && -d "$base/$pending/data" ]] || die 'Invalid interrupted-install journal; inspect backups before continuing'
  previous_target=$(cat "$base/$pending/previous-target")
  [[ -z "$previous_target" || "$previous_target" =~ ^releases/[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$ ]] || die 'Invalid interrupted-install prior release'
  [[ -z "$previous_target" || -d "$base/$previous_target" ]] || die 'Interrupted-install prior release is missing'
  if [[ -n "$previous_target" ]]; then check_release "$base/$previous_target"; fi
  transaction="$base/$pending"
  active_transaction=1
  die 'Interrupted installation detected. Recovery is being performed; rerun the requested command afterward'
fi
if [[ -L "$base/current" ]]; then
  previous_target=$(readlink "$base/current")
  [[ "$previous_target" =~ ^releases/[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$ && -d "$base/$previous_target" ]] || die 'Invalid current-release link'
  check_release "$base/$previous_target"
elif [[ -e "$base/current" ]]; then die 'Current release must be a managed symbolic link'; fi

if [[ "$mode" == uninstall ]]; then
  if (( production )) && [[ -f "$unit" ]]; then
    systemctl disable --now powerlinux.service
    rm -f -- "$unit"
    systemctl daemon-reload
  fi
  if (( production )) && [[ -f "$agent_unit" ]]; then
    systemctl disable --now powerlinux-agent.service
    rm -f -- "$agent_unit"
    systemctl daemon-reload
  fi
  rm -f -- "$base/current"
  say 'Uninstalled service/current command. Data, configuration and release backups are retained:\n  %s\n  %s\n  %s\n' "$data" "$config" "$base"
  exit 0
fi

if (( production )); then
  for command in getent useradd id chown; do command -v "$command" >/dev/null || die 'Required command missing: %s' "$command"; done
  if getent passwd powerlinux >/dev/null; then
    [[ $(getent passwd powerlinux | cut -d: -f6) == "$data" ]] || die 'Existing powerlinux user has unexpected home; refusing to reuse'
    [[ $(getent passwd powerlinux | cut -d: -f7) =~ /(nologin|false)$ ]] || die 'Existing powerlinux user is interactive; refusing to reuse'
  else
    useradd --system --user-group --home-dir "$data" --no-create-home --shell /usr/sbin/nologin powerlinux
  fi
  [[ $(id -u powerlinux) != 0 && $(id -g powerlinux) != 0 && $(getent group powerlinux | cut -d: -f3) != 0 ]] || die 'API user/group must not have root identity'
  if [[ ! -d "$data" ]]; then mkdir -p "$data"; fi
  if [[ ! -d "$config" ]]; then mkdir -p "$config"; fi
  [[ ! -L "$data" && ! -L "$config" ]] || die 'Managed paths changed unexpectedly'
else
  mkdir -p "$data" "$config"
  if [[ ! -f "$data/server.lock" ]]; then
    [[ ! -L "$data/server.lock" ]] || die 'Server lock must not be a symbolic link'
    exec 8>"$data/server.lock"
    flock -n 8 || die 'Stop the prefix-mode server before modifying its installation'
  fi
fi
if (( !production )); then
  printf '%s\n' "$marker" > "$data/.powerlinux-managed"
  printf '%s\n' "$marker" > "$config/.powerlinux-managed"
fi
mkdir -p "$base/releases" "$base/backups"

if [[ "$mode" == rollback ]]; then
  [[ -f "$base/rollback-target" ]] || die 'No rollback snapshot exists'
  restore=$(cat "$base/rollback-target")
  [[ "$restore" =~ ^backups/[A-Za-z0-9._-]+$ && -d "$base/$restore/data" ]] || die 'Invalid rollback snapshot'
  target=$(cat "$base/$restore/previous-target")
  [[ "$target" =~ ^releases/[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$ && -d "$base/$target" ]] || die 'No prior release exists in snapshot'
  check_release "$base/$target"
  stop_service
  snapshot
  find "$data" -mindepth 1 -maxdepth 1 ! -name server.lock -exec rm -rf -- {} +
  cp -a "$base/$restore/data/." "$data/"
  if (( production )); then
    restore_deployment "$base/$restore" preserve-agent-policy || die 'Unable to restore prior deployment'
  fi
  ln -s "$target" "$base/.current-$$"
  mv -Tf -- "$base/.current-$$" "$base/current"
  if (( production )); then health_binary="$base/current/powerlinux"; fi
  start_service
  active_transaction=0
  rm -f -- "$base/pending-transaction"
  printf '%s\n' "backups/$(basename "$transaction")" > "$base/rollback-target.tmp"
  mv -f -- "$base/rollback-target.tmp" "$base/rollback-target"
  say 'Rolled back API to %s with its pre-upgrade data; intervening data retained in %s.\n' "$target" "$transaction"
  if (( production )); then say 'Latest Agent host ownership/state was preserved; an incompatible Agent remains stopped for recovery.\n'; fi
  exit 0
fi

release_dir="$base/releases/$version"
[[ ! -L "$release_dir" ]] || die 'Release directories must not be symbolic links'
if [[ -e "$release_dir" ]]; then
  [[ -f "$release_dir/archive.sha256" && $(cat "$release_dir/archive.sha256") == "$actual_hash" ]] || die 'A different artifact is already installed under this version'
  check_release "$release_dir"
else
  cp -a "$stage/package" "$base/releases/.stage-$version-$$"
  printf '%s\n' "$actual_hash" > "$base/releases/.stage-$version-$$/archive.sha256"
  (cd "$base/releases/.stage-$version-$$" && sha256sum powerlinux > powerlinux.sha256 && sha256sum folio > folio.sha256)
  chmod -R go-w "$base/releases/.stage-$version-$$"
  chmod 0755 "$base" "$base/releases" "$base/releases/.stage-$version-$$" "$base/releases/.stage-$version-$$/folio" "$base/releases/.stage-$version-$$/powerlinux"
  mv -T -- "$base/releases/.stage-$version-$$" "$release_dir"
fi
if [[ "$previous_target" == "releases/$version" && "$enable_agent" == 0 ]]; then
  if (( production )); then start_service; fi
  say 'Version %s is already installed; data left unchanged.\n' "$version"
  ui_log_location
  exit 0
fi
stop_service
snapshot
if (( production )); then
  configure_deployment
  agent_argument=
  if [[ -f "$agent_unit" ]]; then agent_argument=" --agent-socket $agent_socket"; fi
  api_unit_stage=$(mktemp /etc/systemd/system/.powerlinux-api.XXXXXX)
  cat > "$api_unit_stage" <<EOF
# Managed by Folio rewrite installer
[Unit]
Description=Folio lightweight Linux server visualization panel
After=network.target

[Service]
Type=simple
User=powerlinux
Group=powerlinux
ExecStart=$base/current/powerlinux serve --data-dir $data --server-config $config/server.json$agent_argument
WorkingDirectory=$data
UMask=0077
Restart=on-failure
RestartSec=3
TimeoutStopSec=20
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$data

[Install]
WantedBy=multi-user.target
EOF
  chmod 0644 "$api_unit_stage"
  mv -f -- "$api_unit_stage" "$unit"
  systemctl daemon-reload
  systemctl enable powerlinux.service
  [[ ! -f "$agent_unit" ]] || systemctl enable powerlinux-agent.service
fi
ln -s "releases/$version" "$base/.current-$$"
mv -Tf -- "$base/.current-$$" "$base/current"
start_service
active_transaction=0
rm -f -- "$base/pending-transaction"
if [[ -n "$previous_target" ]]; then
  printf '%s\n' "backups/$(basename "$transaction")" > "$base/rollback-target.tmp"
  mv -f -- "$base/rollback-target.tmp" "$base/rollback-target"
fi
say 'Installed Folio %s. Preserved data: %s\n' "$version" "$data"
if (( production )); then
  say 'Service uses preserved root-owned %s/server.json; new installs default to loopback.\n' "$config"
else
  say "No user or service was changed. Start: %s/current/powerlinux serve --server-config '' --data-dir %s --listen 127.0.0.1:9000\n" "$base" "$data"
fi

ui_phase "6" "Ready for your next step"
if (( production )); then
  say 'Folio is ready. The service health check passed.\n'
  say 'For a fresh localhost installation, keep this SSH tunnel open on your computer:\n'
  printf '  ssh -N -L 9000:127.0.0.1:9000 <user>@<server>\n'
  say 'Then open http://127.0.0.1:9000 in your browser. Existing access settings remain unchanged.\n'
  if [[ -f "$data/bootstrap-token" ]]; then
    say 'For first setup, read the private bootstrap-token file in a trusted server console. Do not put its contents in URLs or logs.\n'
    printf '  sudo cat %s/bootstrap-token\n' "$data"
  fi
  say 'Service diagnostics: sudo systemctl status powerlinux.service\n'
else
  say 'The package is ready. Start the command above, then open http://127.0.0.1:9000.\n'
fi
ui_log_location
