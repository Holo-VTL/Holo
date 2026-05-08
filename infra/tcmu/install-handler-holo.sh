#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HANDLER_SO="${SCRIPT_DIR}/handler_holo.so"
VERIFY_TARGETCLI="${VERIFY_TARGETCLI:-1}"

run_root() {
  if [[ "${EUID}" -eq 0 ]]; then
    "$@"
  else
    sudo "$@"
  fi
}

if [[ ! -f "${HANDLER_SO}" ]]; then
  "${SCRIPT_DIR}/build-handler-holo.sh"
fi

if command -v dpkg-architecture >/dev/null 2>&1; then
  MULTIARCH="$(dpkg-architecture -qDEB_HOST_MULTIARCH)"
else
  MULTIARCH="x86_64-linux-gnu"
fi
PLUGIN_DIR="/usr/lib/${MULTIARCH}/tcmu-runner"
PLUGIN_SO="${PLUGIN_DIR}/handler_holo.so"

echo "[install] plugin dir: ${PLUGIN_DIR}"
run_root mkdir -p "${PLUGIN_DIR}"
run_root install -m 0644 "${HANDLER_SO}" "${PLUGIN_SO}"

echo "[install] restarting tcmu-runner"
run_root systemctl restart tcmu-runner
run_root systemctl --no-pager --full status tcmu-runner | sed -n '1,12p'

if [[ "${VERIFY_TARGETCLI}" == "1" ]]; then
  echo "[verify] checking available user backstores"
  BACKSTORES_OUTPUT="$(run_root targetcli /backstores ls 2>/dev/null || true)"
  printf '%s\n' "${BACKSTORES_OUTPUT}"
  if grep -q "user:holo" <<<"${BACKSTORES_OUTPUT}"; then
    echo "[ok] user:holo handler registered"
  else
    echo "[warn] user:holo not visible in targetcli /backstores ls" >&2
    exit 1
  fi
fi
