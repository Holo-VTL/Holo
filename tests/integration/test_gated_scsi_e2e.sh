#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="${ROOT_DIR}/scripts/gated-scsi-e2e.sh"

assert_contains() {
  local haystack="$1"
  local needle="$2"
  if [[ "${haystack}" != *"${needle}"* ]]; then
    printf 'expected output to contain %q\noutput:\n%s\n' "${needle}" "${haystack}" >&2
    exit 1
  fi
}

run_expect() {
  local expected="$1"
  shift
  set +e
  output="$("$@" 2>&1)"
  status=$?
  set -e
  if [[ ${status} -ne ${expected} ]]; then
    printf 'expected exit %s got %s\ncommand: %s\noutput:\n%s\n' "${expected}" "${status}" "$*" "${output}" >&2
    exit 1
  fi
  printf '%s' "${output}"
}

make_fake_bin() {
  local dir="$1"
  shift
  for name in "$@"; do
    cat >"${dir}/${name}" <<'FAKE'
#!/usr/bin/env bash
exit 0
FAKE
    chmod +x "${dir}/${name}"
  done
}

test_requires_explicit_opt_in() {
  local out
  out="$(run_expect 2 bash "${SCRIPT}" --portal 127.0.0.1:3260 --iqn iqn.2026-04.local.holo:test)"
  assert_contains "${out}" "HOLO_RUN_PRIVILEGED_SCSI_E2E=1"
}

test_check_only_passes_with_fake_linux_root_prereqs() {
  local tmp out
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' RETURN
  make_fake_bin "${tmp}" iscsiadm sg_inq sg_readcap findmnt lsblk
  out="$(run_expect 0 env PATH="${tmp}:${PATH}" HOLO_SCSI_E2E_UNAME_OVERRIDE=Linux HOLO_SCSI_E2E_EUID_OVERRIDE=0 /bin/bash "${SCRIPT}" --check-only --portal 127.0.0.1:3260 --iqn iqn.2026-04.local.holo:test)"
  assert_contains "${out}" "check-only complete"
}

test_check_only_reports_missing_prereq() {
  local tmp out
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' RETURN
  make_fake_bin "${tmp}" iscsiadm sg_inq findmnt lsblk
  out="$(run_expect 2 env PATH="${tmp}" HOLO_SCSI_E2E_UNAME_OVERRIDE=Linux HOLO_SCSI_E2E_EUID_OVERRIDE=0 /bin/bash "${SCRIPT}" --check-only --portal 127.0.0.1:3260 --iqn iqn.2026-04.local.holo:test)"
  assert_contains "${out}" "missing required command: sg_readcap"
}

test_requires_explicit_opt_in
test_check_only_passes_with_fake_linux_root_prereqs
test_check_only_reports_missing_prereq

printf 'gated scsi e2e shell tests passed\n'
