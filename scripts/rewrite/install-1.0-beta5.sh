#!/usr/bin/env bash
# One-command bootstrap for the Folio 1.0-beta5 Linux amd64 release.
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

version=1.0-beta5
archive_name="folio-$version-linux-amd64.tar.gz"
release_url="https://github.com/Aoody743/Folio/releases/download/$version"

usage() {
  if [[ "$ui_language" == zh-CN ]]; then
    cat <<ZH
Folio $version 安装向导（Linux amd64）
用法：下载此脚本后执行 sudo bash install-$version.sh --lang zh-CN
语言：--lang zh-CN|en；自动化：--yes / --non-interactive；纯文本：--no-color
首次安装默认启用网站和软件模块；升级保留已有 Agent 策略。
--no-managed-features 跳过默认模块；--prefix 用于不创建系统用户/服务的本地安装。
ZH
    return
  fi
  cat <<EOF
Install Folio $version on Linux amd64 from its GitHub release.

Usage:
  curl -fsSL $release_url/install-1.0-beta5.sh | sudo bash
  curl -fsSL $release_url/install-1.0-beta5.sh | sudo bash -s -- --no-managed-features

Language: --lang zh-CN|en; automation: --yes / --non-interactive; --no-color.

Fresh system installs enable the reviewed software and website/certificate
modules by default. Existing Agent policies are preserved during upgrades.
Use --no-managed-features to skip default module setup, or --prefix for a
non-privileged local install.
EOF
}

die() { printf 'Folio: ' >&2; say "$@" >&2; printf '\n' >&2; ui_log_location >&2; exit 1; }

installer_args=()
skip_default_modules=0
custom_agent_configuration=0
while (($#)); do
  case "$1" in
    --lang)
      installer_args+=("$1" "$2"); shift 2 ;;
    --yes|--non-interactive|--no-color)
      installer_args+=("$1"); shift ;;
    --help|-h)
      usage
      exit 0
      ;;
    --no-managed-features)
      skip_default_modules=1
      shift
      ;;
    --prefix)
      (($# >= 2)) || die 'Missing value for --prefix'
      skip_default_modules=1
      installer_args+=("$1" "$2")
      shift 2
      ;;
    --enable-agent|--enable-terminal|--enable-software|--enable-sites|--enable-source-build)
      custom_agent_configuration=1
      installer_args+=("$1")
      shift
      ;;
    --agent-policy)
      (($# >= 2)) || die 'Missing value for --agent-policy'
      custom_agent_configuration=1
      installer_args+=("$1" "$2")
      shift 2
      ;;
    --attestation|--rollback|--uninstall)
      installer_args+=("$1"); shift ;;
    *) die 'Unknown option: %s' "$1" ;;

  esac
done

ui_begin
ui_phase "1" "Checking this server"
case "$(uname -s):$(uname -m)" in
  Linux:x86_64) ;;
  *) die 'This one-command release installer supports Linux amd64.' ;;
esac

for command in curl sha256sum tar mktemp awk; do
  command -v "$command" >/dev/null 2>&1 || die 'Required command missing: %s' "$command"
done

if (( EUID == 0 )); then
  temporary_dir=$(mktemp -d /run/folio-beta5-install.XXXXXX)
else
  temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/folio-beta5-install.XXXXXX")
fi
default_policy=
cleanup() {
  local result=$?
  if (( result != 0 )); then ui_log_location >&2; fi
  [[ -z "$default_policy" ]] || rm -f -- "$default_policy"
  rm -rf -- "$temporary_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

archive="$temporary_dir/$archive_name"
checksums="$temporary_dir/SHA256SUMS.txt"
ui_phase "2" "Downloading and verifying the package"
curl --fail --location "${ui_curl_progress[@]}" \
  --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 2 \
  --output "$checksums" "$release_url/SHA256SUMS.txt"
checksum_line=$(awk -v name="$archive_name" '$2 == name { if (found++) exit 2; print } END { if (!found) exit 1 }' "$checksums") \
  || die 'Release SHA256SUMS.txt does not contain exactly one package checksum.'
[[ "$checksum_line" =~ ^([[:xdigit:]]{64})[[:space:]]+\*?${archive_name//./\.}$ ]] \
  || die 'Release SHA256SUMS.txt contains an invalid package checksum line.'
archive_sha256=${BASH_REMATCH[1]}

curl --fail --location "${ui_curl_progress[@]}" \
  --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 2 \
  --output "$archive" "$release_url/$archive_name"
printf '%s  %s\n' "$archive_sha256" "$archive" | sha256sum --check --status \
  || die 'Downloaded release archive failed its SHA-256 check.'

tar --no-same-owner --no-same-permissions -xzf "$archive" \
  -C "$temporary_dir" install.sh
[[ -f "$temporary_dir/install.sh" && ! -L "$temporary_dir/install.sh" ]] \
  || die 'The verified archive did not contain a regular install.sh.'

# Only seed the reviewed default policy on a fresh production install. Existing
# policies and custom module selections remain authoritative during upgrades.
if (( !skip_default_modules && !custom_agent_configuration && EUID == 0 )) &&
   [[ ! -e /etc/powerlinux/agent-policy.json && ! -L /etc/powerlinux/agent-policy.json &&
      ! -e /etc/systemd/system/powerlinux-agent.service && ! -L /etc/systemd/system/powerlinux-agent.service ]]; then
  default_policy=$(mktemp /run/folio-beta5-agent-policy.XXXXXX)
  chmod 0600 "$default_policy"
  cat > "$default_policy" <<'POLICY'
{
  "version": 2,
  "allowed_users": ["powerlinux"],
  "socket_group": "powerlinux",
  "plugins": {
    "root_dir": "/opt/powerlinux-software",
    "state_dir": "/var/lib/powerlinux-agent/software",
    "applications_root_dir": "/srv/powerlinux-apps",
    "run_user": "powerlinux-services",
    "run_group": "powerlinux-services",
    "api_user": "powerlinux",
    "enable_system_packages": true,
    "use_systemd": true
  },
  "sites": {
    "version": 1,
    "root_dir": "/srv/powerlinux-sites",
    "state_dir": "/var/lib/powerlinux-agent/sites",
    "programs": {
      "useradd": "/usr/sbin/useradd",
      "userdel": "/usr/sbin/userdel",
      "usermod": "/usr/sbin/usermod"
    },
    "allowed_ports": [80, 443],
    "allow_ipv6": true
  }
}
POLICY
  installer_args+=(
    --enable-agent
    --enable-software
    --enable-sites
    --agent-policy "$default_policy"
  )
fi

export FOLIO_BOOTSTRAP=1 FOLIO_LANG="$ui_language"
bash "$temporary_dir/install.sh" --archive "$archive" \
  --sha256 "$archive_sha256" "${installer_args[@]}"
