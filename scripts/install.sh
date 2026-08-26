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
# Download + checksum helpers (macOS and Linux)
###########################################
# Every archive is verified against https://oaica.com/download/SHA256SUMS
# (written by scripts/build_oaica.sh) before it is extracted. Cloudflare Pages
# once served an HTTP 200 with a truncated body (1.6 MB of 4.9 MB) during a
# cache fill; that used to surface as a cryptic zstd/tar error. Now a short
# body or a checksum mismatch is retried up to DOWNLOAD_ATTEMPTS times and
# then fails with a clear message.
#
# scripts/tests/install_checksum_test.sh extracts and exercises the block
# between the begin/end markers; keep the markers and keep the block
# self-contained (it may only depend on status/error/warning/available,
# TEMP_DIR and VER_PARAM).
# --- download helpers (begin) ---
DOWNLOAD_ATTEMPTS=3

# Print the SHA-256 hex digest of "$1", or nothing when no tool is available.
sha256_of() {
    if available sha256sum; then
        sha256sum "$1" | cut -d ' ' -f1
    elif available shasum; then
        shasum -a 256 "$1" | cut -d ' ' -f1
    fi
}

# Print the digest recorded for archive "$2" in the SHA256SUMS file "$1"
# (lines are "<sha256>  <filename>"). Returns 1 when there is no entry.
expected_sha256() {
    local sum name
    while read -r sum name; do
        name="${name#\*}"
        if [ "$name" = "$2" ]; then
            echo "$sum"
            return 0
        fi
    done < "$1"
    return 1
}

# Verify the downloaded archive "$1", published as "$2" under "$3", against
# "$3/SHA256SUMS". Returns 1 on a mismatch (or an unusable digest in
# SHA256SUMS) so the caller can retry. When verification is impossible — no
# sha256 tool, no SHA256SUMS on the server, no entry for this archive — it
# warns and returns 0 rather than blocking the install.
verify_archive() {
    local file="$1" name="$2" url_base="$3"
    local actual expected

    actual=$(sha256_of "$file")
    if [ -z "$actual" ]; then
        warning "Neither sha256sum nor shasum is available; skipping checksum verification of $name"
        return 0
    fi

    if ! curl --fail --silent --show-error --location --retry 3 \
            -o "$TEMP_DIR/SHA256SUMS" "${url_base}/SHA256SUMS${VER_PARAM}"; then
        warning "Could not download SHA256SUMS; skipping checksum verification of $name"
        return 0
    fi

    if ! expected=$(expected_sha256 "$TEMP_DIR/SHA256SUMS" "$name"); then
        warning "SHA256SUMS has no entry for $name; skipping checksum verification"
        return 0
    fi

    case "$expected" in
        *[!0-9a-fA-F]*|'') expected="<unreadable>" ;;
    esac
    if [ "$actual" = "$expected" ]; then
        status "Checksum OK: $name"
        return 0
    fi
    status "Checksum mismatch for $name: expected $expected, got $actual"
    return 1
}

# Download archive "$2" from "$1" to "$3" and verify it. Retries on a failed
# or short (truncated) transfer and on a checksum mismatch, up to
# DOWNLOAD_ATTEMPTS attempts, then fails with a clear error.
fetch_archive() {
    local url_base="$1" name="$2" dest="$3"
    local attempt=1 rc reason

    while :; do
        rm -f "$dest"
        rc=0
        curl --fail --show-error --location --progress-bar \
            -o "$dest" "${url_base}/${name}${VER_PARAM}" || rc=$?
        if [ "$rc" -eq 0 ] && [ ! -s "$dest" ]; then
            rc=18
        fi
        case "$rc" in
            0)
                if verify_archive "$dest" "$name" "$url_base"; then
                    return 0
                fi
                reason="checksum mismatch"
                ;;
            18) reason="short body (partial download)" ;; # CURLE_PARTIAL_FILE
            *) reason="download failed (curl exit $rc)" ;;
        esac

        if [ "$attempt" -ge "$DOWNLOAD_ATTEMPTS" ]; then
            error "$reason for $name after $DOWNLOAD_ATTEMPTS attempts. The download from oaica.com is incomplete or corrupt; please re-run the installer."
        fi
        attempt=$((attempt + 1))
        status "$reason, retrying ($attempt/$DOWNLOAD_ATTEMPTS)"
    done
}
# --- download helpers (end) ---

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
    # OAICA is a thin CLI (talks to api.oaica.com — OAICA_FORK_PLAN.md
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

    DARWIN_ARCHIVE="oaica-darwin-${DARWIN_ARCH}.zip"
    BINDIR="/usr/local/bin"

    status "Downloading OAICA for macOS ($DARWIN_ARCH)..."
    fetch_archive "https://oaica.com/download" "$DARWIN_ARCHIVE" "$TEMP_DIR/oaica-darwin.zip"

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

# Function to download, verify and extract with fallback from zst to tgz.
# The archive is downloaded to TEMP_DIR and checked against SHA256SUMS
# (fetch_archive) before anything is extracted into dest_dir.
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
        fetch_archive "$url_base" "${filename}.tar.zst" "$TEMP_DIR/${filename}.tar.zst"
        zstd -dc "$TEMP_DIR/${filename}.tar.zst" | $SUDO tar -xf - -C "${dest_dir}"
        return 0
    fi

    # Fall back to .tgz for older versions
    status "Downloading ${filename}.tgz"
    fetch_archive "$url_base" "${filename}.tgz" "$TEMP_DIR/${filename}.tgz"
    $SUDO tar -xzf - -C "${dest_dir}" < "$TEMP_DIR/${filename}.tgz"
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


# OAICA is a thin CLI (talks to api.oaica.com — OAICA_FORK_PLAN.md
# option 2). No local inference server ever runs here, so — unlike
# upstream Ollama, which this installer was forked from — there is
# nothing to systemd-service-ify and no local GPU driver to install.
# The only prerequisite is a working network path to api.oaica.com.
status "Install complete. Run 'oaica' from the command line."
status "Set OAICA_API_KEY before your first run: export OAICA_API_KEY=<your-key>"

}

main
