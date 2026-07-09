#!/bin/sh
# muxboard installer — proot-distro (Termux), plain Linux, or any box with Go.
#
#   curl -fsSL https://raw.githubusercontent.com/Cyb0rgCode/proot/main/install.sh | sh
#
# Builds from source (needs go + tmux) and installs to ~/.local/bin.
# When binary releases exist this script will prefer downloading them.
set -eu

REPO="https://github.com/Cyb0rgCode/proot"
BIN_DIR="${MUXBOARD_BIN_DIR:-$HOME/.local/bin}"

say()  { printf '\033[1m[muxboard]\033[0m %s\n' "$*"; }
die()  { printf '\033[31m[muxboard]\033[0m %s\n' "$*" >&2; exit 1; }

# --- environment detection -------------------------------------------------
ENV_KIND="linux"
if [ -n "${TERMUX_VERSION:-}" ] || case "${PREFIX:-}" in *com.termux*) true;; *) false;; esac; then
    ENV_KIND="termux"
fi
if [ -f /proc/sys/kernel/osrelease ] && grep -qi proot /proc/sys/kernel/osrelease 2>/dev/null; then
    ENV_KIND="proot"
fi
say "detected environment: $ENV_KIND"

if [ "$ENV_KIND" = "termux" ]; then
    say "You're in the Termux shell itself. muxboard is designed to run INSIDE a"
    say "proot-distro (where your apps and tmux live). Recommended:"
    say "    pkg install proot-distro && proot-distro install debian"
    say "    proot-distro login debian"
    say "then run this installer again inside the distro."
    say "Also set up the Termux side so Android doesn't kill your server:"
    say "    termux-wake-lock        # hold a wake lock"
    say "    (and exempt Termux from battery optimization in Android settings)"
    exit 0
fi

# --- dependencies ----------------------------------------------------------
command -v tmux >/dev/null 2>&1 || die "tmux is required. Install it first (apt install tmux)."
command -v go   >/dev/null 2>&1 || die "go is required to build (apt install golang-go, or use a release binary once available)."

# --- build -----------------------------------------------------------------
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
say "fetching source..."
if command -v git >/dev/null 2>&1; then
    git clone --depth 1 "$REPO" "$TMP/src" >/dev/null 2>&1 || die "git clone failed"
else
    die "git is required (apt install git)."
fi
say "building..."
(cd "$TMP/src" && go build -ldflags "-s -w" -o "$TMP/muxboard" .) || die "build failed"

mkdir -p "$BIN_DIR"
install -m 0755 "$TMP/muxboard" "$BIN_DIR/muxboard"
say "installed to $BIN_DIR/muxboard"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say "NOTE: add $BIN_DIR to your PATH:  export PATH=\"\$PATH:$BIN_DIR\"" ;;
esac

say ""
say "Start it with:   muxboard serve"
say "Then open the printed link (or scan the QR) from your phone or PC."
say "For access from anywhere, put it behind Tailscale or an SSH tunnel."
