#!/usr/bin/env bash
set -euo pipefail

# ── Holo-VTL Release Builder ─────────────────────────────────────
# Builds universal Linux x86_64 package on a remote build machine.
#
# Usage:
#   ./scripts/build-release.sh                    # auto version from git
#   ./scripts/build-release.sh -v 1.0.0           # explicit version
#   ./scripts/build-release.sh -v 1.0.0-beta.1    # pre-release
#   ./scripts/build-release.sh -v 1.0.0 --skip-sync  # rebuild existing remote source
#
# Output: release/holo-vtl-<version>-linux-x86_64.tar.gz

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
RELEASE_DIR="${PROJECT_DIR}/release"

# ── Config ────────────────────────────────────────────────────────
BUILD_HOST="${HOLO_BUILD_HOST:-lei@10.10.1.187}"
BUILD_DIR="${HOLO_BUILD_DIR:-/home/lei/holo-build}"
SSH_OPTS="${HOLO_BUILD_SSH_OPTS:--o StrictHostKeyChecking=accept-new -o ConnectTimeout=10}"

VERSION=""
SKIP_SYNC=0

# ── Args ──────────────────────────────────────────────────────────
usage() {
  cat <<'EOF'
Usage: build-release.sh [options]

Options:
  -v, --version VERSION   Version string (default: git tag or short hash)
  --skip-sync             Skip rsync, rebuild from existing remote source
  --host HOST             Build host (default: lei@10.10.1.187)
  -h, --help              Show this help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -v|--version)  [[ $# -ge 2 ]] || { echo "error: --version requires a value" >&2; exit 1; }
                   VERSION="$2"; shift 2 ;;
    --skip-sync)   SKIP_SYNC=1; shift ;;
    --host)        [[ $# -ge 2 ]] || { echo "error: --host requires a value" >&2; exit 1; }
                   BUILD_HOST="$2"; shift 2 ;;
    -h|--help)     usage; exit 0 ;;
    *)             echo "error: unknown option: $1" >&2; exit 1 ;;
  esac
done

# ── Resolve version ───────────────────────────────────────────────
if [[ -z "${VERSION}" ]]; then
  VERSION="$(git -C "${PROJECT_DIR}" describe --tags --always --dirty 2>/dev/null || true)"
  if [[ -z "${VERSION}" ]]; then
    VERSION="0.0.0-preview.$(git -C "${PROJECT_DIR}" rev-parse --short HEAD 2>/dev/null || date +%Y%m%d%H%M)"
  fi
  # Strip leading 'v' if present from git tag
  VERSION="${VERSION#v}"
fi

PACKAGE_DIR_NAME="holo-vtl-${VERSION}-linux-x86_64"
TARBALL_NAME="${PACKAGE_DIR_NAME}.tar.gz"

echo "=========================================="
echo " Holo-VTL Release Builder"
echo " Version : ${VERSION}"
echo " Host    : ${BUILD_HOST}"
echo " Tarball : ${TARBALL_NAME}"
echo "=========================================="

# ── Preflight ─────────────────────────────────────────────────────
echo "[1/7] Preflight check..."
ssh ${SSH_OPTS} "${BUILD_HOST}" "uname -m" >/dev/null 2>&1 || {
  echo "error: cannot SSH to ${BUILD_HOST}" >&2; exit 1
}

mkdir -p "${RELEASE_DIR}"

# ── Sync source ───────────────────────────────────────────────────
if [[ "${SKIP_SYNC}" == "0" ]]; then
  echo "[2/7] Syncing source to build host..."
  rsync -az --delete \
    --exclude='.git' \
    --exclude='target/' \
    --exclude='node_modules/' \
    --exclude='bin/' \
    --exclude='output/' \
    --exclude='package-root/' \
    --exclude='web-console/dist/' \
    --exclude='.DS_Store' \
    --exclude='*.tar.gz' \
    --exclude='release/' \
    -e "ssh ${SSH_OPTS}" \
    "${PROJECT_DIR}/" \
    "${BUILD_HOST}:${BUILD_DIR}/"

  # Re-sync src/bin (excluded by --exclude='bin/' pattern)
  ssh ${SSH_OPTS} "${BUILD_HOST}" "mkdir -p ${BUILD_DIR}/data-plane/src/bin"
  rsync -az -e "ssh ${SSH_OPTS}" \
    "${PROJECT_DIR}/data-plane/src/bin/" \
    "${BUILD_HOST}:${BUILD_DIR}/data-plane/src/bin/"
else
  echo "[2/7] Skipping sync (--skip-sync)"
  echo "      WARNING: using existing source on ${BUILD_HOST}:${BUILD_DIR}; omit --skip-sync for branch-accurate release builds."
fi

# ── Build ─────────────────────────────────────────────────────────
echo "[3/7] Building on remote host..."
ssh ${SSH_OPTS} "${BUILD_HOST}" "export VERSION=${VERSION}; bash -s" << REMOTE_BUILD
set -euo pipefail

BUILD_DIR="${BUILD_DIR}"
OUTPUT_DIR="${BUILD_DIR}/output"
chmod -R u+rwX "\${OUTPUT_DIR}" 2>/dev/null || true
rm -rf "\${OUTPUT_DIR}"
mkdir -p "\${OUTPUT_DIR}"
BUILD_TMP="\$(mktemp -d)"
trap 'rm -rf "\${BUILD_TMP}"' EXIT

export PATH="/usr/local/go/bin:\$HOME/.cargo/bin:\$PATH"
export GOPROXY=https://goproxy.cn,direct
export RUSTUP_DIST_SERVER=https://mirrors.tuna.tsinghua.edu.cn/rustup

GO_VERSION="1.24.3"
GO_TARBALL="go\${GO_VERSION}.linux-amd64.tar.gz"
GO_TARBALL_SHA256="3333f6ea53afa971e9078895eaa4ac7204a8c6b5c68c10e6bc9a33e8e391bdd8"
RUSTUP_VERSION="1.27.1"
RUSTUP_INIT_SHA256="6aeece6993e902708983b209d04c0d1dbb14ebb405ddb87def578d41f920f56d"
NODE_VERSION="20.11.1"
NODE_TARBALL="node-v\${NODE_VERSION}-linux-x64.tar.xz"
NODE_TARBALL_SHA256="d8dab549b09672b03356aa2257699f3de3b58c96e74eb26a8b495fbdc9cf6fbe"
TCMU_RUNNER_GIT_REF="\${HOLO_TCMU_RUNNER_GIT_REF:-v1.5.4}"
TCMU_RUNNER_EL_TARGETS="\${HOLO_TCMU_RUNNER_EL_TARGETS:-8 9 10}"

download_verified() {
  local name="\$1"
  local expected_sha="\$2"
  local output="\$3"
  shift 3

  local url candidate actual
  for url in "\$@"; do
    candidate="\${output}.download"
    rm -f "\${candidate}"
    echo "  Downloading \${name} from \${url}"
    if ! curl -fsSL "\${url}" -o "\${candidate}"; then
      echo "  WARNING: failed to download \${name} from \${url}" >&2
      continue
    fi
    actual="\$(sha256sum "\${candidate}" | awk '{print \$1}')"
    if [[ "\${actual}" != "\${expected_sha}" ]]; then
      echo "  WARNING: checksum mismatch for \${name} from \${url}" >&2
      echo "           expected=\${expected_sha}" >&2
      echo "           actual=\${actual}" >&2
      rm -f "\${candidate}"
      continue
    fi
    mv "\${candidate}" "\${output}"
    echo "  Verified \${name} sha256=\${expected_sha}"
    return 0
  done

  echo "error: unable to download verified \${name}" >&2
  return 1
}

# ── Ensure build tools ──
ensure_go() {
  if command -v go >/dev/null 2>&1 && go version | grep -q 'go1.2'; then
    return 0
  fi
  echo "  Installing Go..."
  go_archive="\${BUILD_TMP}/\${GO_TARBALL}"
  download_verified "Go \${GO_VERSION}" "\${GO_TARBALL_SHA256}" "\${go_archive}" \
    "https://mirrors.aliyun.com/golang/\${GO_TARBALL}" \
    "https://go.dev/dl/\${GO_TARBALL}"
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "\${go_archive}"
}

ensure_rust() {
  source "\$HOME/.cargo/env" 2>/dev/null || true
  if command -v cargo >/dev/null 2>&1 && cargo --version >/dev/null 2>&1; then
    rustup target add x86_64-unknown-linux-musl 2>/dev/null || true
    return 0
  fi
  echo "  Installing Rust..."
  rustup_init="\${BUILD_TMP}/rustup-init"
  download_verified "rustup-init \${RUSTUP_VERSION}" "\${RUSTUP_INIT_SHA256}" "\${rustup_init}" \
    "https://mirrors.tuna.tsinghua.edu.cn/rustup/rustup/dist/x86_64-unknown-linux-gnu/rustup-init" \
    "https://static.rust-lang.org/rustup/archive/\${RUSTUP_VERSION}/x86_64-unknown-linux-gnu/rustup-init"
  chmod +x "\${rustup_init}"
  "\${rustup_init}" -y --default-toolchain stable --profile default 2>&1 | tail -3
  source "\$HOME/.cargo/env"
  rustup target add x86_64-unknown-linux-musl
}

ensure_docker() {
  if command -v docker >/dev/null 2>&1; then return 0; fi
  echo "  Installing Docker..."
  sudo apt-get update -qq
  sudo apt-get install -y -qq docker.io 2>&1 | tail -3
  sudo systemctl start docker 2>/dev/null || true
  sudo usermod -aG docker \$(whoami)
  echo "  WARNING: Docker installed but may need re-login for group membership" >&2
  echo "  Using sudo docker as fallback" >&2
}

ensure_node() {
  if command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then
    major="\$(node -p 'Number(process.versions.node.split(".")[0])' 2>/dev/null || echo 0)"
    if [ "\${major}" -ge 18 ]; then
      return 0
    fi
  fi
  echo "  Installing Node.js..."
  node_archive="\${BUILD_TMP}/\${NODE_TARBALL}"
  download_verified "Node.js \${NODE_VERSION}" "\${NODE_TARBALL_SHA256}" "\${node_archive}" \
    "https://npmmirror.com/mirrors/node/v\${NODE_VERSION}/\${NODE_TARBALL}" \
    "https://nodejs.org/dist/v\${NODE_VERSION}/\${NODE_TARBALL}"
  sudo rm -rf "/usr/local/node-v\${NODE_VERSION}-linux-x64"
  sudo tar -C /usr/local -xJf "\${node_archive}"
  sudo ln -sf "/usr/local/node-v\${NODE_VERSION}-linux-x64/bin/node" /usr/local/bin/node
  sudo ln -sf "/usr/local/node-v\${NODE_VERSION}-linux-x64/bin/npm" /usr/local/bin/npm
  sudo ln -sf "/usr/local/node-v\${NODE_VERSION}-linux-x64/bin/npx" /usr/local/bin/npx
}

ensure_go
ensure_rust
ensure_docker
ensure_node

source "\$HOME/.cargo/env" 2>/dev/null || true

# ── Build Go binary ──
echo "  Building control-plane (static)..."
cd "\${BUILD_DIR}/control-plane"
VERSION_PKG="github.com/Holo-VTL/Holo/control-plane/internal/config"
COMMIT="\$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_DATE="\$(date -u +%Y-%m-%dT%H:%M:%SZ)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X \${VERSION_PKG}.Version=${VERSION} -X \${VERSION_PKG}.Commit=\${COMMIT} -X \${VERSION_PKG}.BuildDate=\${BUILD_DATE}" \
  -o "\${OUTPUT_DIR}/control-plane" ./cmd/api

# ── Build Rust binary ──
echo "  Building holo-tcmu-handler (musl static)..."
cd "\${BUILD_DIR}/data-plane"
mkdir -p .cargo
cat > .cargo/config.toml << 'CARGOEOF'
[source.crates-io]
replace-with = "rsproxy-sparse"
[source.rsproxy-sparse]
registry = "sparse+https://rsproxy.cn/index/"
CARGOEOF
cargo build --release --bin tcmu_handler --target x86_64-unknown-linux-musl 2>&1 | tail -5
cp target/x86_64-unknown-linux-musl/release/tcmu_handler "\${OUTPUT_DIR}/holo-tcmu-handler"
strip "\${OUTPUT_DIR}/holo-tcmu-handler" 2>/dev/null || true

# ── Build handler_holo.so (cached builder image) ──
TCMU_RUNNER_IMAGE_TAG="\$(printf '%s' "\${TCMU_RUNNER_GIT_REF}" | tr -c 'A-Za-z0-9_.-' '_')"
BUILDER_IMAGE="holo-handler-builder:\${TCMU_RUNNER_IMAGE_TAG}"
if ! sudo docker inspect "\$BUILDER_IMAGE" >/dev/null 2>&1; then
  echo "  Building cached handler builder image (first time only)..."
  sudo docker pull rockylinux:8 -q 2>/dev/null
  sudo docker build --build-arg TCMU_RUNNER_GIT_REF="\${TCMU_RUNNER_GIT_REF}" -t "\$BUILDER_IMAGE" - <<'DOCKEREOF'
FROM rockylinux:8
ARG TCMU_RUNNER_GIT_REF
RUN dnf install -y -q gcc git \
 && dnf clean all \
 && git clone --branch "\${TCMU_RUNNER_GIT_REF}" --depth 1 https://github.com/open-iscsi/tcmu-runner.git /tmp/tcmu-runner
DOCKEREOF
fi
echo "  Building handler_holo.so (Rocky 8, GLIBC_2.17+)..."
sudo docker run --rm \
  -v "\${BUILD_DIR}:/src:ro" \
  -v "\${OUTPUT_DIR}:/out" \
  "\$BUILDER_IMAGE" \
  gcc -std=gnu11 -O2 -fPIC -shared -Wall -Wextra \
    -I/tmp/tcmu-runner -I/tmp/tcmu-runner/ccan -I/tmp/tcmu-runner/ccan/ccan \
    /src/infra/tcmu/handler_holo.c \
    -pthread \
    -o /out/handler_holo.so
sudo chown \$(whoami):\$(whoami) "\${OUTPUT_DIR}/handler_holo.so" 2>/dev/null || true

# ── Build bundled tcmu-runner RPMs for EL family installers ──
build_tcmu_runner_rpms() {
  echo "  Building bundled tcmu-runner RPMs (\${TCMU_RUNNER_GIT_REF})..."

  local el out_dir
  for el in \${TCMU_RUNNER_EL_TARGETS}; do
    case "\${el}" in
      8|9|10) ;;
      *) echo "error: unsupported HOLO_TCMU_RUNNER_EL_TARGETS entry: \${el}" >&2; return 1 ;;
    esac
    local build_image="almalinux:\${el}"

    out_dir="\${OUTPUT_DIR}/packages/dnf/el\${el}"
    rm -rf "\${out_dir}"
    mkdir -p "\${out_dir}"

    echo "    EL\${el}: building tcmu-runner/libtcmu RPMs"
    sudo docker pull "\${build_image}" -q 2>/dev/null || true
    sudo docker run --rm -i \
      -e EL_MAJOR="\${el}" \
      -e TCMU_RUNNER_GIT_REF="\${TCMU_RUNNER_GIT_REF}" \
      -v "\${out_dir}:/out" \
      "\${build_image}" \
      bash -s <<'TCMURPMEOF'
set -euo pipefail

dnf install -y --nogpgcheck dnf-plugins-core epel-release
case "\${EL_MAJOR}" in
  8) dnf config-manager --set-enabled powertools || true ;;
  9) dnf config-manager --set-enabled crb || crb enable || true ;;
  10) dnf config-manager --set-enabled crb || crb enable || true ;;
esac
dnf install -y --nogpgcheck git rpm-build redhat-rpm-config cmake make gcc \
  libnl3-devel glib2-devel zlib-devel kmod-devel systemd-rpm-macros

git clone --branch "\${TCMU_RUNNER_GIT_REF}" --depth 1 https://github.com/open-iscsi/tcmu-runner.git /tmp/tcmu-runner
cd /tmp/tcmu-runner/extra
./make_runnerrpms.sh \
  --without rbd \
  --without glfs \
  --without qcow \
  --without zbc \
  --without tcmalloc

find rpmbuild/RPMS -type f \( -name 'tcmu-runner-*.rpm' -o -name 'libtcmu-*.rpm' \) \
  ! -name '*devel*' -exec cp -v {} /out/ \;
test -n "\$(find /out -maxdepth 1 -type f -name 'tcmu-runner-*.rpm' -print -quit)"
test -n "\$(find /out -maxdepth 1 -type f -name 'libtcmu-*.rpm' -print -quit)"
TCMURPMEOF
  done

  find "\${OUTPUT_DIR}/packages" -type f -name '*.rpm' -print | sort
}

build_tcmu_runner_rpms

# ── Build Web Console ──
echo "  Building web-console..."
cd "\${BUILD_DIR}/web-console"
rm -rf dist
npm ci
VITE_APP_VERSION="${VERSION}" npm run build

# ── Copy web console + installer ──
echo "  Packaging..."
mkdir -p "\${OUTPUT_DIR}/web-console"
cp -a "\${BUILD_DIR}/web-console/dist" "\${OUTPUT_DIR}/web-console/dist"
cp "\${BUILD_DIR}/infra/tcmu/handler_holo.c" "\${OUTPUT_DIR}/handler_holo.c"

# Use scripts/install.sh if it exists
if [ -f "\${BUILD_DIR}/scripts/install.sh" ]; then
  cp "\${BUILD_DIR}/scripts/install.sh" "\${OUTPUT_DIR}/install.sh"
fi

# Package the single maintained Linux installer.
if [ ! -f "\${BUILD_DIR}/scripts/install-holo-universal.sh" ]; then
  echo "Missing scripts/install-holo-universal.sh" >&2
  exit 1
fi
cp "\${BUILD_DIR}/scripts/install-holo-universal.sh" "\${OUTPUT_DIR}/install-holo.sh"

# ── Create tarball ──
cd "\${OUTPUT_DIR}"
PACKAGE_ROOT="\${BUILD_DIR}/package-root"
chmod -R u+rwX "\${PACKAGE_ROOT}" 2>/dev/null || true
rm -rf "\${PACKAGE_ROOT}"
mkdir -p "\${PACKAGE_ROOT}/${PACKAGE_DIR_NAME}"
cp -a control-plane holo-tcmu-handler handler_holo.so handler_holo.c \
  install-holo.sh install.sh web-console "\${PACKAGE_ROOT}/${PACKAGE_DIR_NAME}/"
if [ -d packages ]; then
  cp -a packages "\${PACKAGE_ROOT}/${PACKAGE_DIR_NAME}/"
fi
tar czf "\${BUILD_DIR}/${TARBALL_NAME}" -C "\${PACKAGE_ROOT}" "${PACKAGE_DIR_NAME}"
rm -rf "\${PACKAGE_ROOT}"

echo ""
echo "=== Build complete ==="
ls -lh "\${BUILD_DIR}/${TARBALL_NAME}"
REMOTE_BUILD

# ── Download ──────────────────────────────────────────────────────
echo "[4/7] Downloading tarball..."
rsync -azP -e "ssh ${SSH_OPTS}" \
  "${BUILD_HOST}:${BUILD_DIR}/${TARBALL_NAME}" \
  "${RELEASE_DIR}/" 2>&1 | tail -1

# ── Verify ────────────────────────────────────────────────────────
echo "[5/7] Verifying..."
VERIFY_DIR="$(mktemp -d)"
tar xzf "${RELEASE_DIR}/${TARBALL_NAME}" -C "${VERIFY_DIR}"

# Check all required files exist
VERIFY_ROOT="${VERIFY_DIR}/${PACKAGE_DIR_NAME}"
for f in control-plane holo-tcmu-handler handler_holo.so handler_holo.c install-holo.sh web-console/dist/index.html; do
  if [[ ! -f "${VERIFY_ROOT}/${f}" && ! -d "${VERIFY_ROOT}/${f}" ]]; then
    echo "error: missing ${f} in tarball" >&2
    rm -rf "${VERIFY_DIR}"
    exit 1
  fi
done
for platform in el8 el9 el10; do
  if [[ -z "$(find "${VERIFY_ROOT}/packages/dnf/${platform}" -maxdepth 1 -type f -name 'tcmu-runner-*.rpm' -print -quit 2>/dev/null)" ]]; then
    echo "error: missing bundled tcmu-runner RPM for ${platform}" >&2
    rm -rf "${VERIFY_DIR}"
    exit 1
  fi
  if [[ -z "$(find "${VERIFY_ROOT}/packages/dnf/${platform}" -maxdepth 1 -type f -name 'libtcmu-*.rpm' -print -quit 2>/dev/null)" ]]; then
    echo "error: missing bundled libtcmu RPM for ${platform}" >&2
    rm -rf "${VERIFY_DIR}"
    exit 1
  fi
done

echo "  control-plane:    $(du -h "${VERIFY_ROOT}/control-plane" | cut -f1) $(file "${VERIFY_ROOT}/control-plane" | grep -o 'statically linked')"
echo "  holo-tcmu-handler: $(du -h "${VERIFY_ROOT}/holo-tcmu-handler" | cut -f1) $(file "${VERIFY_ROOT}/holo-tcmu-handler" | grep -o 'static')"
echo "  handler_holo.so:   $(du -h "${VERIFY_ROOT}/handler_holo.so" | cut -f1)"
echo "  bundled RPMs:      $(find "${VERIFY_ROOT}/packages" -type f -name '*.rpm' | wc -l | tr -d ' ')"
rm -rf "${VERIFY_DIR}"

# ── Summary ───────────────────────────────────────────────────────
echo "[6/7] Done!"
echo ""
echo "  ${RELEASE_DIR}/${TARBALL_NAME}"
echo "  $(du -h "${RELEASE_DIR}/${TARBALL_NAME}" | cut -f1)"
echo ""
echo "  Install: (tmp=\$(mktemp -d) && trap 'rm -rf \"\$tmp\"' EXIT && tar xzf ${TARBALL_NAME} -C \"\$tmp\" && sudo bash \"\$tmp/${PACKAGE_DIR_NAME}/install-holo.sh\" install)"
echo "  Upgrade: (tmp=\$(mktemp -d) && trap 'rm -rf \"\$tmp\"' EXIT && tar xzf ${TARBALL_NAME} -C \"\$tmp\" && sudo bash \"\$tmp/${PACKAGE_DIR_NAME}/install-holo.sh\" upgrade)"
echo "=========================================="
