#!/bin/sh
# phoned installer — one line, no build tools needed:
#
#   curl -fsSL https://raw.githubusercontent.com/Cyb0rgCode/proot/main/install.sh | sh
#
# Downloads the latest prebuilt static binary for your architecture
# (arm64 / amd64 / armv7) and installs it to ~/.local/bin. Falls back to
# building from source if no release asset matches and Go is available.
set -eu

OWNER_REPO="Cyb0rgCode/proot"
BIN_DIR="${PHONED_BIN_DIR:-$HOME/.local/bin}"

say() { printf '\033[1m[phoned]\033[0m %s\n' "$*"; }
die() { printf '\033[31m[phoned]\033[0m %s\n' "$*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || die "curl is required."

# --- Termux guard: phoned runs INSIDE a distro, not in the Termux shell ---
if [ -n "${TERMUX_VERSION:-}" ] || case "${PREFIX:-}" in *com.termux*) true;; *) false;; esac; then
    say "You're in the Termux shell itself. phoned is designed to run INSIDE a"
    say "proot-distro (where your apps and tmux live). Do this instead:"
    say "    pkg install proot-distro"
    say "    proot-distro install debian"
    say "    proot-distro login debian"
    say "then run this one-liner again inside the distro."
    say ""
    say "Also protect it from Android battery management:"
    say "    termux-wake-lock"
    say "    (and set Termux to 'Unrestricted' battery use in Android settings)"
    exit 0
fi

command -v tmux >/dev/null 2>&1 || say "WARNING: tmux not found — install it (apt install tmux) before running phoned."

# --- pick architecture ------------------------------------------------------
case "$(uname -m)" in
    aarch64|arm64)  ARCH="arm64" ;;
    x86_64|amd64)   ARCH="amd64" ;;
    armv7l|armv8l)  ARCH="armv7" ;;
    *)              ARCH="" ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fetched=""
if [ -n "$ARCH" ]; then
    URL="https://github.com/$OWNER_REPO/releases/latest/download/phoned-linux-$ARCH"
    say "downloading latest release for linux-$ARCH..."
    if curl -fsSL -o "$TMP/phoned" "$URL"; then
        fetched=1
    else
        say "no release binary available yet — will try building from source."
    fi
fi

if [ -z "$fetched" ]; then
    command -v go  >/dev/null 2>&1 || die "no prebuilt binary and Go not installed. Install Go >= 1.25, or wait for a release."
    command -v git >/dev/null 2>&1 || die "git is required to build from source."
    say "building from source..."
    git clone --depth 1 "https://github.com/$OWNER_REPO" "$TMP/src" >/dev/null 2>&1 || die "git clone failed"
    (cd "$TMP/src" && go build -ldflags "-s -w" -o "$TMP/phoned" .) || die "build failed"
fi

mkdir -p "$BIN_DIR"
install -m 0755 "$TMP/phoned" "$BIN_DIR/phoned"
say "installed to $BIN_DIR/phoned"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say "NOTE: add $BIN_DIR to your PATH:  export PATH=\"\$PATH:$BIN_DIR\"" ;;
esac

say ""
say "Start it with:   phoned serve"
say "Then open the printed link (or scan the QR) from your phone or PC."
say "For access from anywhere, put it behind Tailscale or an SSH tunnel."
