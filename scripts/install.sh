#!/usr/bin/env bash
set -euo pipefail

# Holo-VTL Unified Installer
# Supports both online (github) and offline (local tarball) installation.

GITHUB_REPO="Holo-VTL/Holo"
INSTALLER_NAME="install.sh"
PACKAGE_PREFIX="holo-vtl"

log() {
    printf '[holo-install] %s\n' "$*"
}

die() {
    printf '[holo-install][error] %s\n' "$*" >&2
    exit 1
}

run_installer() {
    local installer="$1"
    shift
    if [[ "${EUID}" -eq 0 ]]; then
        bash "$installer" "$@"
    else
        sudo bash "$installer" "$@"
    fi
}

# 1. Detect if we are running from an extracted release package
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -f "${SCRIPT_DIR}/control-plane" && -f "${SCRIPT_DIR}/holo-tcmu-handler" && -f "${SCRIPT_DIR}/install-holo.sh" ]]; then
    log "Running from extracted release package. Performing local installation..."
    run_installer "${SCRIPT_DIR}/install-holo.sh" "$@"
    exit $?
fi

# 2. Check for --offline flag
OFFLINE=0
args=()
for arg in "$@"; do
    if [[ "$arg" == "--offline" ]]; then
        OFFLINE=1
    else
        args+=("$arg")
    fi
done

if [[ "$OFFLINE" == "1" ]]; then
    # Look for a tarball in the current directory
    tarball=$(ls -t ${PACKAGE_PREFIX}-*.tar.gz 2>/dev/null | head -n 1 || true)
    if [[ -z "$tarball" ]]; then
        die "Offline mode requested but no ${PACKAGE_PREFIX}-*.tar.gz found in current directory."
    fi
    log "Found local tarball: ${tarball}. Extracting..."
    tmp_dir=$(mktemp -d)
    trap 'rm -rf "${tmp_dir}"' EXIT
    tar -xzf "$tarball" -C "$tmp_dir"
    pkg_dir=$(ls -d "${tmp_dir}/${PACKAGE_PREFIX}-"* | head -n 1)
    log "Performing installation from ${tarball}..."
    run_installer "${pkg_dir}/install-holo.sh" "${args[@]}"
    exit $?
fi

# 3. Online installation
log "Performing online installation from GitHub..."
if ! command -v curl >/dev/null 2>&1; then
    die "curl is required for online installation."
fi

# Get latest version
log "Fetching latest release information from GitHub..."
LATEST_RELEASE_URL="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
VERSION_JSON=$(curl -fsSL "$LATEST_RELEASE_URL")
VERSION_TAG=$(echo "$VERSION_JSON" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')

if [[ -z "$VERSION_TAG" ]]; then
    die "Could not detect latest version from GitHub."
fi
log "Latest version detected: ${VERSION_TAG}"

# Find the linux-x86_64 tarball asset
DOWNLOAD_URL=$(echo "$VERSION_JSON" | grep '"browser_download_url":' | grep 'linux-x86_64.tar.gz' | head -n 1 | sed -E 's/.*"([^"]+)".*/\1/')

if [[ -z "$DOWNLOAD_URL" ]]; then
    die "Could not find a suitable linux-x86_64 tarball in the latest release."
fi

log "Downloading ${DOWNLOAD_URL}..."
tmp_dir=$(mktemp -d)
trap 'rm -rf "${tmp_dir}"' EXIT
curl -fsSL "$DOWNLOAD_URL" -o "${tmp_dir}/holo.tar.gz"

log "Extracting..."
tar -xzf "${tmp_dir}/holo.tar.gz" -C "$tmp_dir"
pkg_dir=$(ls -d "${tmp_dir}/${PACKAGE_PREFIX}-"* | head -n 1)

log "Starting installer..."
run_installer "${pkg_dir}/install-holo.sh" "${args[@]}"
