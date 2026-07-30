#!/bin/sh

set -eu

usage() {
  printf '%s\n' \
    "Usage: $0 VERSION GOOS GOARCH [OUTPUT_DIRECTORY]" \
    "Example: $0 v0.1.0 darwin arm64 dist"
}

if [ "$#" -lt 3 ] || [ "$#" -gt 4 ]; then
  usage >&2
  exit 2
fi

version=$1
goos=$2
goarch=$3
output_directory=${4:-dist}

case "$version" in
  v*) ;;
  *)
    printf 'package-release: VERSION must start with v\n' >&2
    exit 2
    ;;
esac
case "$version" in
  *[!0-9A-Za-z.+-]*)
    printf 'package-release: VERSION contains unsupported characters\n' >&2
    exit 2
    ;;
esac
numeric_version=${version#v}
numeric_version=${numeric_version%%-*}
numeric_version=${numeric_version%%+*}
previous_ifs=$IFS
IFS=.
set -- $numeric_version
IFS=$previous_ifs
if [ "$#" -ne 3 ]; then
  printf 'package-release: VERSION must contain major, minor, and patch numbers\n' >&2
  exit 2
fi
for number in "$@"; do
  case "$number" in
    ""|*[!0-9]*)
      printf 'package-release: VERSION must contain numeric major, minor, and patch values\n' >&2
      exit 2
      ;;
  esac
done
case "$goos" in
  darwin|linux|windows) ;;
  *)
    printf 'package-release: unsupported GOOS %s\n' "$goos" >&2
    exit 2
    ;;
esac
case "$goarch" in
  amd64|arm64) ;;
  *)
    printf 'package-release: unsupported GOARCH %s\n' "$goarch" >&2
    exit 2
    ;;
esac

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$output_directory"
output_directory=$(CDPATH= cd -- "$output_directory" && pwd)
staging_root=$(mktemp -d "${TMPDIR:-/tmp}/remote-sync-release.XXXXXX")
trap 'rm -rf "$staging_root"' EXIT HUP INT TERM

package_name="remote-sync_${version}_${goos}_${goarch}"
package_directory="$staging_root/$package_name"
mkdir -p "$package_directory"

commit=${COMMIT_SHA:-$(git -C "$repository_root" rev-parse HEAD)}
commit=$(printf '%.12s' "$commit")
build_date=${BUILD_DATE:-$(git -C "$repository_root" show -s --format=%cI HEAD)}
ldflags="-s -w"
ldflags="$ldflags -X github.com/hawoond/remote-sync/internal/buildinfo.Version=$version"
ldflags="$ldflags -X github.com/hawoond/remote-sync/internal/buildinfo.Commit=$commit"
ldflags="$ldflags -X github.com/hawoond/remote-sync/internal/buildinfo.Date=$build_date"

extension=
if [ "$goos" = windows ]; then
  extension=.exe
fi

for command in sync-agent sync-server sync-migrate; do
  (
    cd "$repository_root"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath -ldflags "$ldflags" \
      -o "$package_directory/$command$extension" "./cmd/$command"
  )
done

cp "$repository_root/release/QUICKSTART_EN.txt" "$package_directory/"
cp "$repository_root/release/QUICKSTART_KO.txt" "$package_directory/"
cp "$repository_root/release/remote-sync.env.example" "$package_directory/"
cp "$repository_root/.env.example" "$package_directory/server.env.example"
cp -R "$repository_root/migrations" "$package_directory/"
printf '%s\n' "$version" >"$package_directory/VERSION"

case "$goos" in
  windows)
    cp "$repository_root/release/start-agent.cmd" \
      "$package_directory/Start Remote Sync.cmd"
    archive="$output_directory/$package_name.zip"
    rm -f "$archive"
    (
      cd "$staging_root"
      zip -qr "$archive" "$package_name"
    )
    ;;
  darwin)
    cp "$repository_root/release/start-agent.sh" \
      "$package_directory/Start Remote Sync.command"
    chmod 755 \
      "$package_directory/sync-agent" \
      "$package_directory/sync-server" \
      "$package_directory/sync-migrate" \
      "$package_directory/Start Remote Sync.command"
    archive="$output_directory/$package_name.tar.gz"
    rm -f "$archive"
    tar -C "$staging_root" -czf "$archive" "$package_name"
    ;;
  linux)
    cp "$repository_root/release/start-agent.sh" \
      "$package_directory/start-remote-sync.sh"
    chmod 755 \
      "$package_directory/sync-agent" \
      "$package_directory/sync-server" \
      "$package_directory/sync-migrate" \
      "$package_directory/start-remote-sync.sh"
    archive="$output_directory/$package_name.tar.gz"
    rm -f "$archive"
    tar -C "$staging_root" -czf "$archive" "$package_name"
    ;;
esac

printf '%s\n' "$archive"
