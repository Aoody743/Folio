#!/usr/bin/env bash
set -Eeuo pipefail
umask 022

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
version=dev
output="$root/dist/rewrite"
architectures=amd64,arm64
skip_frontend=0
skip_install=0
while (($#)); do
  case "$1" in
    --version) version=${2:?missing version}; shift 2 ;;
    --output) output=${2:?missing output}; shift 2 ;;
    --architectures) architectures=${2:?missing architectures}; shift 2 ;;
    --skip-frontend) skip_frontend=1; shift ;;
    --skip-install) skip_install=1; shift ;;
    *) printf 'Unknown build option: %s\n' "$1" >&2; exit 2 ;;
  esac
done
[[ "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$ ]] || { echo 'Invalid release version' >&2; exit 2; }
cd "$root"
go_bin=${GO_BIN:-go}
required_go=$(awk '$1 == "go" { print $2 }' go.mod)
actual_go=$("$go_bin" version | awk '{ sub(/^go/, "", $3); print $3 }')
[[ "$actual_go" == "$required_go" ]] || { printf 'Use Go %s (found %s)\n' "$required_go" "$actual_go" >&2; exit 1; }
commit=$(git rev-parse HEAD)
cache=${POWERLINUX_BUILD_CACHE:-$root/.cache/rewrite}
parallelism=${POWERLINUX_BUILD_PARALLELISM:-2}
[[ "$parallelism" =~ ^([1-9]|[12][0-9]|3[0-2])$ ]] || { echo 'POWERLINUX_BUILD_PARALLELISM must be 1..32' >&2; exit 2; }
export GOCACHE="${GOCACHE:-$cache/go-build}" GOMODCACHE="${GOMODCACHE:-$cache/go-mod}"
mkdir -p "$GOCACHE" "$GOMODCACHE" "$cache/npm" "$output"
mkdir -p "$root/.cache/rewrite"
exec 7>"$root/.cache/rewrite/build.lock"
lock_dir=
if command -v flock >/dev/null 2>&1; then
  flock -n 7 || { echo 'Another release build is refreshing/using the embedded assets' >&2; exit 1; }
else
  # macOS does not ship flock. Keep a portable mkdir lock for local builds.
  lock_dir="$root/.cache/rewrite/build.lock.d"
  if ! mkdir "$lock_dir" 2>/dev/null; then
    echo 'Another release build is refreshing/using the embedded assets' >&2
    exit 1
  fi
fi
if (( !skip_frontend )); then
  if (( !skip_install )); then npm ci --prefix frontend --cache "$cache/npm" --no-audit --no-fund; fi
  npm run typecheck --prefix frontend
  NEXT_BUILD_ID="$commit" NEXT_TELEMETRY_DISABLED=1 npm run build --prefix frontend
fi
[[ -s frontend/out/index.html ]] || { echo 'Build frontend/out first; placeholder UI cannot be packaged' >&2; exit 1; }
mkdir -p internal/webassets/dist
# Only this owned generated asset directory is refreshed, never the checkout.
find internal/webassets/dist -mindepth 1 -maxdepth 1 ! -name README.txt -exec rm -rf -- {} +
cp -a frontend/out/. internal/webassets/dist/
dirty=true
if [[ -z $(git status --porcelain --untracked-files=normal) ]]; then dirty=false; fi
epoch=${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct HEAD)}
[[ "$epoch" =~ ^[0-9]+$ ]] || { echo 'Invalid SOURCE_DATE_EPOCH' >&2; exit 2; }
stage=$(mktemp -d "$cache/release.XXXXXX")
trap 'rm -rf -- "$stage"; if [[ -n "$lock_dir" ]]; then rmdir "$lock_dir" 2>/dev/null || true; fi' EXIT
IFS=, read -r -a arch_list <<< "$architectures"
for arch in "${arch_list[@]}"; do
  [[ "$arch" == amd64 || "$arch" == arm64 ]] || { printf 'Unsupported architecture: %s\n' "$arch" >&2; exit 2; }
  package="$stage/$arch"
  mkdir -p "$package"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$go_bin" build -p "$parallelism" -mod=readonly -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.version=$version" -o "$package/powerlinux" ./cmd/powerlinux
  cp "$package/powerlinux" "$package/folio"
  printf '%s\n' "$version" > "$package/VERSION"
  printf 'linux-%s\n' "$arch" > "$package/PLATFORM"
  printf 'commit=%s\nworking_tree_dirty=%s\ngo=%s\nsource_date_epoch=%s\n' "$commit" "$dirty" "$actual_go" "$epoch" > "$package/BUILDINFO"
  cp scripts/rewrite/install.sh "$package/install.sh"
  cp scripts/rewrite/README.md "$package/README.txt"
  chmod 0755 "$package/folio" "$package/powerlinux" "$package/install.sh"
  chmod 0644 "$package/VERSION" "$package/PLATFORM" "$package/BUILDINFO" "$package/README.txt"
  name="folio-$version-linux-$arch.tar.gz"
  tar_args=(-C "$package")
  if tar --help 2>&1 | grep -q -- '--mtime'; then
    tar_args+=(--mtime="@$epoch")
  fi
  if tar --help 2>&1 | grep -q -- '--owner'; then
    tar_args+=(--owner=0 --group=0 --numeric-owner)
  fi
  if tar --help 2>&1 | grep -q -- '--sort=name'; then
    tar_args+=(--sort=name)
  fi
  tar "${tar_args[@]}" -cf - BUILDINFO PLATFORM README.txt VERSION install.sh folio powerlinux | gzip -n > "$output/$name.tmp"
  mv -f -- "$output/$name.tmp" "$output/$name"
done
(
  cd "$output"
  : > CHECKSUMS.txt.tmp
  for arch in "${arch_list[@]}"; do
    if command -v sha256sum >/dev/null 2>&1; then
      sha256sum -- "folio-$version-linux-$arch.tar.gz" >> CHECKSUMS.txt.tmp
    else
      shasum -a 256 "folio-$version-linux-$arch.tar.gz" >> CHECKSUMS.txt.tmp
    fi
  done
  mv -f CHECKSUMS.txt.tmp CHECKSUMS.txt
)
printf 'Built version %s in %s (Linux build outputs; target execution remains separate).\n' "$version" "$output"
