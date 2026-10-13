#!/usr/bin/env bash
# Reproduce the cloud development dependencies without running legacy installers.
set -Eeuo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
tools_dir=${POWERLINUX_TOOLS_DIR:-/workspace/.tools}
go_version=1.27.2
go_sha256=ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5
go_bin=${GO_BIN:-$tools_dir/go-$go_version/go/bin/go}
if [[ ! -x "$go_bin" ]]; then
  [[ "$(uname -m)" == x86_64 ]] || { echo 'This cloud bootstrap is pinned to the amd64 development host.' >&2; exit 1; }
  mkdir -p "$tools_dir"
  temporary=$(mktemp -d "$tools_dir/go-install.XXXXXX")
  trap 'rm -rf -- "$temporary"' EXIT
  curl --fail --location --retry 2 --proto '=https' --tlsv1.2 \
    "https://go.dev/dl/go$go_version.linux-amd64.tar.gz" -o "$temporary/go.tar.gz"
  printf '%s  %s\n' "$go_sha256" "$temporary/go.tar.gz" | sha256sum --check --status
  tar -xzf "$temporary/go.tar.gz" -C "$temporary"
  mv -- "$temporary" "$tools_dir/go-$go_version"
  # The verified archive is unnecessary after extraction.
  rm -- "$tools_dir/go-$go_version/go.tar.gz"
  trap - EXIT
fi
[[ "$("$go_bin" version)" == "go version go$go_version linux/amd64" ]] || { echo "Use the pinned Go $go_version toolchain." >&2; exit 1; }
[[ "$(node --version)" == v24.* ]] || { echo 'Node24 is required by the cloud development workflow.' >&2; exit 1; }
export GO_BIN="$go_bin" GOPATH="${GOPATH:-/workspace/.cache/go}"
export GOCACHE="${GOCACHE:-/workspace/.cache/go-build}"
export GOMODCACHE="${GOMODCACHE:-$GOPATH/pkg/mod}"
export NEXT_TELEMETRY_DISABLED=1
mkdir -p "$GOCACHE" "$GOMODCACHE" /workspace/.cache/npm
cd "$root"
"$GO_BIN" mod download
"$GO_BIN" mod verify
npm ci --prefix frontend --cache /workspace/.cache/npm --no-audit --no-fund
npm run typecheck --prefix frontend
printf 'Cloud dependencies verified. GO_BIN=%s\n' "$GO_BIN"
