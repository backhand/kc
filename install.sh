#!/bin/sh
# kc installer — fetch the latest released binary from GitHub and put it on PATH.
#
#   curl -fsSL https://raw.githubusercontent.com/backhand/kc/master/install.sh | sh
#
# This is the trust-free install path (no Homebrew tap-trust needed). It downloads
# the prebuilt static binary from the GitHub release and verifies it against the
# release checksums.txt before installing.
#
# Environment overrides:
#   KC_VERSION      install a specific tag (e.g. v0.1.0); default: latest release
#   KC_INSTALL_DIR  install destination directory; default: /usr/local/bin
#                   (uses sudo if that dir needs root)
#
# Needs: curl or wget, tar, and sha256sum or shasum (for verification).
set -eu

REPO="backhand/kc"
BIN="kc"

# --- output helpers ----------------------------------------------------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	BOLD='\033[1m'; BLUE='\033[1;34m'; RED='\033[1;31m'; YEL='\033[1;33m'; RST='\033[0m'
else
	BOLD=''; BLUE=''; RED=''; YEL=''; RST=''
fi
info() { printf '%b==>%b %s\n' "$BLUE" "$RST" "$1"; }
warn() { printf '%bwarning:%b %s\n' "$YEL" "$RST" "$1" >&2; }
die()  { printf '%berror:%b %s\n' "$RED" "$RST" "$1" >&2; exit 1; }

# --- prerequisites -----------------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }

if have curl; then
	fetch()  { curl -fsSL "$1"; }          # url -> stdout
	fetchf() { curl -fsSL -o "$2" "$1"; }  # url, dest-file
elif have wget; then
	fetch()  { wget -qO- "$1"; }
	fetchf() { wget -qO "$2" "$1"; }
else
	die "need curl or wget"
fi

have tar || die "need tar"

if have sha256sum; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif have shasum; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	sha256() { echo ""; }   # no tool -> verification is skipped (warned below)
fi

# --- detect platform ---------------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
	linux | darwin) ;;
	*) die "unsupported OS: $os (kc ships linux and darwin builds)" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported architecture: $arch (kc ships amd64 and arm64 builds)" ;;
esac

# --- resolve the release tag -------------------------------------------------
latest_tag() {
	if have curl; then
		# Follow the /releases/latest redirect to /releases/tag/<tag>; no API rate limit.
		curl -fsSLI -o /dev/null -w '%{url_effective}\n' \
			"https://github.com/$REPO/releases/latest" 2>/dev/null \
			| sed -n 's#.*/releases/tag/##p' | tail -n1
	else
		fetch "https://api.github.com/repos/$REPO/releases/latest" \
			| grep -m1 '"tag_name":' \
			| sed -E 's/.*"tag_name": *"([^"]+)".*/\1/'
	fi
}

tag="${KC_VERSION:-}"
if [ -z "$tag" ]; then
	info "resolving latest release"
	tag=$(latest_tag)
	[ -n "$tag" ] || die "could not determine the latest release — set KC_VERSION to a specific tag (e.g. v0.1.0)"
fi
version="${tag#v}"   # goreleaser names archives without the leading v

archive="${BIN}_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

# --- download ----------------------------------------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

info "downloading ${BOLD}$archive${RST} ($tag)"
fetchf "$base/$archive" "$tmp/$archive" || die "download failed: $base/$archive"

# --- verify checksum ---------------------------------------------------------
if fetchf "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
	want=$(awk -v f="$archive" '$2 == f {print $1}' "$tmp/checksums.txt")
	got=$(sha256 "$tmp/$archive")
	if [ -z "$got" ]; then
		warn "no sha256 tool found — skipping checksum verification"
	elif [ -z "$want" ]; then
		warn "checksums.txt has no entry for $archive — skipping verification"
	elif [ "$want" != "$got" ]; then
		die "checksum mismatch for $archive (expected $want, got $got)"
	else
		info "checksum verified"
	fi
else
	warn "could not fetch checksums.txt — skipping verification"
fi

# --- extract -----------------------------------------------------------------
tar -xzf "$tmp/$archive" -C "$tmp" || die "failed to extract $archive"
[ -f "$tmp/$BIN" ] || die "archive did not contain a '$BIN' binary"
chmod +x "$tmp/$BIN"

# --- install -----------------------------------------------------------------
dest="${KC_INSTALL_DIR:-/usr/local/bin}"
src="$tmp/$BIN"

place() {
	# place <dir> [runner] — mkdir, copy, chmod via the optional runner (e.g. sudo).
	run=${2:-}
	$run mkdir -p "$1" 2>/dev/null \
		&& $run cp "$src" "$1/$BIN" 2>/dev/null \
		&& $run chmod 0755 "$1/$BIN" 2>/dev/null
}

if place "$dest"; then
	:
elif have sudo; then
	info "writing to $dest (needs sudo)"
	place "$dest" sudo \
		|| die "could not install to $dest — set KC_INSTALL_DIR to a writable directory (e.g. KC_INSTALL_DIR=\$HOME/.local/bin) and re-run"
else
	die "cannot write to $dest — set KC_INSTALL_DIR to a writable directory (e.g. KC_INSTALL_DIR=\$HOME/.local/bin) and re-run"
fi

info "installed ${BOLD}$BIN $tag${RST} -> $dest/$BIN"

# --- post-install hints ------------------------------------------------------
"$dest/$BIN" --version 2>/dev/null || true

# Nudge if the install dir isn't on PATH (common for a custom KC_INSTALL_DIR).
case ":$PATH:" in
	*":$dest:"*) ;;
	*) warn "$dest is not on your PATH — add: export PATH=\"$dest:\$PATH\"" ;;
esac

have kubectl || warn "kubectl not found — kc shells out to it at runtime (brew install kubernetes-cli)"
