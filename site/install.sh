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
cleanup() { rm -rf "$TEMP_DIR"; }
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

###########################################
# Download + checksum helpers (macOS and Linux)
###########################################
# Archives and SHA256SUMS come from the GitHub release for the version being
# installed — the artifacts CI built from that tag — not from a copy published
# on oaica.com. That copy was maintained by hand and drifted: it was still
# serving 0.5.45 while the newest release was 0.5.46, so a user asking for the
# latest got something older with no indication. A release URL cannot drift
# from its tag.
#
# Every archive is verified against that release's SHA256SUMS (written by
# scripts/build_oaica.sh) before it is extracted. Cloudflare Pages once served
# an HTTP 200 with a truncated body (1.6 MB of 4.9 MB) during a cache fill;
# that used to surface as a cryptic zstd/tar error. Now a short body or a
# checksum mismatch is retried up to DOWNLOAD_ATTEMPTS times and then fails
# with a clear message.
#
# scripts/tests/install_checksum_test.sh extracts and exercises the block
# between the begin/end markers; keep the markers and keep the block
# self-contained (it may only depend on status/error/warning/available,
# TEMP_DIR, and the OAICA_* environment variables).
# --- download helpers (begin) ---
DOWNLOAD_ATTEMPTS=3

# release_download_base prints the base URL the archives and SHA256SUMS are
# fetched from. Precedence:
#
#   OAICA_DOWNLOAD_BASE   overrides everything — a mirror, or an air-gapped
#                         host serving the release assets.
#   OAICA_VERSION         pins that version's release (tag oaica-v<version>);
#                         a leading "v" is accepted.
#   neither               the latest published release.
#
# OAICA_RELEASE_REPO overrides the repository (a fork, or an internal mirror of
# the releases) and defaults to this one.
release_download_base() {
    if [ -n "${OAICA_DOWNLOAD_BASE:-}" ]; then
        printf '%s' "${OAICA_DOWNLOAD_BASE%/}"
        return 0
    fi
    repo="${OAICA_RELEASE_REPO:-sprapp-com/oaica-code}"
    if [ -n "${OAICA_VERSION:-}" ]; then
        # Either spelling pins: a bare semver (0.5.46) or the full tag the
        # releases page shows (oaica-v0.5.46). Only "v" used to be stripped,
        # so a tag produced the tag prefix twice — "oaica-voaica-v0.5.46",
        # a 404 — which is exactly what a user copying the tag from the
        # releases page typed (2026-09-26 audit).
        ver="${OAICA_VERSION#oaica-v}"
        ver="${ver#v}"
        printf 'https://github.com/%s/releases/download/oaica-v%s' "$repo" "$ver"
        return 0
    fi
    printf 'https://github.com/%s/releases/latest/download' "$repo"
}

# curl options that keep an https download from being taken over a plain-http redirect: TLS to the named
# host is the only protection on the unpinned path, and SHA256SUMS comes from the same hop as the archive.
# Prints nothing for a non-https base (a local mirror), which is the operator's own choice.
redirect_guard() {
    case "$1" in
        https://*) printf '%s' '--proto-redir =https' ;;
    esac
}

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
        # A SHA256SUMS that went through Windows / git autocrlf ends its lines in CR: the entry was never found and
        # the archive installed unchecked (2026-09-30 audit, round 139, F139-A-1).
        name="${name%$(printf '\r')}"
        name="${name#\*}"
        if [ "$name" = "$2" ]; then
            printf '%s\n' "$sum" | tr -d '\r' | tr 'A-F' 'a-f'
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

    # On the default origin (the GitHub release) SHA256SUMS always exists, so a missing file, a missing entry or a
    # missing sha256 tool there is a failure, not a reason to install unchecked; a mirror (OAICA_DOWNLOAD_BASE) may
    # still fall back to a warning unless OAICA_REQUIRE_CHECKSUM=1 (round 139, F139-A-2).
    local strict=""
    if [ -z "${OAICA_DOWNLOAD_BASE:-}" ] || [ "${OAICA_REQUIRE_CHECKSUM:-}" = "1" ]; then strict=1; fi

    actual=$(sha256_of "$file" | tr 'A-F' 'a-f')
    if [ -z "$actual" ]; then
        if [ -n "$strict" ]; then error "Neither sha256sum nor shasum is available; refusing to install $name unverified"; fi
        warning "Neither sha256sum nor shasum is available; skipping checksum verification of $name"
        return 0
    fi

    if ! curl --fail --silent --show-error --location $(redirect_guard "$url_base") --retry 3 \
            -o "$TEMP_DIR/SHA256SUMS" "${url_base}/SHA256SUMS"; then
        if [ -n "$strict" ]; then status "Could not download SHA256SUMS for $name"; return 1; fi
        warning "Could not download SHA256SUMS; skipping checksum verification of $name"
        return 0
    fi

    if ! expected=$(expected_sha256 "$TEMP_DIR/SHA256SUMS" "$name"); then
        if [ -n "$strict" ]; then status "SHA256SUMS has no entry for $name"; return 1; fi
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
        curl --fail --show-error --location $(redirect_guard "$url_base") --progress-bar \
            -o "$dest" "${url_base}/${name}" || rc=$?
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
            error "$reason for $name after $DOWNLOAD_ATTEMPTS attempts. The download from $url_base is incomplete or corrupt; please re-run the installer."
        fi
        attempt=$((attempt + 1))
        status "$reason, retrying ($attempt/$DOWNLOAD_ATTEMPTS)"
    done
}
# --- download helpers (end) ---

DOWNLOAD_BASE="$(release_download_base)"
if [ -n "${OAICA_VERSION:-}" ]; then
    status "Installing OAICA ${OAICA_VERSION#v} from $DOWNLOAD_BASE"
fi

###########################################
# Uninstall
###########################################
# curl -fsSL https://oaica.com/install.sh | OAICA_UNINSTALL=1 bash
# The variable must go to `bash`, not to `curl` — `OAICA_UNINSTALL=1 curl … |
# bash` sets it only in curl's environment, so the piped shell never sees it
# and the install runs instead of the uninstall.
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
    fetch_archive "$DOWNLOAD_BASE" "$DARWIN_ARCHIVE" "$TEMP_DIR/oaica-darwin.zip"

    status "Installing OAICA to $BINDIR..."
    unzip -q "$TEMP_DIR/oaica-darwin.zip" -d "$TEMP_DIR"
    if [ ! -f "$TEMP_DIR/bin/oaica" ] || [ -L "$TEMP_DIR/bin/oaica" ]; then error "the archive did not contain a regular bin/oaica"; fi
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
# (fetch_archive) before anything is extracted, and it is unpacked into
# unpack_dir — a directory under TEMP_DIR, owned by the user running the
# script. The caller then installs bin/oaica from there.
#
# Nothing is extracted as root any more. The Linux branch used to pass the
# install PREFIX as the destination and unpack with `$SUDO tar -xf -C`, so the
# archive was written straight into /usr/local as root — which is not what
# docs/ENTERPRISE.md tells a reviewer this installer does ("extract bin/oaica
# into a temporary directory, then install it as /usr/local/bin/oaica (mode
# 755)"), and which leaves a half-written tree in the prefix when a download
# is truncated mid-stream (2026-09-26 audit).
download_and_extract() {
    local url_base="$1"
    local unpack_dir="$2"
    local filename="$3"

    rm -rf "$unpack_dir"
    mkdir -p "$unpack_dir"

    # Check if .tar.zst is available
    if curl --fail --silent --head --location $(redirect_guard "$url_base") "${url_base}/${filename}.tar.zst" >/dev/null 2>&1; then
        # zst file exists - check if we have zstd tool
        if ! available zstd; then
            error "This version requires zstd for extraction. Please install zstd and try again:
  - Debian/Ubuntu: sudo apt-get install zstd
  - RHEL/CentOS/Fedora: sudo dnf install zstd
  - Arch: sudo pacman -S zstd"
        fi

        status "Downloading ${filename}.tar.zst"
        fetch_archive "$url_base" "${filename}.tar.zst" "$TEMP_DIR/${filename}.tar.zst"
        zstd -dc "$TEMP_DIR/${filename}.tar.zst" | tar -xf - -C "${unpack_dir}"
        return 0
    fi

    # Fall back to .tgz for older versions
    status "Downloading ${filename}.tgz"
    fetch_archive "$url_base" "${filename}.tgz" "$TEMP_DIR/${filename}.tgz"
    tar -xzf "$TEMP_DIR/${filename}.tgz" -C "${unpack_dir}"
}

for BINDIR in /usr/local/bin /usr/bin /bin; do
    echo $PATH | grep -q $BINDIR && break || continue
done
OAICA_INSTALL_DIR=$(dirname ${BINDIR})

if [ -d "$OAICA_INSTALL_DIR/lib/oaica" ] ; then
    status "Cleaning up old version at $OAICA_INSTALL_DIR/lib/oaica"
    $SUDO rm -rf "$OAICA_INSTALL_DIR/lib/oaica"
fi
status "Installing oaica to $BINDIR/oaica"
$SUDO install -o0 -g0 -m755 -d $BINDIR
# No $OAICA_INSTALL_DIR/lib/oaica: upstream Ollama unpacked a server there,
# this fork ships one binary in bin/. The cleanup directly above removes the
# directory older versions of this installer left behind (2026-09-26 audit).
UNPACK_DIR="$TEMP_DIR/oaica-unpack"
download_and_extract "$DOWNLOAD_BASE" "$UNPACK_DIR" "oaica-linux-${ARCH}"
if [ ! -f "$UNPACK_DIR/bin/oaica" ] || [ -L "$UNPACK_DIR/bin/oaica" ]; then
    # A symlink would be copied as root with the target's content (a 600-mode file made world-readable).
    error "the archive did not contain a regular bin/oaica"
fi
$SUDO install -o0 -g0 -m755 "$UNPACK_DIR/bin/oaica" "$BINDIR/oaica"


# OAICA is a thin CLI (talks to api.oaica.com — OAICA_FORK_PLAN.md
# option 2). No local inference server ever runs here, so — unlike
# upstream Ollama, which this installer was forked from — there is
# nothing to systemd-service-ify and no local GPU driver to install.
# The only prerequisite is a working network path to api.oaica.com.
status "Install complete. Run 'oaica' from the command line."
status "Set OAICA_API_KEY before your first run: export OAICA_API_KEY=<your-key>"

}

main
