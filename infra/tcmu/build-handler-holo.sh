#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUTPUT_SO="${SCRIPT_DIR}/handler_holo.so"
SRC_DIR="${TCMU_SOURCE_DIR:-}"
TEMP_CACHE_DIR=""
CACHE_DIR="${TCMU_SOURCE_CACHE_DIR:-}"

if [[ -z "${SRC_DIR}" ]]; then
  if [[ -z "${CACHE_DIR}" ]]; then
    TEMP_CACHE_DIR="$(mktemp -d /tmp/holo-tcmu-source.XXXXXX)"
    CACHE_DIR="${TEMP_CACHE_DIR}"
    trap '[[ -z "${TEMP_CACHE_DIR}" ]] || rm -rf "${TEMP_CACHE_DIR}"' EXIT
  fi
  mkdir -p "${CACHE_DIR}"
  if [[ ! -d "${CACHE_DIR}/tcmu-1.5.4" ]]; then
    pushd "${CACHE_DIR}" >/dev/null
    if ! ls tcmu-* >/dev/null 2>&1; then
      apt-get source -qq tcmu-runner
    fi
    popd >/dev/null
  fi

  SRC_DIR="$(find "${CACHE_DIR}" -maxdepth 1 -type d -name 'tcmu-*' | sort | tail -n 1)"
fi

if [[ -z "${SRC_DIR}" || ! -f "${SRC_DIR}/tcmu-runner.h" ]]; then
  echo "ERROR: tcmu-runner source not found. Set TCMU_SOURCE_DIR or run: apt-get source tcmu-runner" >&2
  exit 1
fi

echo "[build] source: ${SRC_DIR}"
echo "[build] output: ${OUTPUT_SO}"

gcc \
  -std=gnu11 \
  -O2 \
  -fPIC \
  -shared \
  -Wall \
  -Wextra \
  -I"${SRC_DIR}" \
  -I"${SRC_DIR}/ccan" \
  -I"${SRC_DIR}/ccan/ccan" \
  "${SCRIPT_DIR}/handler_holo.c" \
  -o "${OUTPUT_SO}"

echo "[ok] built ${OUTPUT_SO}"
