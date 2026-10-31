#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
: "${TCMU_SOURCE_DIR:?Set TCMU_SOURCE_DIR to tcmu-runner source headers}"
bin=$(mktemp)
trap 'rm -f "$bin"' EXIT
"${CC:-cc}" -std=gnu11 -O2 -Wall -Wextra -Werror \
  -ffunction-sections -fdata-sections \
  -I"${TCMU_SOURCE_DIR}" -I"${TCMU_SOURCE_DIR}/ccan" -I"${TCMU_SOURCE_DIR}/ccan/ccan" \
  -pthread "${root}/tests/read_len_test.c" -Wl,--gc-sections -o "$bin"
"$bin"
