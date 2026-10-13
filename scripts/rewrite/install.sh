#!/usr/bin/env bash
# PowerLinux rewrite installer. No compilation or third-party service installs.
set -Eeuo pipefail
umask 077

usage() {
  cat <<'EOF'
Usage: install.sh --archive FILE --sha256 TRUSTED_SHA256 [--prefix ABSOLUTE_DIR]
       install.sh --version VERSION --sha256 TRUSTED_SHA256 [--prefix ABSOLUTE_DIR]
       install.sh --rollback [--prefix ABSOLUTE_DIR]
       install.sh --uninstall [--prefix ABSOLUTE_DIR]

Optional production deployment: --enable-agent --agent-policy ROOT_PRIVATE_JSON
--enable-terminal, --enable-software and --enable-sites create isolated module
users/directories; they require --enable-agent and an explicit trusted policy.
--enable-source-build also requires --enable-software and prepares a separate
compiler identity/cache; source builds remain explicitly controlled by policy.

Production requires root and running systemd; only PowerLinux-owned paths,
user and unit are changed. --prefix is nonprivileged, creates no user/service,
and requires the caller to stop a running instance before update or rollback.
Data is preserved during uninstall. Delete preserved data manually after backup.
Remote packages are fetched from github.com/Aoody743/PowerLinux over HTTPS.
--sha256 must be obtained through a trusted release channel; a same-channel
checksum alone does not authenticate a malicious publisher. --attestation
additionally verifies the GitHub build attestation with an installed gh CLI.
EOF
}
die() { printf 'PowerLinux: %s\n' "$*" >&2; exit 1; }
archive= version= expected_hash= prefix= mode=install attestation=0
enable_agent=0 enable_terminal=0 enable_software=0 enable_sites=0 enable_source_build=0 agent_policy=
while (($#)); do
  case "$1" in
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
    *) die "Unknown option: $1" ;;
  esac
done
if (( enable_terminal || enable_software || enable_sites || enable_source_build )); then (( enable_agent )) || die 'Module deployment requires --enable-agent'; fi
if (( enable_source_build )); then (( enable_software )) || die '--enable-source-build requires --enable-software'; fi
if (( enable_agent )); then [[ -n "$agent_policy" ]] || die '--enable-agent requires an explicit --agent-policy'; fi
[[ -z "$agent_policy" || "$enable_agent" == 1 ]] || die '--agent-policy requires --enable-agent'
[[ "$mode" == install || "$enable_agent" == 0 ]] || die 'Deployment flags apply only to install/upgrade'
for command in sha256sum tar flock realpath cp mv find mktemp; do command -v "$command" >/dev/null || die "Required command missing: $command"; done
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
    [[ "$mount_path" != "$data/"* ]] || die "Unmount nested data mount before installation/restore: $mount_path"
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
    printf 'Installation failed; restoring the prior release and data snapshot.\n' >&2
    if (( production )); then
      systemctl stop powerlinux.service >/dev/null 2>&1 || true
      [[ ! -f "$agent_unit" ]] || systemctl stop powerlinux-agent.service >/dev/null 2>&1 || true
      acquire_production_data_lock
    fi
    if [[ -d "$transaction/data" ]]; then
      assert_no_nested_mounts
      mkdir -p "$transaction/failed-data"
      if ! cp -a "$data/." "$transaction/failed-data/"; then
        printf 'Failed to preserve current data (check disk capacity). Recovery journal retained; inspect %s before retrying.\n' "$transaction" >&2
        [[ -n "$stage" ]] && rm -rf -- "$stage"
        exit 1
      fi
      find "$data" -mindepth 1 -maxdepth 1 ! -name server.lock -exec rm -rf -- {} +
      if ! cp -a "$transaction/data/." "$data/"; then
        printf 'Snapshot restore failed. Snapshot and journal retained in %s; service remains stopped.\n' "$transaction" >&2
        [[ -n "$stage" ]] && rm -rf -- "$stage"
        exit 1
      fi
    fi
    if (( production )) && ! restore_deployment "$transaction"; then
      printf 'Deployment restore failed; recovery journal retained in %s. Services remain stopped.\n' "$transaction" >&2
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
          printf 'Prior API recovery failed its real health check. Recovery journal retained in %s; services remain stopped.\n' "$transaction" >&2
          exit 1
        fi
        start_agent_or_preserve_recovery "$base/current/powerlinux" || result=1
      fi
    else
      rm -f -- "$base/current"
      if (( production )); then
        systemctl disable powerlinux.service >/dev/null 2>&1 || true
        [[ ! -f "$agent_unit" ]] || mark_agent_recovery 'First installation failed; no API release is active'
      fi
    fi
    rm -f -- "$base/pending-transaction"
    [[ -n "$transaction" ]] && printf 'Preserved failure evidence: %s\n' "$transaction" >&2
  elif (( service_stopped && production )) && [[ -n "$previous_target" ]]; then
    # A snapshot/pre-switch prerequisite can fail after the old service stops.
    # No data replacement occurred, so restart that unchanged release.
    release_production_data_lock
    [[ ! -f "$agent_unit" ]] || systemctl start powerlinux-agent.service || result=1
    systemctl start powerlinux.service || result=1
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
    [[ "$mount_path" != "$tree" && "$mount_path" != "$tree/"* ]] || die "Unmount owned deployment tree before snapshot/restore: $mount_path"
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
  printf 'Privileged Agent remains stopped and needs recovery: %s.\nLatest authoritative host state is retained at %s; deployment snapshots remain at %s.\n' "$reason" "$agent_state" "$transaction" >&2
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
    [[ $(getent passwd "$account" | cut -d: -f6) == "$home_dir" && $(getent passwd "$account" | cut -d: -f7) =~ /(nologin|false)$ ]] || die "Existing $account account has unmanaged home/shell"
  else
    useradd --system --user-group --home-dir "$home_dir" --no-create-home --shell /usr/sbin/nologin "$account"
  fi
  [[ $(id -u "$account") != 0 && $(id -g "$account") != 0 && $(id -u "$account") != "$(id -u powerlinux)" && $(id -g "$account") != "$(id -g powerlinux)" ]] || die 'Module account must have a non-root UID/GID distinct from the API'
  [[ ! -L "$home_dir" ]] || die "Module home is a symbolic link: $home_dir"
  if [[ -e "$home_dir" ]]; then
    [[ -d "$home_dir" && ( $(stat -c '%u' "$home_dir") == 0 || $(stat -c '%u' "$home_dir") == "$(id -u "$account")" ) ]] || die "Module home has an unmanaged owner/type: $home_dir"
  fi
  if [[ -d "$home_dir" && -n $(find "$home_dir" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
    [[ -f "$home_dir/.powerlinux-managed" && ! -L "$home_dir/.powerlinux-managed" && $(cat "$home_dir/.powerlinux-managed") == "$marker" ]] || die "Refusing unmanaged module home: $home_dir"
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
  [[ ! -L "$path" ]] || die "Module directory is a symbolic link: $path"
  if [[ -d "$path" && -n $(find "$path" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
    [[ -f "$path/.powerlinux-managed" && ! -L "$path/.powerlinux-managed" && $(cat "$path/.powerlinux-managed") == "$marker" && $(stat -c '%u' "$path") == 0 ]] || die "Refusing unmanaged module directory: $path"
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
# Managed by PowerLinux rewrite installer
[Unit]
Description=PowerLinux typed privileged Agent
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
}
[[ ! -L "$base" && ! -L "$data" && ! -L "$config" ]] || die 'Managed paths must not be symbolic links'
for path in "$base/releases" "$base/backups" "$base/.install-lock" "$base/pending-transaction" "$base/rollback-target" "$base/agent-recovery-required" "$data/.powerlinux-managed" "$config/.powerlinux-managed"; do
  [[ ! -L "$path" ]] || die "Managed metadata must not be a symbolic link: $path"
done
if [[ -e "$base" ]] && ! managed; then
  [[ "$mode" == install && -d "$base" && -z $(find "$base" -mindepth 1 -maxdepth 1 -print -quit) ]] || die "Refusing to modify unmanaged directory $base"
fi
if (( production )); then
  for path in "$base" "$config"; do
    if [[ -e "$path" ]]; then
      [[ -d "$path" && ! -L "$path" && $(stat -c '%u' "$path") == 0 ]] || die "Installation/configuration paths must be root-owned real directories: $path"
      directory_permissions=$(stat -c '%a' "$path")
      (( (8#$directory_permissions & 0022) == 0 )) || die "Installation/configuration directory permits untrusted replacement: $path"
    fi
  done
  for path in "$data" "$config"; do
    if [[ -d "$path" && -n $(find "$path" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
      [[ -f "$path/.powerlinux-managed" && ! -L "$path/.powerlinux-managed" && $(cat "$path/.powerlinux-managed") == "$marker" ]] || die "Refusing to use unmanaged directory $path"
    fi
  done
fi
if [[ "$mode" != install ]]; then managed || die 'No managed installation found'; fi
if (( production )); then
  for owned_unit in "$unit" "$agent_unit"; do
    [[ ! -L "$owned_unit" ]] || die "Unit must not be a symbolic link: $owned_unit"
    if [[ -e "$owned_unit" ]]; then
      [[ -f "$owned_unit" && $(stat -c '%u' "$owned_unit") == 0 ]] || die "Existing unit must be a root-owned regular file: $owned_unit"
      unit_permissions=$(stat -c '%a' "$owned_unit")
      (( (8#$unit_permissions & 0022) == 0 )) || die "Existing unit permits untrusted modification: $owned_unit"
      head -1 "$owned_unit" | grep -Fxq '# Managed by PowerLinux rewrite installer' || die "Existing unit is unmanaged: $owned_unit"
    fi
    # A vendor unit under /usr/lib or /lib is also an existing deployment;
    # do not silently override it merely because our /etc filename is absent.
    if [[ "$owned_unit" == "$unit" || -f "$owned_unit" || "$enable_agent" == 1 ]]; then
      loaded_fragment=$(systemctl show "$(basename "$owned_unit")" --property=FragmentPath --value 2>/dev/null || true)
      [[ -z "$loaded_fragment" || "$loaded_fragment" == "$owned_unit" ]] || die "Existing unit resolves to an unmanaged location: $loaded_fragment"
    fi
  done
  if [[ -e "$agent_state" ]]; then
    [[ ! -L "$agent_state" && -d "$agent_state" && $(stat -c '%u:%a' "$agent_state") == 0:700 && -f "$agent_state/.powerlinux-managed" && ! -L "$agent_state/.powerlinux-managed" && $(cat "$agent_state/.powerlinux-managed") == "$marker" ]] || die 'Refusing unmanaged Agent state directory'
  fi
fi

if (( production && enable_agent )); then require_agent_policy_source; fi
if (( production )) && { (( enable_agent )) || [[ -f "$agent_unit" ]]; }; then command -v sync >/dev/null || die 'Agent transactions require the coreutils sync command'; fi

if [[ "$mode" == install ]]; then
  [[ "$expected_hash" =~ ^[a-fA-F0-9]{64}$ ]] || die 'A trusted SHA256 pin is required (--sha256)'
  [[ -z "$version" || "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$ ]] || die 'Invalid version'
  stage=$(mktemp -d "${TMPDIR:-/tmp}/powerlinux-install.XXXXXX")
  if [[ -z "$archive" ]]; then
    [[ -n "$version" ]] || die 'Specify --archive or --version'
    command -v curl >/dev/null || die 'curl is required for HTTPS downloads'
    archive="$stage/release.tar.gz"
    curl --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 2 \
      --output "$archive" "https://github.com/Aoody743/PowerLinux/releases/download/$version/powerlinux-$version-linux-$architecture.tar.gz"
  fi
  [[ -f "$archive" ]] || die 'Archive does not exist'
  actual_hash=$(sha256sum -- "$archive"); actual_hash=${actual_hash%% *}
  [[ "${actual_hash,,}" == "${expected_hash,,}" ]] || die 'Archive SHA256 does not match the trusted pin'
  if (( attestation )); then
    command -v gh >/dev/null || die '--attestation requires GitHub CLI'
    gh attestation verify "$archive" --repo Aoody743/PowerLinux --signer-workflow Aoody743/PowerLinux/.github/workflows/rewrite-ci.yml || die 'Release build attestation is invalid'
  fi
  # The release format is deliberately flat and permits regular files only.
  # Reject links, devices, absolute/parent paths, duplicate and unknown members.
  declare -A seen=()
  while IFS= read -r member; do
    case "$member" in powerlinux|VERSION|PLATFORM|install.sh|BUILDINFO|README.txt) ;;
      *) die "Unsafe or unexpected archive member: $member" ;; esac
    [[ -z "${seen[$member]:-}" ]] || die "Duplicate archive member: $member"
    seen[$member]=1
  done < <(tar -tzf "$archive")
  [[ ${#seen[@]} == 6 ]] || die 'Incomplete release package'
  while IFS= read -r member; do [[ "$member" == -* ]] || die 'Release archive contains a non-regular file'; done < <(tar -tvzf "$archive")
  mkdir "$stage/package"
  tar --no-same-owner --no-same-permissions -xzf "$archive" -C "$stage/package"
  package_version=$(cat "$stage/package/VERSION")
  [[ "$package_version" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$ ]] || die 'Invalid packaged version'
  [[ -z "$version" || "$version" == "$package_version" ]] || die 'Requested and packaged version differ'
  version=$package_version
  [[ $(cat "$stage/package/PLATFORM") == "linux-$architecture" ]] || die 'Release platform does not match this machine'
  chmod 0755 "$stage/package/powerlinux"
  [[ $("$stage/package/powerlinux" version) == "$version" ]] || die 'Binary and package version differ'
  health_binary="$stage/package/powerlinux"
fi

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
  printf 'Uninstalled service/current command. Data, configuration and release backups are retained:\n  %s\n  %s\n  %s\n' "$data" "$config" "$base"
  exit 0
fi

if (( production )); then
  for command in getent useradd id chown; do command -v "$command" >/dev/null || die "Required command missing: $command"; done
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
  printf 'Rolled back API to %s with its pre-upgrade data; intervening data retained in %s.\n' "$target" "$transaction"
  if (( production )); then printf 'Latest Agent host ownership/state was preserved; an incompatible Agent remains stopped for recovery.\n'; fi
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
  (cd "$base/releases/.stage-$version-$$" && sha256sum powerlinux > powerlinux.sha256)
  chmod -R go-w "$base/releases/.stage-$version-$$"
  chmod 0755 "$base" "$base/releases" "$base/releases/.stage-$version-$$" "$base/releases/.stage-$version-$$/powerlinux"
  mv -T -- "$base/releases/.stage-$version-$$" "$release_dir"
fi
if [[ "$previous_target" == "releases/$version" && "$enable_agent" == 0 ]]; then
  if (( production )); then start_service; fi
  printf 'Version %s is already installed; data left unchanged.\n' "$version"
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
# Managed by PowerLinux rewrite installer
[Unit]
Description=PowerLinux lightweight panel
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
printf 'Installed PowerLinux %s. Preserved data: %s\n' "$version" "$data"
if (( production )); then
  printf 'Service uses preserved root-owned %s/server.json; new installs default to loopback.\n' "$config"
else
  printf "No user or service was changed. Start: %s/current/powerlinux serve --server-config '' --data-dir %s --listen 127.0.0.1:9000\n" "$base" "$data"
fi
