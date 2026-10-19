#!/usr/bin/env bash
# One-command bootstrap for the Folio 1.0-beta1 Linux amd64 release.
set -Eeuo pipefail
umask 077

version=1.0-beta1
archive_name="folio-$version-linux-amd64.tar.gz"
release_url="https://github.com/Aoody743/Folio/releases/download/$version"

usage() {
  cat <<EOF
Install Folio $version on Linux amd64 from its GitHub release.

Usage:
  curl -fsSL $release_url/install-1.0-beta1.sh | sudo bash

Optional arguments are passed to the verified package installer:
  --prefix ABSOLUTE_DIR  Install without creating system users or services.
  --enable-agent --agent-policy ROOT_PRIVATE_JSON
  --enable-terminal --enable-software --enable-sites --enable-source-build
EOF
}

die() { printf 'Folio 1.0-beta1 installer: %s\n' "$*" >&2; exit 1; }

case "$(uname -s):$(uname -m)" in
  Linux:x86_64) ;;
  *) die 'This one-command release installer supports Linux amd64.' ;;
esac

for command in curl sha256sum tar mktemp awk; do
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

temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/folio-beta1-install.XXXXXX")
trap 'rm -rf -- "$temporary_dir"' EXIT
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

bash "$temporary_dir/install.sh" --archive "$archive" \
  --sha256 "$archive_sha256" "${installer_args[@]}"
