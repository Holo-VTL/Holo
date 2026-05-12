#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
allow_missing="${HOLO_STATIC_ANALYSIS_ALLOW_MISSING:-0}"

require_tool() {
  local name="$1"
  if command -v "${name}" >/dev/null 2>&1; then
    return 0
  fi
  if [[ "${allow_missing}" == "1" ]]; then
    echo "[static-analysis] SKIP: ${name} is not installed"
    return 1
  fi
  echo "[static-analysis] ERROR: ${name} is required" >&2
  return 2
}

run_shellcheck() {
  scripts=()
  while IFS= read -r script; do
    scripts+=("${script}")
  done < <(find "${repo_root}/scripts" "${repo_root}/tests" "${repo_root}/infra" -type f -name '*.sh' -print | sort)
  if [[ "${#scripts[@]}" -eq 0 ]]; then
    echo "[static-analysis] SKIP: no shell scripts found"
    return
  fi
  require_tool shellcheck || {
    [[ "${allow_missing}" == "1" ]] && return 0
    return 2
  }
  shellcheck "${scripts[@]}"
}

run_govulncheck() {
  require_tool govulncheck || {
    [[ "${allow_missing}" == "1" ]] && return 0
    return 2
  }
  (cd "${repo_root}/control-plane" && govulncheck ./...)
}

run_cargo_audit() {
  require_tool cargo-audit || {
    [[ "${allow_missing}" == "1" ]] && return 0
    return 2
  }
  (cd "${repo_root}/data-plane" && cargo audit)
}

run_hadolint() {
  dockerfiles=()
  while IFS= read -r dockerfile; do
    dockerfiles+=("${dockerfile}")
  done < <(find "${repo_root}" -type f -name 'Dockerfile*' -print | sort)
  if [[ "${#dockerfiles[@]}" -eq 0 ]]; then
    echo "[static-analysis] SKIP: no Dockerfiles found"
    return
  fi
  require_tool hadolint || {
    [[ "${allow_missing}" == "1" ]] && return 0
    return 2
  }
  hadolint "${dockerfiles[@]}"
}

run_cargo_deny() {
  if [[ ! -f "${repo_root}/data-plane/deny.toml" && ! -f "${repo_root}/deny.toml" ]]; then
    echo "[static-analysis] SKIP: cargo-deny config is not present"
    return
  fi
  require_tool cargo-deny || {
    [[ "${allow_missing}" == "1" ]] && return 0
    return 2
  }
  (cd "${repo_root}/data-plane" && cargo deny check)
}

run_shellcheck
run_govulncheck
run_cargo_audit
run_hadolint
run_cargo_deny

echo "[static-analysis] All enabled checks passed"
