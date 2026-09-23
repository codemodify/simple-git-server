#!/bin/sh
# Install simple-git-server: detect the platform, fetch the matching release
# assets, verify them, and put them on PATH.
#
#   curl -fsSL https://raw.githubusercontent.com/codemodify/simple-git-server/dev/install.sh | sh
#
# Environment:
#   VERSION      release tag to install, or "latest"   (default: latest)
#   INSTALL_DIR  where to put the binaries             (default: /usr/local/bin)
#   HELPERS      also install the optional push/SSH helpers, 1 or 0 (default: 1)
#   REPO         owner/name to download from           (default: codemodify/simple-git-server)
#
# simple-git-server is required. The helpers (simple-git-server-http-write,
# simple-git-server-ssh-read-write) are optional: if a release does not publish
# them, the script says so and carries on.

set -eu

REPO="${REPO:-codemodify/simple-git-server}"
VERSION="${VERSION:-latest}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
HELPERS="${HELPERS:-1}"

info() { printf '%s\n' "$*" >&2; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# --- which asset does this machine need? ---------------------------------
os=$(uname -s 2>/dev/null || echo unknown)
arch=$(uname -m 2>/dev/null || echo unknown)

case "$os" in
	Linux)                           plat=linux ;;
	Darwin)                          plat=osx ;;
	MINGW*|MSYS*|CYGWIN*|Windows_NT) plat=windows ;;
	*) die "unsupported operating system: $os" ;;
esac

case "$plat:$arch" in
	linux:x86_64|linux:amd64)              suffix="linux-x86_64" ;;
	linux:aarch64|linux:arm64)             suffix="linux-aarch64" ;;
	linux:armv7l|linux:armv8l)             suffix="linux-arm-v7" ;;
	linux:armv6l)                          suffix="linux-arm-v6" ;;
	linux:armv5*)                          suffix="linux-arm-v5" ;;
	osx:x86_64)                            suffix="osx-x86_64" ;;
	osx:arm64|osx:aarch64)                 suffix="osx-aarch64" ;;
	windows:x86_64|windows:amd64)          suffix="windows-x86_64.exe" ;;
	windows:i686|windows:i386|windows:x86) suffix="windows-x86.exe" ;;
	windows:aarch64|windows:arm64)         suffix="windows-aarch64.exe" ;;
	linux:i686|linux:i386)
		die "no 32-bit x86 Linux build is published; build from source:
  go build -o simple-git-server ." ;;
	*) die "unsupported platform: $os/$arch" ;;
esac

exe=""
case "$suffix" in *.exe) exe=".exe" ;; esac

# --- fetch ----------------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	die "need curl or wget on PATH"
fi

tmpdir=$(mktemp -d 2>/dev/null || mktemp -d -t simple-git-server) ||
	die "cannot create a temporary directory"
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM

as_root() {
	if [ "$(id -u 2>/dev/null || echo 0)" = 0 ]; then
		"$@"
	elif command -v sudo >/dev/null 2>&1; then
		sudo "$@"
	elif command -v doas >/dev/null 2>&1; then
		doas "$@"
	else
		die "cannot write to $INSTALL_DIR and neither sudo nor doas is available.
Install somewhere you own instead:
  INSTALL_DIR=\$HOME/.local/bin sh install.sh"
	fi
}

# install_one <binary-name> <required:1|0>
install_one() {
	name=$1
	required=$2
	target="$name$exe"
	tmp="$tmpdir/$target"

	if [ "$VERSION" = latest ]; then
		url="https://github.com/$REPO/releases/latest/download/${name}_${suffix}"
	else
		url="https://github.com/$REPO/releases/download/$VERSION/${name}_${suffix}"
	fi

	if ! fetch "$url" "$tmp" 2>/dev/null; then
		if [ "$required" = 1 ]; then
			die "download failed: $url
No release asset was found. Either this repository has not published a release
yet, or $VERSION does not exist. Build from source instead:
  go build -o $name ."
		fi
		warn "$name is not published for $VERSION; skipping (build it with: go build -o $name ./cmd/...)"
		return 0
	fi

	[ -s "$tmp" ] || die "downloaded an empty file from $url"

	# A 404 or a proxy interstitial can arrive as HTML with a 200 status; only
	# accept real executables.
	magic=$(od -An -tx1 -N4 < "$tmp" 2>/dev/null | tr -d ' \n' || echo "")
	case "$magic" in
		7f454c46*)                               ;;  # ELF
		cffaedfe*|cefaedfe*|cafebabe*|bebafeca*) ;;  # Mach-O, incl. universal
		4d5a*)                                   ;;  # PE / MZ
		*) die "what came back for $name is not an executable (magic: ${magic:-none}).
Check $url in a browser - it may be an error page." ;;
	esac

	chmod +x "$tmp"

	# Catch a wrong-architecture guess before it lands on PATH. Skipped for
	# Windows binaries fetched from a POSIX shell, which cannot run them here.
	if [ "$plat" != windows ]; then
		if ! "$tmp" --version >/dev/null 2>&1; then
			die "the downloaded $name would not run on this machine.
Detected $os/$arch -> $suffix; that mapping may be wrong for your system."
		fi
	fi

	if [ -w "$INSTALL_DIR" ]; then
		mv -f "$tmp" "$INSTALL_DIR/$target"
	else
		as_root mv -f "$tmp" "$INSTALL_DIR/$target"
	fi
	info "installed $INSTALL_DIR/$target"
}

if [ ! -d "$INSTALL_DIR" ]; then
	mkdir -p "$INSTALL_DIR" 2>/dev/null || as_root mkdir -p "$INSTALL_DIR"
fi

info "simple-git-server: $os/$arch -> $suffix ($VERSION)"
install_one simple-git-server 1
if [ "$HELPERS" = 1 ]; then
	install_one simple-git-server-http-write 0
	install_one simple-git-server-ssh-read-write 0
fi

case ":${PATH:-}:" in
	*":$INSTALL_DIR:"*) ;;
	*) warn "$INSTALL_DIR is not on your PATH; add it or use the full path" ;;
esac
