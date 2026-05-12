#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ACTION="install"
DRY_RUN=0
WITH_VALIDATION_TOOLS=0
BUILD_TCMU_PLUGIN=0
PURGE_DATA=0
BUNDLE_DIR="${SCRIPT_DIR}"
DEPS_DIR="${HOLO_INSTALL_DEPS_DIR:-}"
PREFIX="/opt/holo"
CONFIG_DIR="/etc/holo"
DATA_DIR="/var/lib/holo"
LOG_DIR="/var/log/holo"
SERVICE_USER="holo"
SERVICE_GROUP="holo"
API_KEY="${HOLO_INSTALL_API_KEY:-}"
API_KEY_FILE=""
CONTROL_PLANE_PORT="${HOLO_HTTP_PORT:-80}"
CONTROL_PLANE_BIND_ADDR="${HOLO_HTTP_BIND_ADDR:-}"
PORTAL_HOST="${HOLO_TARGET_PORTAL_HOST:-}"
PORTAL_PORT="${HOLO_TARGET_PORTAL_PORT:-3260}"
CONTROL_PLANE_PATH=""
TCMU_HANDLER_PATH=""
WEB_DIST_PATH=""
HANDLER_SO_PATH=""
PLUGIN_SOURCE_DIR="${TCMU_SOURCE_DIR:-}"

OS_ID=""
OS_VERSION_ID=""
OS_MAJOR=""
ARCH=""
PKG_MANAGER=""
PLUGIN_DIR=""
RUNTIME_PACKAGES=()
VALIDATION_PACKAGES=()
BUILD_PACKAGES=()
REPO_ACTIONS=()
WARNINGS=()

usage() {
  cat <<'USAGE'
Usage: install-holo-universal.sh [install|upgrade|uninstall] [options]

Installs, upgrades, or uninstalls Holo-VTL from local release artifacts.

Supported Platforms:
  RHEL / Rocky / AlmaLinux / CentOS Stream  8, 9, 10
  Debian                                     11 (Bullseye), 12 (Bookworm), 13 (Trixie)
  Ubuntu                                     20.04 LTS, 22.04 LTS, 24.04 LTS, 25.04
  SLES                                       12 SP5+, 15 SP3+
  openSUSE Leap                              15.6 (SLES 15 compatibility proxy)

Commands:
  install                   Install Holo-VTL (default)
  upgrade                   Upgrade binaries/config, preserve data
  uninstall                 Remove services and files; preserve data by default

Options:
  --dry-run                 Print planned actions without changes
  --bundle-dir PATH         Release artifacts directory (default: script directory)
  --deps-dir PATH           Bundled tcmu-runner RPM/DEB packages directory
  --prefix PATH             Install prefix (default: /opt/holo)
  --config-dir PATH         Config directory (default: /etc/holo)
  --data-dir PATH           Data directory (default: /var/lib/holo)
  --log-dir PATH            Log directory (default: /var/log/holo)
  --user NAME               Service user/group (default: holo)
  --api-key VALUE           API key; omitted = no-login mode
  --api-key-file PATH       Read API key from a local file to avoid exposing it in process args
  --portal-host HOST        iSCSI portal host (default: auto-detect)
  --portal-port PORT        iSCSI portal port (default: 3260)
  --with-validation-tools   Install jq/lsscsi/sg3-utils etc.
  --build-tcmu-plugin       Build handler_holo.so on host
  --plugin-source-dir PATH  tcmu-runner source for --build-tcmu-plugin
  --control-plane PATH      Override control-plane binary path
  --tcmu-handler PATH       Override holo-tcmu-handler binary path
  --web-dist PATH           Override web-console dist directory
  --handler-so PATH         Override prebuilt handler_holo.so path
  --purge-data              With uninstall, also remove data
  --help                    Show this help

Required bundle layout (beside this script):
  ./control-plane
  ./holo-tcmu-handler
  ./web-console/dist/index.html
  ./handler_holo.so          (unless --build-tcmu-plugin)
USAGE
}

log()   { printf '[holo-install] %s\n' "$*"; }
warn()  { WARNINGS+=("$*"); printf '[holo-install][warn] %s\n' "$*" >&2; }
die()   { printf '[holo-install][error] %s\n' "$*" >&2; exit 1; }

die_usage() {
  printf '[holo-install][error] %s\n\n' "$*" >&2
  usage >&2
  exit 2
}

run_cmd() {
  if [[ "${DRY_RUN}" == "1" ]]; then
    printf '[dry-run]'; printf ' %q' "$@"; printf '\n'; return 0
  fi
  "$@"
}

run_zypper() {
  if [[ "${DRY_RUN}" == "1" ]]; then
    run_cmd zypper "$@"
    return 0
  fi

  local rc
  set +e
  zypper "$@"
  rc=$?
  set -e

  case "${rc}" in
    0)
      return 0
      ;;
    100|101|102|103|106)
      warn "zypper completed with informational exit code ${rc}; continuing"
      return 0
      ;;
    *)
      return "${rc}"
      ;;
  esac
}

run_shell() {
  local script="$1"
  if [[ "${DRY_RUN}" == "1" ]]; then
    printf '[dry-run] bash -c %q\n' "${script}"; return 0
  fi
  bash -c "${script}"
}

restore_selinux_contexts() {
  if [[ "${DRY_RUN}" == "1" ]]; then
    run_cmd restorecon -Rv "$@"
    return 0
  fi
  command -v restorecon >/dev/null 2>&1 || return 0
  restorecon -Rv "$@" || warn "SELinux restorecon failed for: $*"
}

require_root_for_system_change() {
  if [[ "${DRY_RUN}" == "1" ]]; then return 0; fi
  if [[ "${EUID}" -ne 0 ]]; then
    die "system changes must run as root; rerun with sudo or use --dry-run"
  fi
}

# ── Argument parsing ────────────────────────────────────────────────

parse_args() {
  if [[ $# -gt 0 ]]; then
    case "$1" in
      install|upgrade|uninstall) ACTION="$1"; shift ;;
    esac
  fi

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --dry-run)               DRY_RUN=1; shift ;;
      --bundle-dir)            [[ $# -ge 2 ]] || die_usage "--bundle-dir requires a path"
                               BUNDLE_DIR="$2"; shift 2 ;;
      --deps-dir)              [[ $# -ge 2 ]] || die_usage "--deps-dir requires a path"
                               DEPS_DIR="$2"; shift 2 ;;
      --prefix)                [[ $# -ge 2 ]] || die_usage "--prefix requires a path"
                               PREFIX="$2"; shift 2 ;;
      --config-dir)            [[ $# -ge 2 ]] || die_usage "--config-dir requires a path"
                               CONFIG_DIR="$2"; shift 2 ;;
      --data-dir)              [[ $# -ge 2 ]] || die_usage "--data-dir requires a path"
                               DATA_DIR="$2"; shift 2 ;;
      --log-dir)               [[ $# -ge 2 ]] || die_usage "--log-dir requires a path"
                               LOG_DIR="$2"; shift 2 ;;
      --user)                  [[ $# -ge 2 ]] || die_usage "--user requires a name"
                               SERVICE_USER="$2"; SERVICE_GROUP="$2"; shift 2 ;;
      --api-key)               [[ $# -ge 2 ]] || die_usage "--api-key requires a value"
                               [[ -z "${API_KEY_FILE}" ]] || die_usage "--api-key and --api-key-file are mutually exclusive"
                               validate_api_key_value "--api-key" "$2"
                               API_KEY="$2"; shift 2 ;;
      --api-key-file)          [[ $# -ge 2 ]] || die_usage "--api-key-file requires a path"
                               [[ -z "${API_KEY}" ]] || die_usage "--api-key and --api-key-file are mutually exclusive"
                               API_KEY_FILE="$2"; shift 2 ;;
      --portal-host)           [[ $# -ge 2 ]] || die_usage "--portal-host requires a host"
                               PORTAL_HOST="$2"; shift 2 ;;
      --portal-port)           [[ $# -ge 2 ]] || die_usage "--portal-port requires a port"
                               PORTAL_PORT="$2"; shift 2 ;;
      --with-validation-tools) WITH_VALIDATION_TOOLS=1; shift ;;
      --build-tcmu-plugin)     BUILD_TCMU_PLUGIN=1; shift ;;
      --plugin-source-dir)     [[ $# -ge 2 ]] || die_usage "--plugin-source-dir requires a path"
                               PLUGIN_SOURCE_DIR="$2"; shift 2 ;;
      --control-plane)         [[ $# -ge 2 ]] || die_usage "--control-plane requires a path"
                               CONTROL_PLANE_PATH="$2"; shift 2 ;;
      --tcmu-handler)          [[ $# -ge 2 ]] || die_usage "--tcmu-handler requires a path"
                               TCMU_HANDLER_PATH="$2"; shift 2 ;;
      --web-dist)              [[ $# -ge 2 ]] || die_usage "--web-dist requires a path"
                               WEB_DIST_PATH="$2"; shift 2 ;;
      --handler-so)            [[ $# -ge 2 ]] || die_usage "--handler-so requires a path"
                               HANDLER_SO_PATH="$2"; shift 2 ;;
      --purge-data)            PURGE_DATA=1; shift ;;
      --help)                  usage; exit 0 ;;
      install|upgrade|uninstall) die_usage "command must appear before options: $1" ;;
      *)                       die_usage "unknown option: $1" ;;
    esac
  done
}

# ── Input canonicalization ──────────────────────────────────────────

canonicalize_inputs() {
  BUNDLE_DIR="$(cd "${BUNDLE_DIR}" && pwd)"
  if [[ -z "${DEPS_DIR}" && -d "${BUNDLE_DIR}/packages" ]]; then
    DEPS_DIR="${BUNDLE_DIR}/packages"
  fi
  if [[ -n "${DEPS_DIR}" ]]; then
    DEPS_DIR="$(cd "${DEPS_DIR}" && pwd)"
  fi
  CONTROL_PLANE_PATH="${CONTROL_PLANE_PATH:-${BUNDLE_DIR}/control-plane}"
  TCMU_HANDLER_PATH="${TCMU_HANDLER_PATH:-${BUNDLE_DIR}/holo-tcmu-handler}"
  WEB_DIST_PATH="${WEB_DIST_PATH:-${BUNDLE_DIR}/web-console/dist}"
  HANDLER_SO_PATH="${HANDLER_SO_PATH:-${BUNDLE_DIR}/handler_holo.so}"
}

validate_action_options() {
  if [[ "${PURGE_DATA}" == "1" && "${ACTION}" != "uninstall" ]]; then
    die_usage "--purge-data is only valid with the uninstall command"
  fi
}

validate_api_key_value() {
  local label="$1"
  local value="$2"
  if [[ -z "${value}" ]]; then
    die_usage "${label} must not be empty"
  fi
  if [[ "${value}" == *$'\n'* || "${value}" == *$'\r'* ]]; then
    die_usage "${label} must not contain newlines"
  fi
}

validate_port_value() {
  local label="$1"
  local value="$2"
  if [[ ! "${value}" =~ ^[0-9]+$ ]]; then
    die_usage "${label} must be a numeric TCP port"
  fi
  if (( value < 1 || value > 65535 )); then
    die_usage "${label} must be between 1 and 65535"
  fi
}

validate_bind_address_value() {
  local label="$1"
  local value="$2"
  if [[ -z "${value}" ]]; then
    return 0
  fi
  if [[ "${value}" == *:* && ! "${value}" =~ ^\[[0-9A-Fa-f:.]+\]$ ]]; then
    die_usage "${label} IPv6 values must be bracketed"
  fi
  if [[ ! "${value}" =~ ^[-A-Za-z0-9._]+$ && ! "${value}" =~ ^\[[0-9A-Fa-f:.]+\]$ ]]; then
    die_usage "${label} contains unsupported characters"
  fi
}

validate_absolute_path_value() {
  local label="$1"
  local value="$2"
  if [[ "${value}" != /* ]]; then
    die_usage "${label} must be an absolute path"
  fi
  case "${value}" in
    /|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/opt|/proc|/root|/run|/sbin|/sys|/tmp|/usr|/var|/var/lib|/var/log)
      die_usage "${label} points at a system root path"
      ;;
    /dev/*|/home/*|/proc/*|/root/*|/run/*|/sys/*|/tmp/*)
      die_usage "${label} points under a runtime or protected system path"
      ;;
  esac
  if [[ "${value}" == *..* ]]; then
    die_usage "${label} must not contain '..'"
  fi
  if [[ ! "${value}" =~ ^/[-A-Za-z0-9._/+@=]*$ ]]; then
    die_usage "${label} contains unsupported characters"
  fi
}

validate_safe_inputs() {
  validate_port_value "HOLO_HTTP_PORT" "${CONTROL_PLANE_PORT}"
  validate_bind_address_value "HOLO_HTTP_BIND_ADDR" "${CONTROL_PLANE_BIND_ADDR}"
  validate_port_value "--portal-port" "${PORTAL_PORT}"
  validate_absolute_path_value "--prefix" "${PREFIX}"
  validate_absolute_path_value "--config-dir" "${CONFIG_DIR}"
  validate_absolute_path_value "--data-dir" "${DATA_DIR}"
  validate_absolute_path_value "--log-dir" "${LOG_DIR}"
}

reject_unsafe_storage_flow() {
  if [[ "${HOLO_STRICT_STORAGE_FLOW:-}" == "0" ]]; then
    die "HOLO_STRICT_STORAGE_FLOW=0 is forbidden for product installs"
  fi
}

validate_artifacts() {
  local missing=()
  [[ -f "${CONTROL_PLANE_PATH}" ]] || missing+=("${CONTROL_PLANE_PATH}")
  [[ -f "${TCMU_HANDLER_PATH}" ]] || missing+=("${TCMU_HANDLER_PATH}")
  [[ -d "${WEB_DIST_PATH}" && -f "${WEB_DIST_PATH}/index.html" ]] || missing+=("${WEB_DIST_PATH}/index.html")
  if [[ "${BUILD_TCMU_PLUGIN}" != "1" && ! -f "${HANDLER_SO_PATH}" ]]; then
    missing+=("${HANDLER_SO_PATH}")
  fi
  if [[ "${BUILD_TCMU_PLUGIN}" == "1" ]]; then
    [[ -f "${BUNDLE_DIR}/handler_holo.c" || -f "${SCRIPT_DIR}/../infra/tcmu/handler_holo.c" ]] || missing+=("handler_holo.c")
  fi
  if [[ "${#missing[@]}" -gt 0 ]]; then
    printf '[holo-install][error] missing required release artifacts:\n' >&2
    printf '  - %s\n' "${missing[@]}" >&2
    exit 1
  fi
}

# ── tcmu-runner source resolution ───────────────────────────────────

resolve_plugin_source_dir() {
  [[ "${BUILD_TCMU_PLUGIN}" == "1" ]] || return 0

  if [[ -n "${PLUGIN_SOURCE_DIR}" && -f "${PLUGIN_SOURCE_DIR}/tcmu-runner.h" ]]; then
    log "Using provided plugin source dir: ${PLUGIN_SOURCE_DIR}"
    return 0
  fi

  local candidate
  for candidate in /usr/include /usr/include/tcmu-runner /usr/local/include; do
    if [[ -f "${candidate}/tcmu-runner.h" ]]; then
      PLUGIN_SOURCE_DIR="${candidate}"
      log "Auto-detected tcmu-runner.h at ${candidate}"
      return 0
    fi
  done

  local tcmu_src
  tcmu_src="$(mktemp -d /tmp/holo-tcmu-runner-src.XXXXXX)"
  log "tcmu-runner.h not found locally; cloning tcmu-runner source"
  if ! command -v git >/dev/null 2>&1; then
    case "${PKG_MANAGER}" in
      apt)    run_cmd apt-get install -y git ;;
      dnf)    run_cmd dnf install -y git ;;
      zypper) run_zypper -n install git ;;
    esac
  fi
  run_cmd git clone --depth 1 https://github.com/open-iscsi/tcmu-runner.git "${tcmu_src}"
  if [[ -f "${tcmu_src}/tcmu-runner.h" ]]; then
    PLUGIN_SOURCE_DIR="${tcmu_src}"
    log "Using cloned tcmu-runner source: ${tcmu_src}"
    return 0
  fi

  die "Could not locate or obtain tcmu-runner.h. Install tcmu-runner-devel or pass --plugin-source-dir"
}

# ── Platform detection ──────────────────────────────────────────────

read_os_release_value() {
  local key="$1"
  local file="${HOLO_INSTALL_OS_RELEASE:-/etc/os-release}"
  local line val
  [[ -r "${file}" ]] || return 0
  while IFS= read -r line; do
    case "${line}" in
      "${key}="*)
        val="${line#*=}"
        val="${val%\"}"
        val="${val#\"}"
        printf '%s\n' "${val}"
        return 0
        ;;
    esac
  done <"${file}"
}

exit_unsupported() {
  printf '[holo-install][error] %s\n' "$*" >&2
  exit 2
}

detect_platform() {
  OS_ID="$(read_os_release_value ID)"
  OS_VERSION_ID="$(read_os_release_value VERSION_ID)"
  OS_MAJOR="${OS_VERSION_ID%%.*}"
  ARCH="${HOLO_INSTALL_UNAME_M:-$(uname -m)}"

  case "${ARCH}" in
    x86_64|amd64) ;;
    *) exit_unsupported "unsupported architecture ${ARCH}; supported: x86_64" ;;
  esac

  # Normalize OS_ID
  case "${OS_ID}" in
    rhel|rocky|almalinux|centos|centos-stream) ;;
    ubuntu|debian) ;;
    opensuse-leap|sles|sles_sap) ;;
    *) ;;
  esac

  case "${OS_ID}:${OS_MAJOR}" in
    ubuntu:20|ubuntu:22|ubuntu:24|ubuntu:25)
      PKG_MANAGER="apt"
      PLUGIN_DIR="/usr/lib/x86_64-linux-gnu/tcmu-runner"
      ;;
    debian:11|debian:12|debian:13)
      PKG_MANAGER="apt"
      PLUGIN_DIR="/usr/lib/x86_64-linux-gnu/tcmu-runner"
      ;;
    rocky:8|rocky:9|rocky:10|rhel:8|rhel:9|rhel:10|almalinux:8|almalinux:9|almalinux:10|centos:8|centos:9|centos:10|centos-stream:8|centos-stream:9)
      PKG_MANAGER="dnf"
      PLUGIN_DIR="/usr/lib64/tcmu-runner"
      ;;
    opensuse-leap:15|sles:12|sles:15|sles_sap:12|sles_sap:15)
      PKG_MANAGER="zypper"
      PLUGIN_DIR="/usr/lib64/tcmu-runner"
      ;;
    *)
      exit_unsupported "unsupported platform ${OS_ID:-unknown} ${OS_VERSION_ID:-unknown}; supported: Ubuntu 20.04/22.04/24.04/25.04, Debian 11/12/13, RHEL/Rocky/Alma 8/9/10, SLES 12 SP5+/15 SP3+, openSUSE Leap 15.6"
      ;;
  esac
  log "Detected platform: ${OS_ID} ${OS_VERSION_ID} ${ARCH} (${PKG_MANAGER})"
}

# ── Package planning ────────────────────────────────────────────────

bundled_dependency_search_dirs() {
  [[ -n "${DEPS_DIR}" && -d "${DEPS_DIR}" ]] || return 0

  local candidates=("${DEPS_DIR}")
  if [[ -n "${PKG_MANAGER}" ]]; then
    candidates+=("${DEPS_DIR}/${PKG_MANAGER}")
  fi
  case "${PKG_MANAGER}" in
    dnf|zypper)
      candidates+=("${DEPS_DIR}/el${OS_MAJOR}" "${DEPS_DIR}/${PKG_MANAGER}/el${OS_MAJOR}")
      ;;
    apt)
      candidates+=("${DEPS_DIR}/${OS_ID}" "${DEPS_DIR}/${PKG_MANAGER}/${OS_ID}-${OS_MAJOR}")
      ;;
  esac

  local dir seen=" "
  for dir in "${candidates[@]}"; do
    [[ -d "${dir}" ]] || continue
    case "${seen}" in
      *" ${dir} "*) continue ;;
    esac
    seen="${seen}${dir} "
    printf '%s\n' "${dir}"
  done
}

bundled_dependency_packages() {
  [[ -n "${DEPS_DIR}" && -d "${DEPS_DIR}" ]] || return 0

  local dir pkg
  while IFS= read -r dir; do
    case "${PKG_MANAGER}" in
      apt)
        for pkg in "${dir}"/libtcmu*.deb "${dir}"/tcmu-runner*.deb; do
          [[ -f "${pkg}" ]] && printf '%s\n' "${pkg}"
        done
        ;;
      dnf|zypper)
        for pkg in "${dir}"/libtcmu*.rpm "${dir}"/tcmu-runner*.rpm; do
          [[ -f "${pkg}" ]] && printf '%s\n' "${pkg}"
        done
        ;;
    esac
  done < <(bundled_dependency_search_dirs) | sort -u
}

has_bundled_tcmu_packages() {
  [[ -n "${DEPS_DIR}" && -d "${DEPS_DIR}" ]] || return 1

  local dir pkg
  while IFS= read -r dir; do
    case "${PKG_MANAGER}" in
      apt)
        for pkg in "${dir}"/tcmu-runner*.deb; do
          [[ -f "${pkg}" ]] && return 0
        done
        ;;
      dnf|zypper)
        for pkg in "${dir}"/tcmu-runner*.rpm; do
          [[ -f "${pkg}" ]] && return 0
        done
        ;;
    esac
  done < <(bundled_dependency_search_dirs)
  return 1
}

build_package_plan() {
  case "${PKG_MANAGER}" in
    apt)
      REPO_ACTIONS=("apt-get update")
      RUNTIME_PACKAGES=(kmod sudo targetcli-fb tcmu-runner xfsprogs)
      VALIDATION_PACKAGES=(curl jq lsscsi sg3-utils open-iscsi)
      BUILD_PACKAGES=(gcc make pkg-config dpkg-dev libtcmu-dev)
      ;;
    dnf)
      if has_bundled_tcmu_packages; then
        case "${OS_ID}:${OS_MAJOR}" in
          rhel:8)
            REPO_ACTIONS=(
              "timeout 30s subscription-manager repos --enable rhel-8-for-\$(uname -m)-baseos-rpms --enable rhel-8-for-\$(uname -m)-appstream-rpms --enable codeready-builder-for-rhel-8-\$(uname -m)-rpms || true"
            )
            ;;
          rhel:9)
            REPO_ACTIONS=(
              "timeout 30s subscription-manager repos --enable rhel-9-for-\$(uname -m)-baseos-rpms --enable rhel-9-for-\$(uname -m)-appstream-rpms --enable codeready-builder-for-rhel-9-\$(uname -m)-rpms || true"
            )
            ;;
          rhel:10)
            REPO_ACTIONS=(
              "timeout 30s subscription-manager repos --enable rhel-10-for-\$(uname -m)-baseos-rpms --enable rhel-10-for-\$(uname -m)-appstream-rpms --enable codeready-builder-for-rhel-10-\$(uname -m)-rpms || true"
            )
            ;;
          *)
            REPO_ACTIONS=()
            ;;
        esac
        RUNTIME_PACKAGES=(kmod sudo targetcli xfsprogs)
      else
        case "${OS_ID}:${OS_MAJOR}" in
          rhel:8)
            REPO_ACTIONS=(
              "timeout 30s subscription-manager repos --enable rhel-8-for-\$(uname -m)-baseos-rpms --enable rhel-8-for-\$(uname -m)-appstream-rpms --enable codeready-builder-for-rhel-8-\$(uname -m)-rpms || true"
              "dnf install -y epel-release || dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-8.noarch.rpm"
              "dnf makecache"
            )
            ;;
          rhel:9)
            REPO_ACTIONS=(
              "timeout 30s subscription-manager repos --enable rhel-9-for-\$(uname -m)-baseos-rpms --enable rhel-9-for-\$(uname -m)-appstream-rpms --enable codeready-builder-for-rhel-9-\$(uname -m)-rpms || true"
              "dnf install -y epel-release || dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-9.noarch.rpm"
              "dnf makecache"
            )
            ;;
          rhel:10)
            REPO_ACTIONS=(
              "timeout 30s subscription-manager repos --enable rhel-10-for-\$(uname -m)-baseos-rpms --enable rhel-10-for-\$(uname -m)-appstream-rpms --enable codeready-builder-for-rhel-10-\$(uname -m)-rpms || true"
              "dnf install -y epel-release || dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-10.noarch.rpm"
              "dnf makecache"
            )
            ;;
          *:8)
            REPO_ACTIONS=(
              "dnf install -y dnf-plugins-core epel-release"
              "dnf config-manager --set-enabled powertools || true"
              "dnf makecache"
            )
            ;;
          *:9)
            REPO_ACTIONS=(
              "dnf install -y dnf-plugins-core epel-release"
              "dnf config-manager --set-enabled crb || crb enable || true"
              "dnf makecache"
            )
            ;;
          *)
            REPO_ACTIONS=(
              "dnf install -y dnf-plugins-core epel-release"
              "dnf makecache"
            )
            ;;
        esac
        RUNTIME_PACKAGES=(kmod sudo targetcli tcmu-runner xfsprogs)
      fi
      if [[ "${OS_MAJOR}" == "10" ]]; then
        RUNTIME_PACKAGES=(kmod sudo "kernel-modules-$(uname -r)" "${RUNTIME_PACKAGES[@]:2}")
      fi
      VALIDATION_PACKAGES=(curl jq lsscsi sg3_utils iscsi-initiator-utils)
      BUILD_PACKAGES=(gcc make pkgconfig rpm-build rpmdevtools tcmu-runner-devel)
      ;;
    zypper)
      REPO_ACTIONS=()
      RUNTIME_PACKAGES=(kernel-default kmod sudo xfsprogs util-linux-systemd python3-targetcli-fb tcmu-runner)
      VALIDATION_PACKAGES=(curl jq lsscsi sg3_utils open-iscsi)
      BUILD_PACKAGES=(gcc make pkg-config)
      ;;
    *)
      die "internal error: package manager not detected"
      ;;
  esac
}

# ── Preflight ───────────────────────────────────────────────────────

preflight_host() {
  log "Checking host prerequisites"
  if [[ "${DRY_RUN}" == "1" ]]; then
    log "Dry-run preflight: systemd and kernel module checks deferred"
    return 0
  fi
  command -v systemctl >/dev/null 2>&1 || die "systemd is required but systemctl was not found"
  [[ -d /run/systemd/system ]] || die "systemd is required but this host is not booted with systemd"
}

print_plan() {
  log "Action: ${ACTION}"
  log "Install bundle: ${BUNDLE_DIR}"
  if [[ -n "${DEPS_DIR}" ]]; then
    log "Bundled dependency dir: ${DEPS_DIR}"
  else
    log "Bundled dependency dir: none"
  fi
  log "Install prefix: ${PREFIX}"
  log "Config dir: ${CONFIG_DIR}"
  log "Data dir: ${DATA_DIR}"
  log "Service user: ${SERVICE_USER}"
  log "TCMU plugin dir: ${PLUGIN_DIR}"
  log "Runtime packages: ${RUNTIME_PACKAGES[*]}"
  if [[ "${WITH_VALIDATION_TOOLS}" == "1" ]]; then
    log "Validation packages: ${VALIDATION_PACKAGES[*]}"
  else
    log "Validation packages: skipped"
  fi
  if [[ "${BUILD_TCMU_PLUGIN}" == "1" ]]; then
    log "Build packages: ${BUILD_PACKAGES[*]}"
  else
    log "Build packages: skipped"
  fi
  log "Runtime invariant: HOLO_TARGET_RUNTIME_MODE=tcmu"
  log "Runtime invariant: HOLO_STRICT_STORAGE_FLOW=1"
}

print_uninstall_plan() {
  log "Action: uninstall"
  log "Install prefix: ${PREFIX}"
  log "Config dir: ${CONFIG_DIR}"
  log "Data dir: ${DATA_DIR}"
  log "Log dir: ${LOG_DIR}"
  log "Service user: ${SERVICE_USER}"
  if [[ "${PURGE_DATA}" == "1" ]]; then
    log "Data policy: purge config, data, and logs"
  else
    log "Data policy: preserve ${CONFIG_DIR}, ${DATA_DIR}, and ${LOG_DIR}"
  fi
}

# ── Package installation ────────────────────────────────────────────

package_installed() {
  local pkg="$1"
  case "${PKG_MANAGER}" in
    dnf) rpm -q "${pkg}" >/dev/null 2>&1 ;;
    *) return 1 ;;
  esac
}

package_satisfied_by_bundle() {
  local pkg="$1"
  case "${PKG_MANAGER}:${pkg}" in
    dnf:tcmu-runner) has_bundled_tcmu_packages ;;
    *) return 1 ;;
  esac
}

dnf_has_enabled_repositories() {
  local out
  if ! out="$(dnf -q repolist --enabled 2>/dev/null)"; then
    return 1
  fi
  awk '
    $1 == "repo" || $1 == "repoid" { next }
    $1 ~ /^[A-Za-z0-9_.:-]+$/ && NF >= 2 { found = 1 }
    END { exit found ? 0 : 1 }
  ' <<<"${out}"
}

validate_dnf_package_sources() {
  [[ "${PKG_MANAGER}" == "dnf" ]] || return 0
  [[ "${DRY_RUN}" == "1" ]] && return 0

  local pkg missing=()
  for pkg in "$@"; do
    if package_installed "${pkg}" || package_satisfied_by_bundle "${pkg}"; then
      continue
    fi
    missing+=("${pkg}")
  done

  [[ "${#missing[@]}" -eq 0 ]] && return 0
  dnf_has_enabled_repositories && return 0

  if [[ "${OS_ID}" == "rhel" ]]; then
    die "missing required runtime packages and no enabled RHEL DNF repository is available: ${missing[*]}; register/attach this host and enable RHEL BaseOS/AppStream/CodeReady Builder repositories, then rerun install"
  fi
  die "missing required runtime packages and no enabled DNF repository is available: ${missing[*]}; enable the OS repositories that provide these packages, then rerun install"
}

install_packages() {
  log "Installing package dependencies"

  local bundled_deps=()
  if [[ -n "${DEPS_DIR}" && -d "${DEPS_DIR}" ]]; then
    while IFS= read -r dep; do
      bundled_deps+=("${dep}")
    done < <(bundled_dependency_packages)
  fi

  local action
  if [[ "${#REPO_ACTIONS[@]}" -gt 0 ]]; then
    for action in "${REPO_ACTIONS[@]}"; do
      run_shell "${action}"
    done
  fi

  validate_dnf_package_sources "${RUNTIME_PACKAGES[@]}"

  case "${PKG_MANAGER}" in
    apt)
      if [[ "${#bundled_deps[@]}" -gt 0 ]]; then
        run_cmd apt-get install -y "${bundled_deps[@]}"
      fi
      run_cmd apt-get install -y "${RUNTIME_PACKAGES[@]}"
      if [[ "${WITH_VALIDATION_TOOLS}" == "1" ]]; then
        run_cmd apt-get install -y "${VALIDATION_PACKAGES[@]}"
      fi
      if [[ "${BUILD_TCMU_PLUGIN}" == "1" ]]; then
        run_cmd apt-get install -y "${BUILD_PACKAGES[@]}"
      fi
      ;;
    dnf)
      if [[ "${#bundled_deps[@]}" -gt 0 ]]; then
        run_cmd dnf install -y "${bundled_deps[@]}"
      fi
      run_cmd dnf install -y "${RUNTIME_PACKAGES[@]}"
      if [[ "${WITH_VALIDATION_TOOLS}" == "1" ]]; then
        run_cmd dnf install -y "${VALIDATION_PACKAGES[@]}"
      fi
      if [[ "${BUILD_TCMU_PLUGIN}" == "1" ]]; then
        run_cmd dnf install -y "${BUILD_PACKAGES[@]}"
      fi
      ;;
    zypper)
      if [[ "${#bundled_deps[@]}" -gt 0 ]]; then
        run_zypper -n install --allow-unsigned-rpm "${bundled_deps[@]}"
      fi
      run_zypper -n install "${RUNTIME_PACKAGES[@]}"
      if [[ "${WITH_VALIDATION_TOOLS}" == "1" ]]; then
        run_zypper -n install "${VALIDATION_PACKAGES[@]}"
      fi
      if [[ "${BUILD_TCMU_PLUGIN}" == "1" ]]; then
        run_zypper -n install "${BUILD_PACKAGES[@]}"
      fi
      ;;
  esac
}

# ── Distro-specific package fixups ──────────────────────────────────

fixup_known_package_issues() {
  # Ubuntu 20.04: python3-urwid test file has missing % operator causing SyntaxWarning
  local urwid_test="/usr/lib/python3/dist-packages/urwid/tests/test_canvas.py"
  if [[ -f "${urwid_test}" ]] && grep -q '" (result, expected)' "${urwid_test}" 2>/dev/null; then
    log "Patching urwid test_canvas.py SyntaxWarning (Ubuntu 20.04)"
    sed -i 's/" (result, expected)/" % (result, expected)/g' "${urwid_test}" 2>/dev/null || \
      warn "Failed to patch ${urwid_test}"
  fi
}

# ── Runtime capability checks ───────────────────────────────────────

runtime_command_hint() {
  local cmd="$1"
  case "${cmd}" in
    modprobe) printf 'kmod' ;;
    targetcli)
      case "${PKG_MANAGER}" in
        apt) printf 'targetcli-fb' ;;
        zypper) printf 'python3-targetcli-fb' ;;
        *) printf 'targetcli' ;;
      esac
      ;;
    mkfs.xfs) printf 'xfsprogs' ;;
    lsblk|findmnt|mount|umount)
      case "${PKG_MANAGER}" in
        zypper) printf 'util-linux-systemd' ;;
        *) printf 'util-linux' ;;
      esac
      ;;
    systemctl|journalctl) printf 'systemd' ;;
    groupadd|useradd)
      case "${PKG_MANAGER}" in
        apt) printf 'passwd' ;;
        zypper) printf 'shadow' ;;
        *) printf 'shadow-utils' ;;
      esac
      ;;
    df|install|cp|chown|chmod|rm|mkdir) printf 'coreutils' ;;
    grep|sed) printf '%s' "${cmd}" ;;
    sort|cut|wc) printf 'coreutils' ;;
    *) printf 'runtime package set' ;;
  esac
}

kernel_module_hint() {
  local module="$1"
  case "${PKG_MANAGER}:${module}" in
    dnf:target_core_user)
      printf 'kernel-modules-%s' "$(uname -r)"
      ;;
    dnf:*)
      printf 'kernel-core-%s or kernel-modules-core-%s' "$(uname -r)" "$(uname -r)"
      ;;
    apt:*)
      printf 'linux-modules-%s' "$(uname -r)"
      ;;
    zypper:*)
      printf 'kernel-default (not kernel-default-base) matching %s' "$(uname -r)"
      ;;
    *)
      printf 'kernel module package matching %s' "$(uname -r)"
      ;;
  esac
}

kernel_module_available_elsewhere() {
  local module="$1"
  find /lib/modules -type f -name "${module}.ko*" -print -quit 2>/dev/null | grep -q .
}

verify_runtime_commands() {
  local commands=(
    systemctl journalctl modprobe targetcli
    lsblk findmnt df mkfs.xfs mount umount
    getent groupadd useradd install cp chown chmod rm mkdir
    grep sed sort cut wc
  )
  local cmd missing=()

  if [[ "${DRY_RUN}" == "1" ]]; then
    for cmd in "${commands[@]}"; do
      printf '[dry-run][verify-command] %s (package: %s)\n' "${cmd}" "$(runtime_command_hint "${cmd}")"
    done
    return 0
  fi

  for cmd in "${commands[@]}"; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      missing+=("${cmd} [package: $(runtime_command_hint "${cmd}")]")
    fi
  done

  if [[ "${#missing[@]}" -gt 0 ]]; then
    printf '[holo-install][error] missing required runtime commands:\n' >&2
    printf '  - %s\n' "${missing[@]}" >&2
    die "install the missing packages above and retry"
  fi
}

verify_runtime_capabilities() {
  log "Verifying runtime commands"
  verify_runtime_commands
}

# ── User & directories ──────────────────────────────────────────────

ensure_user_and_dirs() {
  log "Creating service user and directories"
  if ! getent group "${SERVICE_GROUP}" >/dev/null 2>&1; then
    run_cmd groupadd --system "${SERVICE_GROUP}"
  fi
  if ! id -u "${SERVICE_USER}" >/dev/null 2>&1; then
    run_cmd useradd --system --gid "${SERVICE_GROUP}" --home-dir "${DATA_DIR}" --shell /sbin/nologin "${SERVICE_USER}"
  fi
  run_cmd mkdir -p "${PREFIX}/bin" "${PREFIX}/web-console" "${CONFIG_DIR}" "${DATA_DIR}/storage-pools" "${DATA_DIR}/targets" "${DATA_DIR}/media-state" "${LOG_DIR}" "${PLUGIN_DIR}"
  run_cmd chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "${DATA_DIR}" "${LOG_DIR}"
  run_cmd chmod 0750 "${CONFIG_DIR}"
}

load_kernel_modules() {
  log "Loading LIO/TCMU kernel modules"
  local module hint
  for module in target_core_mod target_core_user iscsi_target_mod; do
    if [[ "${DRY_RUN}" == "1" ]]; then
      printf '[dry-run][verify-module] %s (package: %s)\n' "${module}" "$(kernel_module_hint "${module}")"
      run_cmd modprobe "${module}"
      continue
    fi
    if modprobe "${module}"; then
      continue
    fi
    hint="$(kernel_module_hint "${module}")"
    if [[ "${PKG_MANAGER}" == "zypper" ]] && ! kernel_module_available_elsewhere "${module}"; then
      die "failed to load kernel module ${module}; SUSE minimal kernels may install kernel-default-base, but Holo-VTL requires the full ${hint}"
    fi
    if [[ "${PKG_MANAGER}" == "zypper" ]] && kernel_module_available_elsewhere "${module}"; then
      die "failed to load kernel module ${module}; a full SUSE kernel with this module is installed, but the running kernel $(uname -r) does not expose it yet; reboot into the installed kernel and rerun install"
    fi
    die "failed to load kernel module ${module}; likely missing package ${hint} for running kernel $(uname -r)"
  done
}

# ── Artifact installation ───────────────────────────────────────────

install_artifacts() {
  log "Installing Holo-VTL artifacts"
  run_cmd install -m 0755 "${CONTROL_PLANE_PATH}" "${PREFIX}/bin/control-plane"
  run_cmd install -m 0755 "${TCMU_HANDLER_PATH}" "${PREFIX}/bin/holo-tcmu-handler"
  run_cmd rm -rf "${PREFIX}/web-console/dist"
  run_cmd mkdir -p "${PREFIX}/web-console"
  run_cmd cp -a "${WEB_DIST_PATH}" "${PREFIX}/web-console/dist"

  if [[ "${BUILD_TCMU_PLUGIN}" == "1" ]]; then
    build_tcmu_plugin
  else
    run_cmd install -m 0644 "${HANDLER_SO_PATH}" "${PLUGIN_DIR}/handler_holo.so"
  fi

  if command -v getenforce >/dev/null 2>&1 || [[ "${DRY_RUN}" == "1" ]]; then
    run_cmd chcon -t bin_t "${PREFIX}/bin/control-plane" "${PREFIX}/bin/holo-tcmu-handler" || warn "SELinux chcon for binaries failed"
    run_cmd chcon -t lib_t "${PLUGIN_DIR}/handler_holo.so" || warn "SELinux chcon for handler_holo.so failed"
  fi
}

write_storage_helper() {
  log "Writing storage privilege helper"
  local helper_tmp helper_path
  helper_tmp="$(mktemp)"
  helper_path="${PREFIX}/bin/holo-storage-helper"
  cat >"${helper_tmp}" <<EOF
#!/usr/bin/env bash
set -euo pipefail

STORAGE_POOL_ROOT_BASE="${DATA_DIR}/storage-pools"

die() {
  printf 'holo-storage-helper: %s\n' "\$*" >&2
  exit 1
}

run_cmd() {
  local name="\$1"
  shift
  local candidate
  for candidate in "/usr/sbin/\${name}" "/sbin/\${name}" "/usr/bin/\${name}" "/bin/\${name}"; do
    if [[ -x "\${candidate}" ]]; then
      exec "\${candidate}" "\$@"
    fi
  done
  die "\${name} not found"
}

pool_path() {
  local path="\$1"
  [[ "\${path}" = /* ]] || die "pool path must be absolute"
  [[ "\${path}" != *..* ]] || die "pool path must not contain traversal"
  path="\${path%/}"
  [[ "\${path}" == "\${STORAGE_POOL_ROOT_BASE}/"* ]] || die "pool path outside storage pool root"
  printf '%s\n' "\${path}"
}

device_path() {
  local path="\$1"
  [[ "\${path}" =~ ^/dev/[-A-Za-z0-9._/]+$ ]] || die "invalid device path"
  [[ "\${path}" != *..* ]] || die "device path must not contain traversal"
  printf '%s\n' "\${path}"
}

cmd="\${1:-}"
[[ -n "\${cmd}" ]] || die "missing command"
shift

case "\${cmd}" in
  mkdir)
    [[ "\$#" -eq 2 && "\$1" == "-p" ]] || die "mkdir only supports: -p <pool-path>"
    run_cmd mkdir -p "\$(pool_path "\$2")"
    ;;
  chown)
    [[ "\$#" -eq 2 && "\$1" =~ ^[0-9]+:[0-9]+$ ]] || die "chown only supports: <uid:gid> <pool-path>"
    run_cmd chown "\$1" "\$(pool_path "\$2")"
    ;;
  mkfs.xfs)
    [[ "\$#" -eq 2 && "\$1" == "-f" ]] || die "mkfs.xfs only supports: -f <device>"
    run_cmd mkfs.xfs -f "\$(device_path "\$2")"
    ;;
  mount)
    [[ "\$#" -eq 4 && "\$1" == "-o" && "\$2" == "noatime,nodiratime" ]] || die "mount only supports Holo pool mount options"
    run_cmd mount -o noatime,nodiratime "\$(device_path "\$3")" "\$(pool_path "\$4")"
    ;;
  umount)
    [[ "\$#" -eq 1 ]] || die "umount only supports: <pool-path>"
    run_cmd umount "\$(pool_path "\$1")"
    ;;
  lsblk)
    [[ "\$#" -eq 3 && "\$1" == "-no" && "\$2" == "FSTYPE" ]] || die "lsblk only supports filesystem probing"
    run_cmd lsblk -no FSTYPE "\$(device_path "\$3")"
    ;;
  findmnt)
    if [[ "\$#" -eq 5 && "\$1" == "-rn" && "\$2" == "-S" && "\$4" == "-o" && "\$5" == "TARGET" ]]; then
      run_cmd findmnt -rn -S "\$(device_path "\$3")" -o TARGET
    elif [[ "\$#" -eq 5 && "\$1" == "-rn" && "\$2" == "-M" && "\$4" == "-o" && "\$5" == "SOURCE" ]]; then
      run_cmd findmnt -rn -M "\$(pool_path "\$3")" -o SOURCE
    else
      die "unsupported findmnt invocation"
    fi
    ;;
  *)
    die "unsupported command: \${cmd}"
    ;;
esac
EOF
  if [[ "${DRY_RUN}" == "1" ]]; then
    sed 's/^/[dry-run][helper] /' "${helper_tmp}"
  else
    install -m 0750 -o root -g root "${helper_tmp}" "${helper_path}"
  fi
  rm -f "${helper_tmp}"
}

write_targetcli_helper() {
  log "Writing targetcli privilege helper"
  local helper_tmp helper_path
  helper_tmp="$(mktemp)"
  helper_path="${PREFIX}/bin/holo-targetcli-helper"
  cat >"${helper_tmp}" <<EOF
#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'holo-targetcli-helper: %s\n' "\$*" >&2
  exit 1
}

targetcli_bin() {
  local candidate
  for candidate in /usr/bin/targetcli /usr/sbin/targetcli /bin/targetcli; do
    if [[ -x "\${candidate}" ]]; then
      printf '%s\n' "\${candidate}"
      return 0
    fi
  done
  die "targetcli not found"
}

valid_iqn() {
  [[ "\$1" =~ ^iqn\\.[0-9]{4}-[0-9]{2}\\.[A-Za-z0-9.-]+:[A-Za-z0-9._:-]+$ ]]
}

valid_name() {
  [[ "\$1" =~ ^[A-Za-z0-9_.:-]+$ ]]
}

valid_subtype() {
  [[ "\$1" =~ ^[A-Za-z0-9_-]+$ ]]
}

valid_path_value() {
  local value="\$1"
  [[ "\${value}" = /* ]] || return 1
  [[ "\${value}" != *..* ]] || return 1
  [[ "\${value}" =~ ^/[-A-Za-z0-9._/:]+$ ]] || return 1
  case "\${value}" in
    /run/holo/*|"${DATA_DIR}"/*)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

validate_backstore_ref() {
  local ref="\$1"
  if [[ "\${ref}" =~ ^/backstores/user:([A-Za-z0-9_-]+)/([A-Za-z0-9_.:-]+)$ ]]; then
    local bst_subtype="\${BASH_REMATCH[1]}" bst_name="\${BASH_REMATCH[2]}"
    valid_subtype "\${bst_subtype}" && valid_name "\${bst_name}"
    return \$?
  fi
  if [[ "\${ref}" =~ ^/backstores/fileio/([A-Za-z0-9_.:-]+)$ ]]; then
    local bst_name="\${BASH_REMATCH[1]}"
    valid_name "\${bst_name}"
    return \$?
  fi
  return 1
}

orig=("\$@")
[[ "\$#" -ge 2 ]] || die "missing targetcli path/action"
target_path="\$1"
action="\$2"
shift 2

case "\${target_path}:\${action}" in
  /backstores:ls)
    [[ "\$#" -eq 0 ]] || die "unsupported /backstores ls arguments"
    ;;
  /backstores/fileio:create)
    [[ "\$#" -eq 3 ]] || die "unsupported fileio create arguments"
    [[ "\$1" =~ ^name=([A-Za-z0-9_.:-]+)$ ]] && valid_name "\${BASH_REMATCH[1]}" || die "invalid fileio name"
    [[ "\$2" =~ ^file_or_dev=(/.+)$ ]] && valid_path_value "\${BASH_REMATCH[1]}" || die "invalid fileio path"
    [[ "\$3" =~ ^size=[0-9]+M$ ]] || die "invalid fileio size"
    ;;
  /backstores/fileio:delete)
    [[ "\$#" -eq 1 ]] && valid_name "\$1" || die "invalid fileio delete"
    ;;
  *)
    if [[ "\${target_path}" =~ ^/backstores/user:([A-Za-z0-9_-]+)$ ]]; then
      valid_subtype "\${BASH_REMATCH[1]}" || die "invalid user backstore subtype"
      case "\${action}" in
        create)
          [[ "\$#" -eq 3 ]] || die "unsupported user backstore create arguments"
          [[ "\$1" =~ ^name=([A-Za-z0-9_.:-]+)$ ]] && valid_name "\${BASH_REMATCH[1]}" || die "invalid user backstore name"
          [[ "\$2" =~ ^size=[0-9]+M$ ]] || die "invalid user backstore size"
          [[ "\$3" =~ ^cfgstring=(/.+)$ ]] && valid_path_value "\${BASH_REMATCH[1]}" || die "invalid user backstore cfgstring"
          ;;
        delete)
          [[ "\$#" -eq 1 ]] && valid_name "\$1" || die "invalid user backstore delete"
          ;;
        *)
          die "unsupported user backstore action"
          ;;
      esac
    elif [[ "\${target_path}" == "/iscsi" ]]; then
      [[ "\$#" -eq 1 ]] && valid_iqn "\$1" || die "invalid iscsi target"
      [[ "\${action}" == "create" || "\${action}" == "delete" ]] || die "unsupported iscsi action"
    elif [[ "\${target_path}" =~ ^/iscsi/(iqn\\.[0-9]{4}-[0-9]{2}\\.[A-Za-z0-9.-]+:[A-Za-z0-9._:-]+)/tpg1$ ]]; then
      valid_iqn "\${BASH_REMATCH[1]}" || die "invalid tpg iqn"
      [[ "\${action}" == "set" && "\$#" -ge 2 ]] || die "unsupported tpg action"
      case "\$1" in
        attribute)
          shift
          for item in "\$@"; do
            case "\${item}" in
              authentication=0|generate_node_acls=1|demo_mode_write_protect=0|cache_dynamic_acls=1) ;;
              *) die "unsupported tpg attribute" ;;
            esac
          done
          ;;
        parameter)
          [[ "\$#" -eq 2 ]] || die "unsupported tpg parameter arguments"
          case "\$2" in
            InitialR2T=No|ImmediateData=Yes|FirstBurstLength=8388608|MaxBurstLength=8388608|MaxRecvDataSegmentLength=8388608|MaxXmitDataSegmentLength=8388608) ;;
            *) die "unsupported tpg parameter" ;;
          esac
          ;;
        *)
          die "unsupported tpg set type"
          ;;
      esac
    elif [[ "\${target_path}" =~ ^/iscsi/(iqn\\.[0-9]{4}-[0-9]{2}\\.[A-Za-z0-9.-]+:[A-Za-z0-9._:-]+)/tpg1/luns$ ]]; then
      valid_iqn "\${BASH_REMATCH[1]}" || die "invalid lun iqn"
      [[ "\${action}" == "create" && "\$#" -eq 1 ]] || die "unsupported lun action"
      validate_backstore_ref "\$1" || die "invalid lun backstore reference"
    else
      die "unsupported targetcli command"
    fi
    ;;
esac

bin="\$(targetcli_bin)"
export TARGETCLI_HOME="/tmp/.holo-targetcli"
exec "\${bin}" "\${orig[@]}"
EOF
  if [[ "${DRY_RUN}" == "1" ]]; then
    sed 's/^/[dry-run][targetcli-helper] /' "${helper_tmp}"
  else
    install -m 0750 -o root -g root "${helper_tmp}" "${helper_path}"
  fi
  rm -f "${helper_tmp}"
}

write_support_helper() {
  log "Writing support bundle privilege helper"
  local helper_tmp helper_path
  helper_tmp="$(mktemp)"
  helper_path="${PREFIX}/bin/holo-support-helper"
  cat >"${helper_tmp}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'holo-support-helper: %s\n' "$*" >&2
  exit 64
}

resolve_bin() {
  local name="$1"
  shift
  for candidate in "$@"; do
    if [[ -x "${candidate}" ]]; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done
  die "${name} not found"
}

valid_unit() {
  [[ "$1" =~ ^[A-Za-z0-9@._-]+$ ]] && [[ "$1" != *..* ]]
}

valid_count() {
  [[ "$1" =~ ^[0-9]+$ ]] && (( $1 > 0 )) && (( $1 <= 100000 ))
}

valid_support_path() {
  case "$1" in
    /etc/holo|/etc/holo/*|/var/lib/holo|/var/lib/holo/*|/var/log/holo|/var/log/holo/*|/run/holo|/run/holo/*)
      [[ "$1" != *..* ]]
      ;;
    *)
      return 1
      ;;
  esac
}

run_targetcli() {
  local bin home
  bin="$(resolve_bin targetcli /usr/bin/targetcli /usr/sbin/targetcli /bin/targetcli)"
  home="/run/holo/targetcli-home"
  mkdir -p "${home}"
  export TARGETCLI_HOME="${home}"
  exec "${bin}" "$@"
}

[[ $# -ge 1 ]] || die "missing subcommand"
sub="$1"
shift

case "${sub}" in
  targetcli-ls)
    [[ $# -eq 0 ]] || die "unsupported targetcli-ls arguments"
    run_targetcli ls
    ;;
  targetcli-backstores)
    [[ $# -eq 0 ]] || die "unsupported targetcli-backstores arguments"
    run_targetcli /backstores ls
    ;;
  targetcli-iscsi)
    [[ $# -eq 0 ]] || die "unsupported targetcli-iscsi arguments"
    run_targetcli /iscsi ls
    ;;
  find-config)
    [[ $# -eq 1 ]] || die "find-config requires <path>"
    valid_support_path "$1" || die "invalid support path"
    bin="$(resolve_bin find /usr/bin/find /bin/find)"
    exec "${bin}" "$1" -maxdepth 3 -ls
    ;;
  find-log)
    [[ $# -eq 1 ]] || die "find-log requires <path>"
    valid_support_path "$1" || die "invalid support path"
    bin="$(resolve_bin find /usr/bin/find /bin/find)"
    exec "${bin}" "$1" -maxdepth 3 -ls
    ;;
  find-data)
    [[ $# -eq 1 ]] || die "find-data requires <path>"
    valid_support_path "$1" || die "invalid support path"
    bin="$(resolve_bin find /usr/bin/find /bin/find)"
    exec "${bin}" "$1" -maxdepth 4 -type f -printf '%p %s bytes %TY-%Tm-%Td %TH:%TM:%TS\n'
    ;;
  find-run)
    [[ $# -eq 1 ]] || die "find-run requires <path>"
    valid_support_path "$1" || die "invalid support path"
    bin="$(resolve_bin find /usr/bin/find /bin/find)"
    exec "${bin}" "$1" -maxdepth 3 -ls
    ;;
  dmesg)
    [[ $# -eq 0 ]] || die "unsupported dmesg arguments"
    bin="$(resolve_bin dmesg /bin/dmesg /usr/bin/dmesg)"
    exec "${bin}" -T --level=err,warn
    ;;
  iscsiadm-sessions)
    [[ $# -eq 0 ]] || die "unsupported iscsiadm-sessions arguments"
    bin="$(resolve_bin iscsiadm /usr/bin/iscsiadm /sbin/iscsiadm /usr/sbin/iscsiadm)"
    exec "${bin}" -m session
    ;;
  iscsiadm-nodes)
    [[ $# -eq 0 ]] || die "unsupported iscsiadm-nodes arguments"
    bin="$(resolve_bin iscsiadm /usr/bin/iscsiadm /sbin/iscsiadm /usr/sbin/iscsiadm)"
    exec "${bin}" -m node
    ;;
  sg-map-i)
    [[ $# -eq 0 ]] || die "unsupported sg-map-i arguments"
    bin="$(resolve_bin sg_map /usr/bin/sg_map /usr/sbin/sg_map /bin/sg_map)"
    exec "${bin}" -i
    ;;
  journalctl-unit)
    [[ $# -eq 2 ]] || die "journalctl-unit requires <unit> <count>"
    valid_unit "$1" || die "invalid unit name"
    valid_count "$2" || die "invalid count"
    bin="$(resolve_bin journalctl /bin/journalctl /usr/bin/journalctl)"
    exec "${bin}" -u "$1" -n "$2" --no-pager -o short-iso
    ;;
  journalctl-kernel)
    [[ $# -eq 1 ]] || die "journalctl-kernel requires <count>"
    valid_count "$1" || die "invalid count"
    bin="$(resolve_bin journalctl /bin/journalctl /usr/bin/journalctl)"
    exec "${bin}" -k -n "$1" --no-pager -o short-iso
    ;;
  journalctl-cdb-trace)
    [[ $# -eq 2 ]] || die "journalctl-cdb-trace requires <unit> <count>"
    valid_unit "$1" || die "invalid unit name"
    valid_count "$2" || die "invalid count"
    bin="$(resolve_bin journalctl /bin/journalctl /usr/bin/journalctl)"
    grep_bin="$(resolve_bin grep /bin/grep /usr/bin/grep)"
    set +o pipefail
    "${bin}" -u "$1" -n "$2" --no-pager -o short-iso | "${grep_bin}" '\[cdb_trace\]'
    exit 0
    ;;
  *)
    die "unsupported support helper subcommand: ${sub}"
    ;;
esac
EOF
  if [[ "${DRY_RUN}" == "1" ]]; then
    sed 's/^/[dry-run][support-helper] /' "${helper_tmp}"
  else
    install -m 0750 -o root -g root "${helper_tmp}" "${helper_path}"
  fi
  rm -f "${helper_tmp}"
}

stop_control_plane_for_upgrade() {
  if [[ "${ACTION}" != "upgrade" ]]; then return 0; fi
  log "Stopping control-plane before upgrade"
  run_shell "systemctl stop holo-control-plane 2>/dev/null || true"
}

build_tcmu_plugin() {
  log "Building TCMU plugin from source"
  local handler_src="${BUNDLE_DIR}/handler_holo.c"
  if [[ ! -f "${handler_src}" ]]; then
    handler_src="${SCRIPT_DIR}/../infra/tcmu/handler_holo.c"
  fi
  run_cmd gcc -std=gnu11 -O2 -fPIC -shared -Wall -Wextra \
    -I"${PLUGIN_SOURCE_DIR}" \
    -I"${PLUGIN_SOURCE_DIR}/ccan" \
    -I"${PLUGIN_SOURCE_DIR}/ccan/ccan" \
    "${handler_src}" \
    -o "${PLUGIN_DIR}/handler_holo.so"
  run_cmd chmod 0644 "${PLUGIN_DIR}/handler_holo.so"
}

# ── Runtime config ──────────────────────────────────────────────────

detect_portal_host() {
  if [[ -n "${PORTAL_HOST}" ]]; then return 0; fi
  local addresses
  addresses="$(hostname -I 2>/dev/null || true)"
  PORTAL_HOST="${addresses%% *}"
  if [[ -z "${PORTAL_HOST}" ]]; then
    PORTAL_HOST="127.0.0.1"
  fi
}

existing_env_value() {
  local key="$1"
  local env_file="${CONFIG_DIR}/holo.env"
  local line
  if [[ -f "${env_file}" ]]; then
    while IFS= read -r line; do
      case "${line}" in
        "${key}="*)
          printf '%s\n' "${line#*=}"
          return 0
          ;;
      esac
    done <"${env_file}"
  fi
}

load_existing_api_key() {
  if [[ -n "${API_KEY_FILE}" ]]; then
    [[ -f "${API_KEY_FILE}" && -r "${API_KEY_FILE}" ]] || die_usage "--api-key-file must be a readable file"
    API_KEY="$(<"${API_KEY_FILE}")"
    validate_api_key_value "--api-key-file" "${API_KEY}"
    return 0
  fi
  if [[ -n "${API_KEY}" ]]; then return 0; fi
  API_KEY="$(existing_env_value HOLO_API_KEY || true)"
}

is_loopback_bind_addr() {
  case "$1" in
    127.*|localhost|\[::1\])
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

resolve_control_plane_bind_addr() {
  if [[ -z "${CONTROL_PLANE_BIND_ADDR}" ]]; then
    CONTROL_PLANE_BIND_ADDR="0.0.0.0"
  fi
}

write_runtime_config() {
  log "Writing runtime configuration"
  detect_portal_host
  load_existing_api_key
  resolve_control_plane_bind_addr
  local trusted_proxy_cidrs
  trusted_proxy_cidrs="$(existing_env_value HOLO_TRUSTED_PROXY_CIDRS || true)"
  if [[ -n "${HOLO_TRUSTED_PROXY_CIDRS:-}" ]]; then
    trusted_proxy_cidrs="${HOLO_TRUSTED_PROXY_CIDRS}"
  fi

  local env_tmp
  env_tmp="$(mktemp)"
  cat >"${env_tmp}" <<EOF
HOLO_HTTP_ADDR=${CONTROL_PLANE_BIND_ADDR}:${CONTROL_PLANE_PORT}
HOLO_API_KEY=${API_KEY}
HOLO_TRUSTED_PROXY_CIDRS=${trusted_proxy_cidrs}
HOLO_CONFIG_DIR=${CONFIG_DIR}
HOLO_DATA_DIR=${DATA_DIR}
HOLO_LOG_DIR=${LOG_DIR}
HOLO_RUN_DIR=/run/holo
HOLO_METADATA_DSN=${DATA_DIR}/holo.db
HOLO_TARGET_RUNTIME_MODE=tcmu
HOLO_TARGET_PORTAL_HOST=${PORTAL_HOST}
HOLO_TARGET_PORTAL_PORT=${PORTAL_PORT}
HOLO_TARGET_BACKSTORE_DIR=${DATA_DIR}/targets
HOLO_TARGET_BACKSTORE_SIZE_MB=128
HOLO_TARGET_RUNTIME_USE_SUDO=true
HOLO_TARGETCLI_PRIVILEGED_HELPER=${PREFIX}/bin/holo-targetcli-helper
HOLO_STORAGE_PRIVILEGED_HELPER=${PREFIX}/bin/holo-storage-helper
HOLO_SUPPORT_PRIVILEGED_HELPER=${PREFIX}/bin/holo-support-helper
HOLO_STORAGE_POOL_ROOT_BASE=${DATA_DIR}/storage-pools
HOLO_STRICT_STORAGE_FLOW=1
HOLO_MEDIA_STATE_DIR=${DATA_DIR}/media-state
HOLO_TCMU_SOCKET_DIR=/run/holo
HOLO_TCMU_HANDLER_BIN=${PREFIX}/bin/holo-tcmu-handler
HOLO_TCMU_TARGETCLI_TIMEOUT_SEC=15
HOLO_TCMU_SOCKET_BUF_BYTES=67108864
HOLO_STORAGE_SYNC_EVERY_WRITES=4096
HOLO_STORAGE_TRUST_HOT_CACHE=1
HOLO_TAPE_DEDUP_ENABLED=1
HOLO_TAPE_PAYLOAD_CHECKSUM_ENABLED=1
HOLO_READ_PREFETCH=1
HOLO_READ_PREFETCH_DEPTH=2
HOLO_USAGE_COUNTER_PERSIST_EVERY_OPS=8192
HOLO_CDB_TIMING_EVERY=0
HOLO_WEB_UI_DIST=${PREFIX}/web-console/dist
EOF
  if [[ "${DRY_RUN}" == "1" ]]; then
    sed 's/^/[dry-run][env] /' "${env_tmp}"
    rm -f "${env_tmp}"
  else
    install -m 0640 -o root -g "${SERVICE_GROUP}" "${env_tmp}" "${CONFIG_DIR}/holo.env"
    rm -f "${env_tmp}"
  fi
}

# ── Systemd & sudoers ───────────────────────────────────────────────

write_systemd_units() {
  log "Writing systemd service and sudoers policy"
  local unit_tmp sudoers_tmp
  unit_tmp="$(mktemp)"
  sudoers_tmp="$(mktemp)"

  cat >"${unit_tmp}" <<EOF
[Unit]
Description=Holo-VTL control-plane
Wants=network-online.target
After=network-online.target tcmu-runner.service
Requires=tcmu-runner.service

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
WorkingDirectory=${PREFIX}
EnvironmentFile=${CONFIG_DIR}/holo.env
RuntimeDirectory=holo
RuntimeDirectoryMode=0755
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID CAP_DAC_OVERRIDE CAP_FOWNER CAP_SYS_ADMIN CAP_AUDIT_WRITE CAP_CHOWN
ExecStart=${PREFIX}/bin/control-plane
KillMode=control-group
TimeoutStopSec=15s
SendSIGKILL=yes
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

cat >"${sudoers_tmp}" <<EOF
Defaults:${SERVICE_USER} !requiretty
Defaults:${SERVICE_USER} !pam_session
${SERVICE_USER} ALL=(root) NOPASSWD: ${PREFIX}/bin/holo-storage-helper
${SERVICE_USER} ALL=(root) NOPASSWD: ${PREFIX}/bin/holo-targetcli-helper
${SERVICE_USER} ALL=(root) NOPASSWD: ${PREFIX}/bin/holo-support-helper
EOF

  if [[ "${DRY_RUN}" == "1" ]]; then
    sed 's/^/[dry-run][unit] /' "${unit_tmp}"
    sed 's/^/[dry-run][sudoers] /' "${sudoers_tmp}"
  else
    if [[ -f /etc/systemd/system/holo-control-plane.service ]]; then
      cp -a /etc/systemd/system/holo-control-plane.service "/etc/systemd/system/holo-control-plane.service.bak.$(date +%Y%m%d%H%M%S)"
    fi
    install -m 0644 "${unit_tmp}" /etc/systemd/system/holo-control-plane.service
    visudo -cf "${sudoers_tmp}" >/dev/null 2>/dev/null
    install -m 0440 "${sudoers_tmp}" /etc/sudoers.d/holo
    restore_selinux_contexts /etc/systemd/system/holo-control-plane.service /etc/sudoers.d/holo
    systemctl daemon-reload
  fi
  rm -f "${unit_tmp}" "${sudoers_tmp}"
}

write_performance_tuning() {
  log "Writing VTL performance tuning"
  local tcmu_tmp sysctl_tmp
  tcmu_tmp="$(mktemp)"
  sysctl_tmp="$(mktemp)"

  cat >"${tcmu_tmp}" <<'EOF'
[Service]
Environment=HOLO_TCMU_SOCKET_BUF_BYTES=67108864
Environment=HOLO_TCMU_SOCKET_TIMEOUT_SEC=60
EOF

  cat >"${sysctl_tmp}" <<'EOF'
net.core.rmem_max = 134217728
net.core.wmem_max = 134217728
net.core.rmem_default = 1048576
net.core.wmem_default = 1048576
EOF

  if [[ "${DRY_RUN}" == "1" ]]; then
    sed 's/^/[dry-run][tcmu-runner] /' "${tcmu_tmp}"
    sed 's/^/[dry-run][sysctl] /' "${sysctl_tmp}"
  else
    install -d -m 0755 /etc/systemd/system/tcmu-runner.service.d
    install -m 0644 "${tcmu_tmp}" /etc/systemd/system/tcmu-runner.service.d/20-holo-perf.conf
    install -m 0644 "${sysctl_tmp}" /etc/sysctl.d/90-holo-vtl.conf
    restore_selinux_contexts /etc/systemd/system/tcmu-runner.service.d /etc/sysctl.d/90-holo-vtl.conf
    systemctl daemon-reload
    sysctl -p /etc/sysctl.d/90-holo-vtl.conf >/dev/null || warn "sysctl performance tuning apply failed"
  fi
  rm -f "${tcmu_tmp}" "${sysctl_tmp}"
}

# ── Service management ──────────────────────────────────────────────

start_services() {
  log "Enabling and starting services"
  run_cmd systemctl enable --now tcmu-runner
  run_cmd systemctl restart tcmu-runner
  run_cmd systemctl enable --now holo-control-plane
  run_cmd systemctl restart holo-control-plane
}

verify_install() {
  log "Verifying installation"
  if [[ "${DRY_RUN}" == "1" ]]; then
    log "Dry-run verification skipped"
    return 0
  fi

  systemctl is-active --quiet tcmu-runner || {
    systemctl status tcmu-runner --no-pager -l || true
    die "tcmu-runner is not active"
  }
  systemctl is-active --quiet holo-control-plane || {
    systemctl status holo-control-plane --no-pager -l || true
    die "holo-control-plane is not active"
  }

  local backstores
  backstores="$(targetcli /backstores ls 2>&1 || true)"
  grep -q 'user:holo' <<<"${backstores}" || {
    printf '%s\n' "${backstores}" >&2
    die "user:holo backstore is not registered"
  }

  local health deadline
  health=""
  deadline=$((SECONDS + 120))
  while (( SECONDS < deadline )); do
    health="$(timeout 5s bash -c 'port="$1"; if ! exec 3<>"/dev/tcp/127.0.0.1/${port}"; then exit 1; fi; printf "GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n" >&3; head -c 512 <&3' _ "${CONTROL_PLANE_PORT}" 2>&1 || true)"
    if grep -q '200 OK' <<<"${health}"; then
      return 0
    fi
    sleep 2
  done
  printf '%s\n' "${health}" >&2
  journalctl -u holo-control-plane -n 80 --no-pager >&2 || true
  die "control-plane health check failed"
}

write_summary() {
  log "Writing install summary"
  local summary="${DATA_DIR}/install-summary.txt"
  local content
  content=$(cat <<EOF
Holo-VTL install summary
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
action=${ACTION}
platform=${OS_ID} ${OS_VERSION_ID} ${ARCH}
package_manager=${PKG_MANAGER}
prefix=${PREFIX}
config=${CONFIG_DIR}/holo.env
data_dir=${DATA_DIR}
portal=${PORTAL_HOST}:${PORTAL_PORT}
runtime_mode=tcmu
strict_storage_flow=1
web_ui=http://${PORTAL_HOST}/ui/
EOF
)
  if [[ "${DRY_RUN}" == "1" ]]; then
    printf '%s\n' "${content}" | sed 's/^/[dry-run][summary] /'
  else
    printf '%s\n' "${content}" >"${summary}"
    chown "${SERVICE_USER}:${SERVICE_GROUP}" "${summary}"
    log "Summary: ${summary}"
  fi
  if [[ "${#WARNINGS[@]}" -gt 0 ]]; then
    printf '[holo-install] warnings:\n'
    printf '  - %s\n' "${WARNINGS[@]}"
  fi
}

# ── Uninstall ───────────────────────────────────────────────────────

cleanup_runtime_targets() {
  log "Cleaning Holo-VTL runtime targets"
  run_shell "if command -v targetcli >/dev/null 2>&1; then targetcli /iscsi ls 2>/dev/null | grep -oE 'iqn\\.2026-04\\.[a-z.]+\\.holo:[^ ]+' | while read -r iqn; do targetcli /iscsi delete \"\$iqn\" >/dev/null 2>&1 || true; done; fi"
  run_shell "if command -v targetcli >/dev/null 2>&1; then targetcli /backstores/user:holo ls 2>/dev/null | grep -oE 'holo_pub_[A-Za-z0-9_.:-]+' | while read -r bs; do targetcli /backstores/user:holo delete \"\$bs\" >/dev/null 2>&1 || true; done; fi"
  run_shell "pgrep -f '^${PREFIX}/bin/holo-tcmu-handler( |$)' | xargs -r kill -TERM 2>/dev/null || true"
  run_shell "sleep 1; pgrep -f '^${PREFIX}/bin/holo-tcmu-handler( |$)' | xargs -r kill -KILL 2>/dev/null || true"
}

unmount_storage_pools() {
  log "Unmounting Holo-VTL storage pools"
  run_shell "base='${DATA_DIR}/storage-pools'
if [[ ! -d \"\${base}\" ]]; then
  exit 0
fi
if ! command -v findmnt >/dev/null 2>&1; then
  echo '[holo-install][warn] findmnt not found; skipping storage pool unmount discovery' >&2
  exit 0
fi
findmnt -R -n -o TARGET --target \"\${base}\" 2>/dev/null \
  | while IFS= read -r target; do printf '%s %s\n' \"\${#target}\" \"\${target}\"; done \
  | sort -rn \
  | cut -d' ' -f2- \
  | while IFS= read -r target; do
      [[ -n \"\${target}\" ]] || continue
      case \"\${target}\" in
        \"\${base}\"|\"\${base}\"/*)
          umount \"\${target}\" || {
            echo \"[holo-install][error] failed to unmount storage pool mount: \${target}\" >&2
            echo '[holo-install][error] close iSCSI sessions or processes using the pool, then retry uninstall' >&2
            exit 1
          }
          ;;
      esac
    done"
}

remove_systemd_units() {
  log "Removing Holo-VTL systemd and sudoers files"
  run_shell "if systemctl list-unit-files holo-control-plane.service >/dev/null 2>&1 || systemctl status holo-control-plane >/dev/null 2>&1; then
  if command -v timeout >/dev/null 2>&1; then
    timeout 15s systemctl stop holo-control-plane 2>/dev/null || {
      systemctl kill --kill-who=all --signal=SIGKILL holo-control-plane 2>/dev/null || true
      timeout 10s systemctl stop holo-control-plane 2>/dev/null || true
    }
  else
    systemctl stop holo-control-plane 2>/dev/null || {
      systemctl kill --kill-who=all --signal=SIGKILL holo-control-plane 2>/dev/null || true
    }
  fi
fi
systemctl disable holo-control-plane 2>/dev/null || true
systemctl reset-failed holo-control-plane 2>/dev/null || true"
  run_cmd rm -f /etc/systemd/system/holo-control-plane.service
  run_cmd rm -f /etc/systemd/system/tcmu-runner.service.d/20-holo-perf.conf
  run_cmd rm -f /etc/sysctl.d/90-holo-vtl.conf
  run_cmd rm -f /etc/sudoers.d/holo
  run_shell "systemctl daemon-reload 2>/dev/null || true"
}

remove_installed_artifacts() {
  log "Removing Holo-VTL application files and TCMU plugin"
  run_cmd rm -rf "${PREFIX}"
  run_cmd rm -f /usr/lib64/tcmu-runner/handler_holo.so
  run_cmd rm -f /usr/lib/x86_64-linux-gnu/tcmu-runner/handler_holo.so
  run_shell "systemctl restart tcmu-runner 2>/dev/null || true"
}

purge_config_and_data() {
  if [[ "${PURGE_DATA}" != "1" ]]; then
    log "Preserving ${CONFIG_DIR}, ${DATA_DIR}, and ${LOG_DIR}"
    return 0
  fi
  log "Purging Holo-VTL config, data, and logs"
  run_cmd rm -rf "${CONFIG_DIR}" "${DATA_DIR}" "${LOG_DIR}"
}

# ── Main flows ──────────────────────────────────────────────────────

uninstall_holo() {
  require_root_for_system_change
  print_uninstall_plan
  remove_systemd_units
  cleanup_runtime_targets
  unmount_storage_pools
  remove_installed_artifacts
  purge_config_and_data
  log "Uninstall completed"
}

install_or_upgrade_holo() {
  require_root_for_system_change
  reject_unsafe_storage_flow
  canonicalize_inputs
  validate_artifacts
  detect_platform
  build_package_plan
  preflight_host
  print_plan
  install_packages
  fixup_known_package_issues
  verify_runtime_capabilities
  resolve_plugin_source_dir
  load_kernel_modules
  ensure_user_and_dirs
  stop_control_plane_for_upgrade
  if [[ "${ACTION}" == "upgrade" ]]; then
    cleanup_runtime_targets
  fi
  install_artifacts
  write_storage_helper
  write_targetcli_helper
  write_support_helper
  write_runtime_config
  write_systemd_units
  write_performance_tuning
  start_services
  verify_install
  write_summary
  if [[ "${ACTION}" == "upgrade" ]]; then
    log "Upgrade completed"
  else
    log "Install completed"
  fi
}

main() {
  parse_args "$@"
  validate_action_options
  validate_safe_inputs
  case "${ACTION}" in
    install|upgrade)
      install_or_upgrade_holo
      ;;
    uninstall)
      uninstall_holo
      ;;
    *)
      die_usage "unknown command: ${ACTION}"
      ;;
  esac
}

main "$@"
