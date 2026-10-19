#!/usr/bin/env bash
# Pinned one-command bootstrap for the Folio 0.1.0-rc.15 Linux amd64 release.
set -Eeuo pipefail
umask 077

version=0.1.0-rc.15
archive_name="folio-$version-linux-amd64.tar.gz"
archive_sha256=392458cadb7850be2653dd341a10e2f1e1935c8709640eb44ecb6e8e37d4a9a6
archive_url="https://github.com/Aoody743/Folio/releases/download/$version/$archive_name"

usage() {
  cat <<EOF
Install Folio $version on Linux amd64 from its pinned GitHub release archive.

Usage:
  curl -fsSL https://github.com/Aoody743/Folio/releases/download/$version/install-rc15.sh | sudo bash

Optional arguments are passed to the verified package installer:
  --prefix ABSOLUTE_DIR  Install without creating system users or services.
  --enable-agent --agent-policy ROOT_PRIVATE_JSON
  --enable-terminal --enable-software --enable-sites --enable-source-build

The archive version and SHA-256 are pinned in this script and cannot be overridden.
EOF
}

die() { printf 'Folio RC15 installer: %s\n' "$*" >&2; exit 1; }

case "$(uname -s):$(uname -m)" in
  Linux:x86_64) ;;
  *) die 'This one-command release installer supports Linux amd64 only.' ;;
esac

for command in curl sha256sum tar mktemp; do
  command -v "$command" >/dev/null 2>&1 || die "Required command missing: $command"
done

installer_args=()
while (($#)); do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --prefix|--agent-policy)
      (($# >= 2)) || die "Missing value for $1"
      installer_args+=("$1" "$2")
      shift 2
      ;;
    --enable-agent|--enable-terminal|--enable-software|--enable-sites|--enable-source-build)
      installer_args+=("$1")
      shift
      ;;
    *) die "Unsupported option: $1" ;;
  esac
done

temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/folio-rc15-install.XXXXXX")
trap 'rm -rf -- "$temporary_dir"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
archive="$temporary_dir/$archive_name"

curl --fail --location --silent --show-error \
  --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 2 \
  --output "$archive" "$archive_url"
printf '%s  %s\n' "$archive_sha256" "$archive" | sha256sum --check --status \
  || die 'Downloaded release archive failed the pinned SHA-256 check.'

tar --no-same-owner --no-same-permissions -xzf "$archive" \
  -C "$temporary_dir" install.sh
[[ -f "$temporary_dir/install.sh" && ! -L "$temporary_dir/install.sh" ]] \
  || die 'The verified archive did not contain a regular install.sh.'

bash "$temporary_dir/install.sh" --archive "$archive" \
  --sha256 "$archive_sha256" "${installer_args[@]}"
