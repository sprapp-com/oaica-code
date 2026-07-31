#!/bin/sh
# This script installs OAICA on Linux and macOS.
# It detects the current operating system architecture and installs the appropriate version of OAICA.

# Wrap script in main function so that a truncated partial download doesn't end
# up executing half a script.
main() {

set -eu

red="$( (/usr/bin/tput bold || :; /usr/bin/tput setaf 1 || :) 2>&-)"
plain="$( (/usr/bin/tput sgr0 || :) 2>&-)"

status() { echo ">>> $*" >&2; }
error() { echo "${red}ERROR:${plain} $*"; exit 1; }
warning() { echo "${red}WARNING:${plain} $*"; }

TEMP_DIR=$(mktemp -d)
cleanup() { rm -rf $TEMP_DIR; }
trap cleanup EXIT

available() { command -v $1 >/dev/null; }
require() {
    local MISSING=''
    for TOOL in $*; do
        if ! available $TOOL; then
            MISSING="$MISSING $TOOL"
        fi
    done

    echo $MISSING
}

OS="$(uname -s)"
ARCH=$(uname -m)
case "$ARCH" in
    x86_64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) error "Unsupported architecture: $ARCH" ;;
esac

VER_PARAM="${OAICA_VERSION:+?version=$OAICA_VERSION}"

###########################################
# Uninstall
###########################################
# OAICA_UNINSTALL=1 curl -fsSL https://oaica.com/install.sh | bash
if [ -n "${OAICA_UNINSTALL:-}" ]; then
    UNINSTALL_SUDO=
    [ "$(id -u)" -ne 0 ] && available sudo && UNINSTALL_SUDO="sudo"

    FOUND=0
    for BINDIR in /usr/local/bin /usr/bin /bin; do
        if [ -e "$BINDIR/oaica" ]; then
            status "Removing $BINDIR/oaica"
            $UNINSTALL_SUDO rm -f "$BINDIR/oaica"
            FOUND=1
        fi
        INSTALL_DIR="$(dirname "$BINDIR")"
        if [ -d "$INSTALL_DIR/lib/oaica" ]; then
            status "Removing $INSTALL_DIR/lib/oaica"
            $UNINSTALL_SUDO rm -rf "$INSTALL_DIR/lib/oaica"
            FOUND=1
        fi
    done

    if [ "$FOUND" -eq 0 ]; then
        status "OAICA is not installed."
    else
        status "OAICA has been uninstalled."
    fi
    exit 0
fi

###########################################
# macOS
###########################################

if [ "$OS" = "Darwin" ]; then
    # OAICA is a thin CLI (talks to api.sprapp.com — OAICA_FORK_PLAN.md
    # option 2), not a GUI desktop app, so unlike upstream Ollama this
    # ships a plain binary in a zip, not an OAICA.app bundle.
    NEEDS=$(require curl unzip)
    if [ -n "$NEEDS" ]; then
        status "ERROR: The following tools are required but missing:"
        for NEED in $NEEDS; do
            echo "  - $NEED"
        done
        exit 1
    fi

    ARCH=$(uname -m)
    case "$ARCH" in
        arm64|aarch64) DARWIN_ARCH="arm64" ;;
        x86_64|amd64) DARWIN_ARCH="amd64" ;;
        *) error "Unsupported macOS architecture: $ARCH" ;;
    esac

    DOWNLOAD_URL="https://oaica.com/download/oaica-darwin-${DARWIN_ARCH}.zip${VER_PARAM}"
    BINDIR="/usr/local/bin"

    status "Downloading OAICA for macOS ($DARWIN_ARCH)..."
    curl --fail --show-error --location --progress-bar \
        -o "$TEMP_DIR/oaica-darwin.zip" "$DOWNLOAD_URL"

    status "Installing OAICA to $BINDIR..."
    unzip -q "$TEMP_DIR/oaica-darwin.zip" -d "$TEMP_DIR"
    mkdir -p "$BINDIR" 2>/dev/null || sudo mkdir -p "$BINDIR"
    if [ -w "$BINDIR" ]; then
        install -m755 "$TEMP_DIR/bin/oaica" "$BINDIR/oaica"
    else
        status "Installing to $BINDIR requires sudo..."
        sudo install -m755 "$TEMP_DIR/bin/oaica" "$BINDIR/oaica"
    fi

    status "Install complete. You can now run 'oaica'."
    exit 0
fi

###########################################
# Linux
###########################################

[ "$OS" = "Linux" ] || error 'This script is intended to run on Linux and macOS only.'

IS_WSL2=false

KERN=$(uname -r)
case "$KERN" in
    *icrosoft*WSL2 | *icrosoft*wsl2) IS_WSL2=true;;
    *icrosoft) error "Microsoft WSL1 is not currently supported. Please use WSL2 with 'wsl --set-version <distro> 2'" ;;
    *) ;;
esac

SUDO=
if [ "$(id -u)" -ne 0 ]; then
    # Running as root, no need for sudo
    if ! available sudo; then
        error "This script requires superuser permissions. Please re-run as root."
    fi

    SUDO="sudo"
fi

NEEDS=$(require curl awk grep sed tee xargs)
if [ -n "$NEEDS" ]; then
    status "ERROR: The following tools are required but missing:"
    for NEED in $NEEDS; do
        echo "  - $NEED"
    done
    exit 1
fi

# Function to download and extract with fallback from zst to tgz
download_and_extract() {
    local url_base="$1"
    local dest_dir="$2"
    local filename="$3"

    # Check if .tar.zst is available
    if curl --fail --silent --head --location "${url_base}/${filename}.tar.zst${VER_PARAM}" >/dev/null 2>&1; then
        # zst file exists - check if we have zstd tool
        if ! available zstd; then
            error "This version requires zstd for extraction. Please install zstd and try again:
  - Debian/Ubuntu: sudo apt-get install zstd
  - RHEL/CentOS/Fedora: sudo dnf install zstd
  - Arch: sudo pacman -S zstd"
        fi

        status "Downloading ${filename}.tar.zst"
        curl --fail --show-error --location --progress-bar \
            "${url_base}/${filename}.tar.zst${VER_PARAM}" | \
            zstd -d | $SUDO tar -xf - -C "${dest_dir}"
        return 0
    fi

    # Fall back to .tgz for older versions
    status "Downloading ${filename}.tgz"
    curl --fail --show-error --location --progress-bar \
        "${url_base}/${filename}.tgz${VER_PARAM}" | \
        $SUDO tar -xzf - -C "${dest_dir}"
}

for BINDIR in /usr/local/bin /usr/bin /bin; do
    echo $PATH | grep -q $BINDIR && break || continue
done
OAICA_INSTALL_DIR=$(dirname ${BINDIR})

if [ -d "$OAICA_INSTALL_DIR/lib/oaica" ] ; then
    status "Cleaning up old version at $OAICA_INSTALL_DIR/lib/oaica"
    $SUDO rm -rf "$OAICA_INSTALL_DIR/lib/oaica"
fi
status "Installing oaica to $OAICA_INSTALL_DIR"
$SUDO install -o0 -g0 -m755 -d $BINDIR
$SUDO install -o0 -g0 -m755 -d "$OAICA_INSTALL_DIR/lib/oaica"
download_and_extract "https://oaica.com/download" "$OAICA_INSTALL_DIR" "oaica-linux-${ARCH}"

if [ "$OAICA_INSTALL_DIR/bin/oaica" != "$BINDIR/oaica" ] ; then
    status "Making oaica accessible in the PATH in $BINDIR"
    $SUDO ln -sf "$OAICA_INSTALL_DIR/oaica" "$BINDIR/oaica"
fi


# OAICA is a thin CLI (talks to api.sprapp.com — OAICA_FORK_PLAN.md
# option 2). No local inference server ever runs here, so — unlike
# upstream Ollama, which this installer was forked from — there is
# nothing to systemd-service-ify and no local GPU driver to install.
# The only prerequisite is a working network path to api.sprapp.com.
status "Install complete. Run 'oaica' from the command line."
status "Set OAICA_API_KEY before your first run: export OAICA_API_KEY=<your-key>"

}

main
