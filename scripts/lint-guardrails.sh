#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

fail=0

fail_block() {
  local title="$1"
  shift
  echo "[guardrail] FAIL: ${title}" >&2
  if "$@"; then
    true
  fi
  fail=1
}

pass_line() {
  echo "[guardrail] PASS: $1"
}

# 1) API key default must remain empty so product installs use internal no-login mode unless explicitly configured.
if rg -n 'APIKey:\s*getenv\("HOLO_API_KEY",\s*""\)' control-plane/internal/config/config.go >/dev/null; then
  pass_line "HOLO_API_KEY default is empty for internal no-login mode"
else
  echo "[guardrail] FAIL: HOLO_API_KEY default must be empty string" >&2
  rg -n 'APIKey:\s*getenv\("HOLO_API_KEY"' control-plane/internal/config/config.go || true
  fail=1
fi

# 2) No direct request-body decoder in production API handlers
if rg -n 'json\.NewDecoder\(r\.Body\)' control-plane/internal/api --glob '*.go' --glob '!**/*_test.go' >/dev/null; then
  fail_block "Do not use json.NewDecoder(r.Body) directly in API handlers" rg -n 'json\.NewDecoder\(r\.Body\)' control-plane/internal/api --glob '*.go' --glob '!**/*_test.go'
else
  pass_line "No direct json.NewDecoder(r.Body) in API handlers"
fi

# 3) No raw err.Error leakage in HTTP responses
if rg -n 'http\.Error\(w,\s*err\.Error\(\)' control-plane/internal/api --glob '*.go' --glob '!**/*_test.go' >/dev/null; then
  fail_block "Do not leak err.Error() via http.Error" rg -n 'http\.Error\(w,\s*err\.Error\(\)' control-plane/internal/api --glob '*.go' --glob '!**/*_test.go'
else
  pass_line "No raw err.Error() leakage in API handlers"
fi

# 4) No silent audit write drops in production code
if rg -n '_ = .*\b(writer|auditW|auditWriter|memW|h\.writer)\.Write\(' control-plane/internal --glob '*.go' --glob '!**/*_test.go' >/dev/null; then
  fail_block "Do not silently drop audit write errors" rg -n '_ = .*\b(writer|auditW|auditWriter|memW|h\.writer)\.Write\(' control-plane/internal --glob '*.go' --glob '!**/*_test.go'
else
  pass_line "No silent audit write drops in production paths"
fi

if [[ ${fail} -ne 0 ]]; then
  exit 1
fi

echo "[guardrail] All checks passed"
