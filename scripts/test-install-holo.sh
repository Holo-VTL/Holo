#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALLER="${ROOT_DIR}/scripts/install-holo-universal.sh"
TMP_ROOT="$(mktemp -d)"

cleanup() {
  rm -rf "${TMP_ROOT}"
}
trap cleanup EXIT

fail() {
  echo "[test][fail] $*" >&2
  exit 1
}

assert_contains() {
  local haystack="$1"
  local needle="$2"
  grep -Fq -- "${needle}" <<<"${haystack}" || fail "expected output to contain: ${needle}"
}

assert_not_contains() {
  local haystack="$1"
  local needle="$2"
  if grep -Fq -- "${needle}" <<<"${haystack}"; then
    fail "expected output not to contain: ${needle}"
  fi
}

make_bundle() {
  local dir="$1"
  mkdir -p "${dir}/web-console/dist"
  printf '#!/bin/sh\n' >"${dir}/control-plane"
  printf '#!/bin/sh\n' >"${dir}/holo-tcmu-handler"
  chmod +x "${dir}/control-plane" "${dir}/holo-tcmu-handler"
  printf '<!doctype html>\n' >"${dir}/web-console/dist/index.html"
  printf 'fake-so\n' >"${dir}/handler_holo.so"
}

make_os_release() {
  local file="$1"
  local id="$2"
  local version="$3"
  cat >"${file}" <<EOF
ID=${id}
VERSION_ID="${version}"
EOF
}

run_dry() {
  local os_release="$1"
  local bundle="$2"
  shift 2
  HOLO_INSTALL_OS_RELEASE="${os_release}" \
  HOLO_INSTALL_UNAME_M=x86_64 \
    bash "${INSTALLER}" --dry-run --bundle-dir "${bundle}" --portal-host 10.0.0.10 "$@" 2>&1
}

run_dry_action() {
  local action="$1"
  local os_release="$2"
  local bundle="$3"
  shift 3
  HOLO_INSTALL_OS_RELEASE="${os_release}" \
  HOLO_INSTALL_UNAME_M=x86_64 \
    bash "${INSTALLER}" "${action}" --dry-run --bundle-dir "${bundle}" --portal-host 10.0.0.10 "$@" 2>&1
}

test_ubuntu_plan() {
  local bundle="${TMP_ROOT}/bundle-ubuntu"
  local osr="${TMP_ROOT}/ubuntu.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" ubuntu 22.04
  local out
  out="$(run_dry "${osr}" "${bundle}")"
  assert_contains "${out}" "Action: install"
  assert_contains "${out}" "Detected platform: ubuntu 22.04 x86_64 (apt)"
  assert_contains "${out}" "Runtime packages: kmod sudo targetcli-fb tcmu-runner xfsprogs open-iscsi"
  assert_contains "${out}" "Runtime invariant: HOLO_STRICT_STORAGE_FLOW=1"
  assert_contains "${out}" "[dry-run][env] HOLO_HTTP_ADDR=0.0.0.0:80"
  assert_contains "${out}" "[dry-run][env] HOLO_API_KEY="
  assert_contains "${out}" "[dry-run][env] HOLO_TRUSTED_PROXY_CIDRS="
  assert_contains "${out}" "[dry-run][env] HOLO_TARGET_RUNTIME_MODE=tcmu"
  assert_contains "${out}" "[dry-run][env] HOLO_TARGETCLI_PRIVILEGED_HELPER=/opt/holo/bin/holo-targetcli-helper"
  assert_contains "${out}" "[dry-run][env] HOLO_ISCSI_PRIVILEGED_HELPER=/opt/holo/bin/holo-iscsi-helper"
  assert_contains "${out}" "[dry-run][env] HOLO_STORAGE_PRIVILEGED_HELPER=/opt/holo/bin/holo-storage-helper"
  assert_contains "${out}" "[dry-run][env] HOLO_STRICT_STORAGE_FLOW=1"
  assert_contains "${out}" "[dry-run][env] HOLO_TCMU_SOCKET_BUF_BYTES=67108864"
  assert_contains "${out}" "[dry-run][env] HOLO_STORAGE_SYNC_EVERY_WRITES=4096"
  assert_contains "${out}" "[dry-run][env] HOLO_READ_PREFETCH=1"
  assert_contains "${out}" "[dry-run][env] HOLO_READ_PREFETCH_DEPTH=2"
  assert_contains "${out}" "[dry-run][env] HOLO_USAGE_COUNTER_PERSIST_EVERY_OPS=8192"
  assert_contains "${out}" "[dry-run][tcmu-runner] Environment=HOLO_TCMU_SOCKET_BUF_BYTES=67108864"
  assert_contains "${out}" "[dry-run][sysctl] net.core.rmem_max = 134217728"
  assert_contains "${out}" "[dry-run][unit] AmbientCapabilities=CAP_NET_BIND_SERVICE"
  assert_not_contains "${out}" "[dry-run][unit] AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_SYS_ADMIN"
  assert_contains "${out}" "[dry-run][unit] CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID CAP_DAC_OVERRIDE CAP_FOWNER CAP_SYS_ADMIN CAP_AUDIT_WRITE CAP_CHOWN"
  assert_not_contains "${out}" "[dry-run][unit] ProtectHome="
  assert_not_contains "${out}" "[dry-run][unit] ProtectSystem="
  assert_not_contains "${out}" "[dry-run][unit] ReadWritePaths="
  assert_not_contains "${out}" "[dry-run][unit] PrivateTmp=yes"
  assert_not_contains "${out}" "[dry-run][unit] RestrictNamespaces=yes"
  assert_not_contains "${out}" "[dry-run][unit] MemoryDenyWriteExecute=yes"
  assert_not_contains "${out}" "[dry-run][unit] LockPersonality=yes"
  assert_contains "${out}" "[dry-run][helper] STORAGE_POOL_ROOT_BASE=\"/var/lib/holo/storage-pools\""
  assert_contains "${out}" "[dry-run][targetcli-helper] valid_iqn()"
  assert_contains "${out}" "[dry-run][iscsi-helper]   login)"
  assert_contains "${out}" "[dry-run][support-helper]   export TARGETCLI_HOME=\"\${home}\""
  assert_contains "${out}" "[dry-run][support-helper]   find-config)"
  assert_contains "${out}" "[dry-run][support-helper]   sg-map-i)"
  assert_contains "${out}" "[dry-run][support-helper]     valid_support_path \"\$1\" || die \"invalid support path\""
  assert_contains "${out}" "[dry-run][sudoers] Defaults:holo !pam_session"
  assert_contains "${out}" "[dry-run][sudoers] holo ALL=(root) NOPASSWD: /opt/holo/bin/holo-storage-helper"
  assert_contains "${out}" "[dry-run][sudoers] holo ALL=(root) NOPASSWD: /opt/holo/bin/holo-targetcli-helper"
  assert_contains "${out}" "[dry-run][sudoers] holo ALL=(root) NOPASSWD: /opt/holo/bin/holo-iscsi-helper"
  assert_contains "${out}" "[dry-run][sudoers] holo ALL=(root) NOPASSWD: /opt/holo/bin/holo-support-helper"
  assert_not_contains "${out}" "[dry-run][sudoers] holo ALL=(root) NOPASSWD: /usr/bin/targetcli"
  assert_not_contains "${out}" "[dry-run][sudoers] holo ALL=(root) NOPASSWD: /usr/bin/mount"
  assert_not_contains "${out}" "[dry-run][sudoers] holo ALL=(root) NOPASSWD: /usr/sbin/mkfs.xfs"
  assert_not_contains "${out}" "[dry-run][sudoers] holo ALL=(root) NOPASSWD: /usr/bin/chown"
  assert_contains "${out}" "[dry-run][summary] web_ui=http://10.0.0.10/ui/"
  assert_contains "${out}" "[dry-run][verify-command] targetcli (package: targetcli-fb)"
  assert_contains "${out}" "[dry-run][verify-module] target_core_user (package: linux-modules-"
  assert_not_contains "${out}" "jq lsscsi sg3-utils"

  make_os_release "${osr}" ubuntu 25.04
  out="$(run_dry "${osr}" "${bundle}")"
  assert_contains "${out}" "Detected platform: ubuntu 25.04 x86_64 (apt)"
  assert_contains "${out}" "Runtime packages: kmod sudo targetcli-fb tcmu-runner xfsprogs open-iscsi"
}

test_authenticated_public_bind_plan() {
  local bundle="${TMP_ROOT}/bundle-auth-bind"
  local osr="${TMP_ROOT}/auth-bind.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" ubuntu 22.04
  local out
  out="$(run_dry "${osr}" "${bundle}" --api-key test-key)"
  assert_contains "${out}" "[dry-run][env] HOLO_HTTP_ADDR=0.0.0.0:80"
  assert_contains "${out}" "[dry-run][env] HOLO_API_KEY=test-key"
}

test_api_key_file_plan() {
  local bundle="${TMP_ROOT}/bundle-api-key-file"
  local osr="${TMP_ROOT}/api-key-file.os-release"
  local key_file="${TMP_ROOT}/api-key.txt"
  make_bundle "${bundle}"
  make_os_release "${osr}" ubuntu 22.04
  printf 'file-secret-key' >"${key_file}"
  local out code
  out="$(run_dry "${osr}" "${bundle}" --api-key-file "${key_file}")"
  assert_contains "${out}" "[dry-run][env] HOLO_API_KEY=file-secret-key"

  set +e
  out="$(run_dry "${osr}" "${bundle}" --api-key value --api-key-file "${key_file}")"
  code=$?
  set -e
  [[ "${code}" -eq 2 ]] || fail "api-key conflict should exit 2, got ${code}"
  assert_contains "${out}" "--api-key and --api-key-file are mutually exclusive"
}

test_explicit_no_login_public_bind_stays_quiet() {
  local bundle="${TMP_ROOT}/bundle-explicit-bind"
  local osr="${TMP_ROOT}/explicit-bind.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" ubuntu 22.04
  local out
  out="$(HOLO_HTTP_BIND_ADDR=0.0.0.0 HOLO_INSTALL_OS_RELEASE="${osr}" HOLO_INSTALL_UNAME_M=x86_64 bash "${INSTALLER}" --dry-run --bundle-dir "${bundle}" --portal-host 10.0.0.10 2>&1)"
  assert_not_contains "${out}" "HOLO_API_KEY is empty while HOLO_HTTP_BIND_ADDR=0.0.0.0 exposes no-login management"
  assert_contains "${out}" "[dry-run][env] HOLO_HTTP_ADDR=0.0.0.0:80"
}

test_upgrade_plan() {
  local bundle="${TMP_ROOT}/bundle-upgrade"
  local osr="${TMP_ROOT}/upgrade.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" ubuntu 22.04
  local out
  out="$(run_dry_action upgrade "${osr}" "${bundle}")"
  assert_contains "${out}" "Action: upgrade"
  assert_contains "${out}" "Stopping control-plane before upgrade"
  assert_contains "${out}" "systemctl\\ stop\\ holo-control-plane"
  assert_contains "${out}" "[dry-run][summary] action=upgrade"
}

test_uninstall_plan_preserves_data_without_artifacts() {
  local bundle="${TMP_ROOT}/bundle-uninstall-empty"
  local osr="${TMP_ROOT}/uninstall.os-release"
  mkdir -p "${bundle}"
  make_os_release "${osr}" debian 12
  local out
  out="$(run_dry_action uninstall "${osr}" "${bundle}")"
  assert_contains "${out}" "Action: uninstall"
  assert_contains "${out}" "Data policy: preserve"
  assert_contains "${out}" "systemctl stop holo-control-plane"
  assert_contains "${out}" "Unmounting Holo-VTL storage pools"
  assert_contains "${out}" "Preserving /etc/holo, /var/lib/holo, and /var/log/holo"
  assert_not_contains "${out}" "missing required release artifacts"
}

test_uninstall_purge_plan() {
  local bundle="${TMP_ROOT}/bundle-uninstall-purge"
  local osr="${TMP_ROOT}/uninstall-purge.os-release"
  mkdir -p "${bundle}"
  make_os_release "${osr}" ubuntu 22.04
  local out
  out="$(run_dry_action uninstall "${osr}" "${bundle}" --purge-data)"
  assert_contains "${out}" "Data policy: purge config, data, and logs"
  assert_contains "${out}" "Unmounting Holo-VTL storage pools"
  assert_contains "${out}" "Purging Holo-VTL config, data, and logs"
  assert_contains "${out}" "rm -rf /etc/holo /var/lib/holo /var/log/holo"
}

test_purge_data_rejected_for_install() {
  local bundle="${TMP_ROOT}/bundle-purge-install"
  local osr="${TMP_ROOT}/purge-install.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" ubuntu 22.04
  set +e
  local out
  out="$(run_dry_action install "${osr}" "${bundle}" --purge-data)"
  local code=$?
  set -e
  [[ "${code}" -eq 2 ]] || fail "--purge-data with install should exit 2, got ${code}"
  assert_contains "${out}" "--purge-data is only valid with the uninstall command"
}

test_rocky_plan() {
  local bundle="${TMP_ROOT}/bundle-rocky"
  local osr="${TMP_ROOT}/rocky.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" rocky 9.4
  local out
  out="$(run_dry "${osr}" "${bundle}")"
  assert_contains "${out}" "Detected platform: rocky 9.4 x86_64 (dnf)"
  assert_not_contains "${out}" "centos-release-gluster9"
  assert_contains "${out}" "Runtime packages: kmod sudo targetcli tcmu-runner xfsprogs iscsi-initiator-utils"
  assert_contains "${out}" "TCMU plugin dir: /usr/lib64/tcmu-runner"
}

test_rocky_bundled_tcmu_plan() {
  local bundle="${TMP_ROOT}/bundle-rocky-deps"
  local osr="${TMP_ROOT}/rocky-deps.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" rocky 9.4
  mkdir -p "${bundle}/packages/dnf/el9"
  touch "${bundle}/packages/dnf/el9/tcmu-runner-1.5.4-0.el9.x86_64.rpm"
  touch "${bundle}/packages/dnf/el9/libtcmu-1.5.4-0.el9.x86_64.rpm"
  local out
  out="$(run_dry "${osr}" "${bundle}")"
  assert_contains "${out}" "Bundled dependency dir: ${bundle}/packages"
  assert_contains "${out}" "Runtime packages: kmod sudo targetcli xfsprogs iscsi-initiator-utils"
  assert_contains "${out}" "dnf install -y ${bundle}/packages/dnf/el9/libtcmu-1.5.4-0.el9.x86_64.rpm ${bundle}/packages/dnf/el9/tcmu-runner-1.5.4-0.el9.x86_64.rpm"
  assert_not_contains "${out}" "centos-release-gluster9"
}

test_rocky10_bundled_tcmu_plan() {
  local bundle="${TMP_ROOT}/bundle-rocky10-deps"
  local osr="${TMP_ROOT}/rocky10-deps.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" rocky 10.1
  mkdir -p "${bundle}/packages/dnf/el10"
  touch "${bundle}/packages/dnf/el10/tcmu-runner-1.5.4-0.el10.x86_64.rpm"
  touch "${bundle}/packages/dnf/el10/libtcmu-1.5.4-0.el10.x86_64.rpm"
  local out
  out="$(run_dry "${osr}" "${bundle}")"
  assert_contains "${out}" "Detected platform: rocky 10.1 x86_64 (dnf)"
  assert_contains "${out}" "Runtime packages: kmod sudo kernel-modules-"
  assert_contains "${out}" "targetcli xfsprogs"
  assert_contains "${out}" "[dry-run][verify-command] targetcli (package: targetcli)"
  assert_contains "${out}" "[dry-run][verify-module] target_core_user (package: kernel-modules-"
  assert_contains "${out}" "dnf install -y ${bundle}/packages/dnf/el10/libtcmu-1.5.4-0.el10.x86_64.rpm ${bundle}/packages/dnf/el10/tcmu-runner-1.5.4-0.el10.x86_64.rpm"
}

test_rhel_bundled_tcmu_plan() {
  local bundle="${TMP_ROOT}/bundle-rhel-deps"
  local osr="${TMP_ROOT}/rhel-deps.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" rhel 9.7
  mkdir -p "${bundle}/packages/dnf/el9"
  touch "${bundle}/packages/dnf/el9/tcmu-runner-1.5.4-0.el9.x86_64.rpm"
  touch "${bundle}/packages/dnf/el9/libtcmu-1.5.4-0.el9.x86_64.rpm"
  local out
  out="$(run_dry "${osr}" "${bundle}")"
  assert_contains "${out}" "Detected platform: rhel 9.7 x86_64 (dnf)"
  assert_contains "${out}" "Runtime packages: kmod sudo targetcli xfsprogs iscsi-initiator-utils"
  assert_contains "${out}" "[dry-run] bash -c timeout\\ 30s\\ subscription-manager\\ repos\\ --enable\\ rhel-9-for-\\$\\(uname\\ -m\\)-baseos-rpms\\ --enable\\ rhel-9-for-\\$\\(uname\\ -m\\)-appstream-rpms\\ --enable\\ codeready-builder-for-rhel-9-\\$\\(uname\\ -m\\)-rpms\\ \\|\\|\\ true"
  assert_contains "${out}" "dnf install -y ${bundle}/packages/dnf/el9/libtcmu-1.5.4-0.el9.x86_64.rpm ${bundle}/packages/dnf/el9/tcmu-runner-1.5.4-0.el9.x86_64.rpm"
}

test_sles_plan() {
  local bundle="${TMP_ROOT}/bundle-sles"
  local osr="${TMP_ROOT}/sles.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" sles 15.6
  local out
  out="$(run_dry "${osr}" "${bundle}")"
  assert_contains "${out}" "Detected platform: sles 15.6 x86_64 (zypper)"
  assert_contains "${out}" "Runtime packages: kernel-default kmod sudo xfsprogs util-linux-systemd python3-targetcli-fb tcmu-runner open-iscsi"
  assert_contains "${out}" "[dry-run] zypper -n install kernel-default kmod sudo xfsprogs util-linux-systemd python3-targetcli-fb tcmu-runner open-iscsi"
  assert_contains "${out}" "[dry-run][verify-command] targetcli (package: python3-targetcli-fb)"
}

test_opensuse_leap_plan() {
  local bundle="${TMP_ROOT}/bundle-opensuse"
  local osr="${TMP_ROOT}/opensuse.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" opensuse-leap 15.6
  local out
  out="$(run_dry "${osr}" "${bundle}")"
  assert_contains "${out}" "Detected platform: opensuse-leap 15.6 x86_64 (zypper)"
  assert_contains "${out}" "Runtime packages: kernel-default kmod sudo xfsprogs util-linux-systemd python3-targetcli-fb tcmu-runner open-iscsi"
  assert_contains "${out}" "[dry-run] zypper -n install kernel-default kmod sudo xfsprogs util-linux-systemd python3-targetcli-fb tcmu-runner open-iscsi"
  assert_contains "${out}" "[dry-run][verify-command] targetcli (package: python3-targetcli-fb)"
}

test_optional_package_sets() {
  local bundle="${TMP_ROOT}/bundle-optional"
  local osr="${TMP_ROOT}/optional.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" ubuntu 24.04
  mkdir -p "${bundle}/tcmu-src"
  touch "${bundle}/tcmu-src/tcmu-runner.h"
  printf 'int x;\n' >"${bundle}/handler_holo.c"
  local out
  out="$(run_dry "${osr}" "${bundle}" --with-validation-tools --build-tcmu-plugin --plugin-source-dir "${bundle}/tcmu-src")"
  assert_contains "${out}" "Validation packages: curl jq lsscsi sg3-utils open-iscsi"
  assert_contains "${out}" "Build packages: gcc make pkg-config dpkg-dev"
}

test_strict_storage_rejection() {
  local bundle="${TMP_ROOT}/bundle-strict"
  local osr="${TMP_ROOT}/strict.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" ubuntu 22.04
  set +e
  local out
  out="$(HOLO_STRICT_STORAGE_FLOW=0 HOLO_INSTALL_OS_RELEASE="${osr}" HOLO_INSTALL_UNAME_M=x86_64 bash "${INSTALLER}" --dry-run --bundle-dir "${bundle}" 2>&1)"
  local code=$?
  set -e
  [[ "${code}" -ne 0 ]] || fail "strict storage rejection should fail"
  assert_contains "${out}" "HOLO_STRICT_STORAGE_FLOW=0 is forbidden"
}

test_rejects_unsafe_prefix() {
  local bundle="${TMP_ROOT}/bundle-unsafe-prefix"
  local osr="${TMP_ROOT}/unsafe-prefix.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" ubuntu 22.04
  set +e
  local out
  out="$(run_dry_action install "${osr}" "${bundle}" --prefix '/opt/holo;touch/tmp/pwned')"
  local code=$?
  set -e
  [[ "${code}" -eq 2 ]] || fail "unsafe prefix should exit 2, got ${code}"
  assert_contains "${out}" "--prefix contains unsupported characters"

  set +e
  out="$(run_dry_action install "${osr}" "${bundle}" --prefix '/')"
  code=$?
  set -e
  [[ "${code}" -eq 2 ]] || fail "root prefix should exit 2, got ${code}"
  assert_contains "${out}" "--prefix points at a system root path"

  set +e
  out="$(run_dry_action install "${osr}" "${bundle}" --prefix '/home/holo')"
  code=$?
  set -e
  [[ "${code}" -eq 2 ]] || fail "protected home prefix should exit 2, got ${code}"
  assert_contains "${out}" "--prefix points under a runtime or protected system path"
}

test_rejects_unsafe_http_port() {
  local bundle="${TMP_ROOT}/bundle-unsafe-port"
  local osr="${TMP_ROOT}/unsafe-port.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" ubuntu 22.04
  set +e
  local out
  out="$(HOLO_HTTP_PORT='80;touch/tmp/pwned' HOLO_INSTALL_OS_RELEASE="${osr}" HOLO_INSTALL_UNAME_M=x86_64 bash "${INSTALLER}" install --dry-run --bundle-dir "${bundle}" 2>&1)"
  local code=$?
  set -e
  [[ "${code}" -eq 2 ]] || fail "unsafe HOLO_HTTP_PORT should exit 2, got ${code}"
  assert_contains "${out}" "HOLO_HTTP_PORT must be a numeric TCP port"
}

test_rejects_unsafe_http_bind_addr() {
  local bundle="${TMP_ROOT}/bundle-unsafe-bind"
  local osr="${TMP_ROOT}/unsafe-bind.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" ubuntu 22.04
  set +e
  local out
  out="$(HOLO_HTTP_BIND_ADDR='0.0.0.0;touch/tmp/pwned' HOLO_INSTALL_OS_RELEASE="${osr}" HOLO_INSTALL_UNAME_M=x86_64 bash "${INSTALLER}" install --dry-run --bundle-dir "${bundle}" 2>&1)"
  local code=$?
  set -e
  [[ "${code}" -eq 2 ]] || fail "unsafe HOLO_HTTP_BIND_ADDR should exit 2, got ${code}"
  assert_contains "${out}" "HOLO_HTTP_BIND_ADDR contains unsupported characters"
}

test_missing_artifacts_fail_early() {
  local bundle="${TMP_ROOT}/bundle-missing"
  local osr="${TMP_ROOT}/missing.os-release"
  mkdir -p "${bundle}"
  make_os_release "${osr}" ubuntu 22.04
  set +e
  local out
  out="$(run_dry "${osr}" "${bundle}")"
  local code=$?
  set -e
  [[ "${code}" -ne 0 ]] || fail "missing artifacts should fail"
  assert_contains "${out}" "missing required release artifacts"
  assert_contains "${out}" "${bundle}/control-plane"
  assert_not_contains "${out}" "apt-get install"
}

test_unsupported_platform() {
  local bundle="${TMP_ROOT}/bundle-unsupported"
  local osr="${TMP_ROOT}/unsupported.os-release"
  make_bundle "${bundle}"
  make_os_release "${osr}" fedora 40
  set +e
  local out
  out="$(run_dry "${osr}" "${bundle}")"
  local code=$?
  set -e
  [[ "${code}" -eq 2 ]] || fail "unsupported platform should exit 2, got ${code}"
  assert_contains "${out}" "unsupported platform fedora 40"
}

echo "[test] ubuntu dry-run plan"
test_ubuntu_plan
echo "[test] authenticated public bind"
test_authenticated_public_bind_plan
echo "[test] api key file"
test_api_key_file_plan
echo "[test] explicit no-login public bind stays quiet"
test_explicit_no_login_public_bind_stays_quiet
echo "[test] rocky dry-run plan"
test_rocky_plan
echo "[test] rocky bundled tcmu dry-run plan"
test_rocky_bundled_tcmu_plan
echo "[test] rocky10 bundled tcmu dry-run plan"
test_rocky10_bundled_tcmu_plan
echo "[test] rhel bundled tcmu dry-run plan"
test_rhel_bundled_tcmu_plan
echo "[test] sles dry-run plan"
test_sles_plan
echo "[test] openSUSE Leap dry-run plan"
test_opensuse_leap_plan
echo "[test] optional package sets"
test_optional_package_sets
echo "[test] upgrade dry-run plan"
test_upgrade_plan
echo "[test] uninstall dry-run preserves data"
test_uninstall_plan_preserves_data_without_artifacts
echo "[test] uninstall purge dry-run"
test_uninstall_purge_plan
echo "[test] purge-data rejected for install"
test_purge_data_rejected_for_install
echo "[test] strict storage rejection"
test_strict_storage_rejection
echo "[test] unsafe prefix rejection"
test_rejects_unsafe_prefix
echo "[test] unsafe http port rejection"
test_rejects_unsafe_http_port
echo "[test] unsafe http bind rejection"
test_rejects_unsafe_http_bind_addr
echo "[test] missing artifacts"
test_missing_artifacts_fail_early
echo "[test] unsupported platform"
test_unsupported_platform
echo "[test] installer tests passed"
