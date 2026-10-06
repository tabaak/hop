#!/bin/sh
# Installs the latest hop release from GitHub.
#
#   curl -fsSL https://raw.githubusercontent.com/tabaak/hop/main/install.sh | sh
#
# Installs the server instead (Linux only):
#
#   curl -fsSL https://raw.githubusercontent.com/tabaak/hop/main/install.sh | sh -s -- hopd
#
# Environment:
#   HOP_VERSION      a release tag such as v1.1.0 (default: the latest release)
#   HOP_INSTALL_DIR  where to put the binary (default: /usr/local/bin, or
#                    ~/.local/bin when /usr/local/bin isn't writable and sudo
#                    isn't available)
set -eu

REPO="tabaak/hop"
BINARY="${1:-hop}"

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

case "$BINARY" in
hop | hopd) ;;
*) fail "unknown binary \"$BINARY\" (expected hop or hopd)" ;;
esac

case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) fail "unsupported OS $(uname -s); download a release manually from https://github.com/$REPO/releases" ;;
esac
if [ "$BINARY" = hopd ] && [ "$os" != linux ]; then
	fail "hopd is only released for Linux"
fi

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "unsupported architecture $(uname -m)" ;;
esac

if command -v curl >/dev/null 2>&1; then
	download() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	download() { wget -q -O "$2" "$1"; }
else
	fail "curl or wget is required"
fi

if [ -n "${HOP_VERSION:-}" ]; then
	base="https://github.com/$REPO/releases/download/$HOP_VERSION"
else
	base="https://github.com/$REPO/releases/latest/download"
fi
archive="${BINARY}_${os}_${arch}.tar.gz"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "Downloading $archive..."
download "$base/$archive" "$tmp/$archive" || fail "could not download $base/$archive"
download "$base/checksums.txt" "$tmp/checksums.txt" || fail "could not download checksums.txt"

# Refuse to install a binary that doesn't match the release's checksum.
expected="$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$expected" ] || fail "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "$tmp/$archive" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
	actual="$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')"
else
	fail "sha256sum or shasum is required to verify the download"
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch for $archive"

tar -xzf "$tmp/$archive" -C "$tmp" "$BINARY"

dir="${HOP_INSTALL_DIR:-/usr/local/bin}"
sudo=""
if [ -z "${HOP_INSTALL_DIR:-}" ] && ! [ -w "$dir" ]; then
	if command -v sudo >/dev/null 2>&1; then
		sudo="sudo"
	else
		dir="$HOME/.local/bin"
	fi
fi
$sudo mkdir -p "$dir"
$sudo install -m 755 "$tmp/$BINARY" "$dir/$BINARY"

echo "Installed $("$dir/$BINARY" version 2>/dev/null | head -n 1 || echo "$BINARY") to $dir/$BINARY"
case ":$PATH:" in
*":$dir:"*) ;;
*) echo "Note: $dir is not on your PATH; add it to use $BINARY from anywhere." ;;
esac
