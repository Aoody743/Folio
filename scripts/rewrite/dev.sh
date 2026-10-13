#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
cd "$root"
cache=${POWERLINUX_BUILD_CACHE:-$root/.cache/rewrite}
export GOCACHE="${GOCACHE:-$cache/go-build}" GOMODCACHE="${GOMODCACHE:-$cache/go-mod}"
mkdir -p "$GOCACHE" "$GOMODCACHE" "$cache/dev-data"
# Use the same exported/embedded UI as the release; no permanent Node service.
case "$(uname -m)" in x86_64) architecture=amd64 ;; aarch64|arm64) architecture=arm64 ;; *) echo 'Unsupported development architecture' >&2; exit 1 ;; esac
bash scripts/rewrite/build.sh --version dev --architectures "$architecture" --output "$cache/dev-build"
# Execute the binary already compiled by build.sh; never compile it twice.
(cd "$cache/dev-build" && sha256sum --check --status CHECKSUMS.txt)
runtime_stage=$(mktemp -d "$cache/dev-extract.XXXXXX")
trap 'rm -rf -- "$runtime_stage"' EXIT
tar --no-same-owner --no-same-permissions -xzf "$cache/dev-build/powerlinux-dev-linux-$architecture.tar.gz" -C "$runtime_stage" powerlinux
chmod 0755 "$runtime_stage/powerlinux"
[[ $("$runtime_stage/powerlinux" version) == dev ]] || { echo 'Development binary version mismatch' >&2; exit 1; }
mkdir -p "$cache/dev-runtime"
mv -f -- "$runtime_stage/powerlinux" "$cache/dev-runtime/powerlinux"
rm -rf -- "$runtime_stage"
trap - EXIT
exec "$cache/dev-runtime/powerlinux" serve --server-config '' --data-dir "${POWERLINUX_DATA_DIR:-$cache/dev-data}" --listen "${POWERLINUX_LISTEN:-127.0.0.1:9000}"
