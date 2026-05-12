#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

if ! command -v rg >/dev/null 2>&1; then
  echo "ERROR: ripgrep (rg) is required" >&2
  exit 2
fi

BASELINE_PRODUCTION=233
PATTERN=' as u(8|16|32|64|size)\b'

count_matches() {
  local output
  output="$("$@" | wc -l | tr -d ' ')"
  printf '%s' "${output:-0}"
}

total="$(count_matches rg -n "${PATTERN}" data-plane/src --type rust)"
production="$(count_matches rg -n "${PATTERN}" data-plane/src --type rust --glob '!**/*_tests.rs' --glob '!**/test_utils.rs')"
test_only="$(count_matches rg -n "${PATTERN}" data-plane/src --type rust --glob '*_tests.rs' --glob '**/test_utils.rs')"

cat <<REPORT
# scan-rust-casts.sh report (data-plane)
total                                      : ${total}
production (excl. *_tests.rs, test_utils.rs): ${production}
test-only                                  : ${test_only}
production baseline                        : ${BASELINE_PRODUCTION}
REPORT

if (( production > BASELINE_PRODUCTION )); then
  echo "baseline check: production ${production} > ${BASELINE_PRODUCTION} FAIL" >&2
  rg -n "${PATTERN}" data-plane/src --type rust --glob '!**/*_tests.rs' --glob '!**/test_utils.rs' >&2 || true
  exit 1
fi

echo "baseline check: production ${production} <= ${BASELINE_PRODUCTION} PASS"
