#!/usr/bin/env sh
# cux installer.
#
# Detects OS/arch, downloads the matching binary from the latest GitHub
# release, and places it at ~/.local/bin/cux (creating the dir if needed).
# Designed to run via:
#
#     curl -fsSL https://raw.githubusercontent.com/inulute/cux/main/scripts/install.sh | sh
#
# The script does not write outside $HOME and does not require sudo. It
# does, however, ask the user to add ~/.local/bin to their PATH if it is
# not already there.

set -eu

REPO="${CUX_REPO:-inulute/cux}"
INSTALL_DIR="${CUX_INSTALL_DIR:-$HOME/.local/bin}"
BIN_NAME="cux"

# Banner. Kept in lockstep with internal/branding/branding.go — when
# you change one, change the other. Suppressed by CUX_NO_BANNER=1 so
# scripted installers can stay quiet.
print_banner() {
    [ -n "${CUX_NO_BANNER:-}" ] && return 0
    cat <<'EOF'

  ██████╗ ██╗   ██╗ ██╗  ██╗
 ██╔════╝ ██║   ██║ ╚██╗██╔╝
 ██║      ██║   ██║  ╚███╔╝
 ██║      ██║   ██║  ██╔██╗
 ╚██████╗ ╚██████╔╝ ██╔╝ ██╗
  ╚═════╝  ╚═════╝  ╚═╝  ╚═╝

 CUX: Run multiple Claude Code Pro/Max accounts as one

EOF
}

print_banner

# --- detect platform ------------------------------------------------------

uname_s=$(uname -s)
uname_m=$(uname -m)

case "$uname_s" in
    Linux)  os="linux" ;;
    Darwin) os="darwin" ;;
    MINGW*|MSYS*|CYGWIN*) os="windows" ;;
    *)
        echo "cux-install: unsupported OS $uname_s" >&2
        exit 1
        ;;
esac

case "$uname_m" in
    x86_64|amd64) arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *)
        echo "cux-install: unsupported architecture $uname_m" >&2
        exit 1
        ;;
esac

# Linux/arm64 and darwin/arm64 are released; windows is amd64-only.
if [ "$os" = "windows" ] && [ "$arch" = "arm64" ]; then
    echo "cux-install: no Windows arm64 build available; falling back to amd64" >&2
    arch="amd64"
fi

archive_name="cux-${os}-${arch}"
if [ "$os" = "windows" ]; then
    archive_name="${archive_name}.exe"
fi

# --- locate latest release ------------------------------------------------

api_url="https://api.github.com/repos/${REPO}/releases/latest"
tag=$(curl -fsSL "$api_url" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
if [ -z "$tag" ]; then
    echo "cux-install: could not resolve latest release tag from $api_url" >&2
    exit 1
fi

download_url="https://github.com/${REPO}/releases/download/${tag}/${archive_name}"
echo "cux-install: downloading ${tag} for ${os}/${arch}"

mkdir -p "$INSTALL_DIR"
target="${INSTALL_DIR}/${BIN_NAME}"
if [ "$os" = "windows" ]; then
    target="${target}.exe"
fi

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT INT TERM

if ! curl -fsSL -o "$tmp" "$download_url"; then
    echo "cux-install: download failed: $download_url" >&2
    exit 1
fi

chmod +x "$tmp"
mv "$tmp" "$target"

# --- post-install hint ---------------------------------------------------

case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
        echo
        echo "cux-install: add this to your shell rc:"
        echo "    export PATH=\"$INSTALL_DIR:\$PATH\""
        ;;
esac

echo
echo "Installed $target"
echo "Next: run \`cux setup\` once to install the /switch slash command."
