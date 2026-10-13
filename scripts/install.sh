#!/bin/sh
# Pinned, per-user Archer bootstrap. No downloaded program is executed here.
set -eu
umask 077

die() { printf '%s\n' "archer: $*" >&2; exit 1; }
version=${ARCHER_VERSION:-v0.3.0-alpha.3}
printf '%s\n' "$version" | awk '/^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$/ {ok=1} END {exit !ok}' || die 'ARCHER_VERSION must be pinned, for example v0.3.0-alpha.3'
case "$version" in *[!A-Za-z0-9.-]*|*..*|*.) die 'unsafe ARCHER_VERSION' ;; esac
case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) die 'supported systems: Linux and macOS' ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) die 'supported architectures: amd64 and arm64' ;; esac
artifact=archer_${os}_${arch}
root=${ARCHER_HOME:-${HOME:?HOME is required}/.archer}
case "$root" in /*) ;; *) die 'ARCHER_HOME must be absolute' ;; esac
case "/$root/" in */../*|*/./*) die 'ARCHER_HOME may not contain dot components' ;; esac
root=${root%/}
[ -n "$root" ] && [ "$root" != / ] || die 'ARCHER_HOME cannot be a filesystem root'

check_dirs() {
    probe=$1
    while [ "$probe" != / ] && [ -n "$probe" ]; do
        [ ! -L "$probe" ] || die "symlink directory refused: $probe"
        if [ -e "$probe" ]; then [ -d "$probe" ] || die "not a directory: $probe"; fi
        probe=${probe%/*}
        [ -n "$probe" ] || probe=/
    done
}
digest() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
    else die 'sha256sum or shasum is required'; fi
}
check_dirs "$root"
check_dirs "$root/bin"
check_dirs "$root/versions"
check_dirs "$root/versions/$version"
tmp_base=${TMPDIR:-/tmp}
# macOS exposes its ordinary temp directory through /var and /tmp symlinks.
# Resolve that existing directory before creating our unique download directory.
tmp_base=$(cd "$tmp_base" && pwd -P) || die 'temporary directory is unavailable'
check_dirs "$tmp_base"
tmp=$(mktemp -d "$tmp_base/archer-download.XXXXXX") || die 'cannot create download directory'
stage=
link_tmp=
cleanup() {
    [ -z "$link_tmp" ] || rm -f "$link_tmp"
    [ -z "$stage" ] || rm -rf "$stage"
    rm -rf "$tmp"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM
fetch() {
    name=$1; limit=$2
    if [ -n "${ARCHER_DIST_DIR:-}" ]; then
        [ -f "$ARCHER_DIST_DIR/$name" ] && [ ! -L "$ARCHER_DIST_DIR/$name" ] || die "missing regular distribution file: $name"
        size=$(wc -c < "$ARCHER_DIST_DIR/$name" | tr -d ' ')
        [ "$size" -le "$limit" ] || die "distribution file exceeds size limit: $name"
        cp "$ARCHER_DIST_DIR/$name" "$tmp/$name"
    else
        command -v curl >/dev/null 2>&1 || die 'curl is required to download Archer'
        curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
            --connect-timeout 15 --max-time 120 --max-filesize "$limit" \
            "https://github.com/Parallax-Web-Services/archer/releases/download/$version/$name" \
            --output "$tmp/$name" || die "download failed: $name"
        size=$(wc -c < "$tmp/$name" | tr -d ' ')
        [ "$size" -le "$limit" ] || die "download exceeds size limit: $name"
    fi
}
fetch SHA256SUMS 1048576
fetch "$artifact" 134217728
expected=$(awk -v file="$artifact" '$2 == file { print $1 }' "$tmp/SHA256SUMS")
[ "${#expected}" -eq 64 ] || die "missing or duplicate checksum for $artifact"
case "$expected" in *[!0-9a-fA-F]*) die 'invalid SHA256SUMS entry' ;; esac
actual=$(digest "$tmp/$artifact")
expected=$(printf '%s' "$expected" | tr 'A-F' 'a-f')
[ "$actual" = "$expected" ] || die "checksum mismatch for $artifact"

marker=$root/.archer-managed-v1
if [ -e "$marker" ] || [ -L "$marker" ]; then
    [ -f "$marker" ] && [ ! -L "$marker" ] && [ "$(cat "$marker")" = 'archer-install-root/v1' ] || die 'invalid installation ownership marker'
else
    [ ! -e "$root/versions" ] && [ ! -L "$root/versions" ] || die 'unmanaged versions directory refused'
    for name in archer archer.exe archer.cmd archer.ps1 archer.bat; do
        [ ! -e "$root/bin/$name" ] && [ ! -L "$root/bin/$name" ] || die "unmanaged binary refused: $root/bin/$name"
    done
fi
target=$root/bin/archer
if [ -e "$target" ] || [ -L "$target" ]; then
    [ -L "$target" ] || die "unmanaged binary refused: $target"
    old=$(readlink "$target")
    case "$old" in
        ../versions/v*/bin/archer) old_version=${old#../versions/} ;;
        "$root"/versions/v*/bin/archer) old_version=${old#"$root/versions/"} ;;
        *) die 'unmanaged Archer symlink refused' ;;
    esac
    old_version=${old_version%/bin/archer}
    case "$old_version" in *[!A-Za-z0-9.-]*|*..*|*.) die 'unsafe installed version' ;; esac
    check_dirs "$root/versions/$old_version/bin"
    [ -f "$root/versions/$old_version/.archer-version-v1" ] && [ ! -L "$root/versions/$old_version/.archer-version-v1" ] || die 'unmanaged Archer symlink refused'
    [ "$(cat "$root/versions/$old_version/.archer-version-v1")" = "$old_version" ] || die 'invalid installed version marker'
    old=$root/versions/$old_version/bin/archer
    [ -f "$old" ] && [ ! -L "$old" ] || die 'invalid installed binary'
fi
version_dir=$root/versions/$version
if [ -e "$version_dir" ]; then
    check_dirs "$version_dir/bin"
    [ -f "$version_dir/.archer-version-v1" ] && [ ! -L "$version_dir/.archer-version-v1" ] || die 'unmanaged version directory refused'
    [ "$(cat "$version_dir/.archer-version-v1")" = "$version" ] || die 'invalid version marker'
    [ -f "$version_dir/bin/archer" ] && [ ! -L "$version_dir/bin/archer" ] || die 'unmanaged version binary refused'
    [ "$(digest "$version_dir/bin/archer")" = "$expected" ] || die 'installed version differs from the pinned release'
else
    mkdir -p "$root/bin" "$root/versions"
    [ -e "$marker" ] || printf '%s\n' 'archer-install-root/v1' > "$marker"
    stage=$(mktemp -d "$root/.archer-install.XXXXXX") || die 'cannot create staging directory'
    mkdir "$stage/bin"
    cp "$tmp/$artifact" "$stage/bin/archer"
    chmod 755 "$stage/bin/archer"
    printf '%s\n' "$version" > "$stage/.archer-version-v1"
    mv "$stage" "$version_dir"
    stage=
fi
link_tmp=$(mktemp "$root/bin/.archer-link.XXXXXX") || die 'cannot stage launcher'
rm -f "$link_tmp"
ln -s "../versions/$version/bin/archer" "$link_tmp"
mv -f "$link_tmp" "$target"
link_tmp=
printf '%s\n' "Installed Archer $version to $target" 'Add this directory to PATH in your shell configuration:'
printf '  %s\n' "$root/bin"
printf '%s\n' 'For the default location: export PATH="$HOME/.archer/bin:$PATH"'
