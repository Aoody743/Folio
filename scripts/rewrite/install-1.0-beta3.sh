#!/usr/bin/env bash
# One-command bootstrap for the Folio 1.0-beta3 Linux amd64 release.
set -Eeuo pipefail
umask 077

version=1.0-beta3
archive_name="folio-$version-linux-amd64.tar.gz"
release_url="https://github.com/Aoody743/Folio/releases/download/$version"

usage() {
  cat <<EOF
Install Folio $version on Linux amd64 from its GitHub release.

Usage:
  curl -fsSL $release_url/install-1.0-beta3.sh | sudo bash
  curl -fsSL $release_url/install-1.0-beta3.sh | sudo bash -s -- --no-managed-features

Fresh system installs enable the reviewed software and website/certificate
modules by default. Existing Agent policies are preserved during upgrades.
Use --no-managed-features to skip default module setup, or --prefix for a
non-privileged local install.
EOF
}

die() { printf 'Folio 1.0-beta3 installer: %s\n' "$*" >&2; exit 1; }

installer_args=()
skip_default_modules=0
custom_agent_configuration=0
while (($#)); do
  case "$1" in
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
    *)
      installer_args+=("$1")
      shift
      ;;
  esac
done

case "$(uname -s):$(uname -m)" in
  Linux:x86_64) ;;
  *) die 'This one-command release installer supports Linux amd64.' ;;
esac

for command in curl sha256sum tar mktemp awk; do
  command -v "$command" >/dev/null 2>&1 || die "Required command missing: $command"
done

if (( EUID == 0 )); then
  temporary_dir=$(mktemp -d /run/folio-beta3-install.XXXXXX)
else
  temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/folio-beta3-install.XXXXXX")
fi
default_policy=
cleanup() {
  [[ -z "$default_policy" ]] || rm -f -- "$default_policy"
  rm -rf -- "$temporary_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

archive="$temporary_dir/$archive_name"
checksums="$temporary_dir/SHA256SUMS.txt"
curl --fail --location --silent --show-error \
  --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 2 \
  --output "$checksums" "$release_url/SHA256SUMS.txt"
checksum_line=$(awk -v name="$archive_name" '$2 == name { if (found++) exit 2; print } END { if (!found) exit 1 }' "$checksums") \
  || die 'Release SHA256SUMS.txt does not contain exactly one package checksum.'
[[ "$checksum_line" =~ ^([[:xdigit:]]{64})[[:space:]]+\*?${archive_name//./\.}$ ]] \
  || die 'Release SHA256SUMS.txt contains an invalid package checksum line.'
archive_sha256=${BASH_REMATCH[1]}

curl --fail --location --silent --show-error \
  --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 2 \
  --output "$archive" "$release_url/$archive_name"
printf '%s  %s\n' "$archive_sha256" "$archive" | sha256sum --check --status \
  || die 'Downloaded release archive failed its SHA-256 check.'

tar --no-same-owner --no-same-permissions -xzf "$archive" \
  -C "$temporary_dir" install.sh
[[ -f "$temporary_dir/install.sh" && ! -L "$temporary_dir/install.sh" ]] \
  || die 'The verified archive did not contain a regular install.sh.'

# Only seed the reviewed beta3 policy on a fresh production install. Existing
# policies and custom module selections remain authoritative during upgrades.
if (( !skip_default_modules && !custom_agent_configuration && EUID == 0 )) &&
   [[ ! -e /etc/powerlinux/agent-policy.json && ! -L /etc/powerlinux/agent-policy.json &&
      ! -e /etc/systemd/system/powerlinux-agent.service && ! -L /etc/systemd/system/powerlinux-agent.service ]]; then
  default_policy=$(mktemp /run/folio-beta3-agent-policy.XXXXXX)
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

bash "$temporary_dir/install.sh" --archive "$archive" \
  --sha256 "$archive_sha256" "${installer_args[@]}"
