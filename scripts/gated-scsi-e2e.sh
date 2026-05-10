#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: bash scripts/gated-scsi-e2e.sh [--check-only] --portal HOST[:PORT] --iqn IQN [--write-read-smoke]

Runs a gated real SCSI E2E smoke on a Linux privileged host. The mutating path
requires HOLO_RUN_PRIVILEGED_SCSI_E2E=1.
USAGE
}

log() {
  printf '[e2e] %s\n' "$*"
}

skip() {
  printf '[e2e][skip] %s\n' "$*" >&2
  exit 2
}

fail() {
  printf '[e2e][fail] %s\n' "$*" >&2
  exit 1
}

run() {
  printf '[e2e][run] %s\n' "$*"
  "$@"
}

check_only=0
write_read_smoke=0
portal=""
iqn=""
runtime_dir=""
logged_in=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --check-only)
      check_only=1
      shift
      ;;
    --write-read-smoke)
      write_read_smoke=1
      shift
      ;;
    --portal)
      [[ $# -ge 2 ]] || fail "--portal requires HOST[:PORT]"
      portal="$2"
      shift 2
      ;;
    --iqn)
      [[ $# -ge 2 ]] || fail "--iqn requires a target IQN"
      iqn="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ -n "${portal}" ]] || fail "--portal is required"
[[ -n "${iqn}" ]] || fail "--iqn is required"

host="${portal%%:*}"
port="${portal#*:}"
if [[ "${host}" == "${port}" ]]; then
  port="3260"
fi

uname_value="${HOLO_SCSI_E2E_UNAME_OVERRIDE:-$(uname -s)}"
euid_value="${HOLO_SCSI_E2E_EUID_OVERRIDE:-${EUID}}"

cleanup() {
  local rc=$?
  if [[ ${logged_in} -eq 1 ]]; then
    printf '[e2e][cleanup] logout %s from %s:%s\n' "${iqn}" "${host}" "${port}" >&2
    iscsiadm -m node -T "${iqn}" -p "${host}:${port}" --logout >/dev/null 2>&1 || true
  fi
  if [[ -n "${runtime_dir}" && -d "${runtime_dir}" ]]; then
    printf '[e2e][cleanup] remove %s\n' "${runtime_dir}" >&2
    rm -rf "${runtime_dir}"
  fi
  exit "${rc}"
}
trap cleanup EXIT INT TERM

require_command() {
  command -v "$1" >/dev/null 2>&1 || skip "missing required command: $1"
}

if [[ ${check_only} -eq 0 && "${HOLO_RUN_PRIVILEGED_SCSI_E2E:-}" != "1" ]]; then
  skip "set HOLO_RUN_PRIVILEGED_SCSI_E2E=1 to allow privileged iSCSI host mutation"
fi

[[ "${uname_value}" == "Linux" ]] || skip "real SCSI E2E requires Linux; found ${uname_value}"
[[ "${euid_value}" == "0" ]] || skip "real SCSI E2E requires root privileges"

for cmd in iscsiadm sg_inq sg_readcap findmnt lsblk; do
  require_command "${cmd}"
done

if [[ ${write_read_smoke} -eq 1 ]]; then
  if ! command -v sg_dd >/dev/null 2>&1 && ! command -v dd >/dev/null 2>&1; then
    skip "write-read smoke requires sg_dd or dd"
  fi
fi

if [[ ${check_only} -eq 1 ]]; then
  log "check-only complete for ${iqn} at ${host}:${port}"
  exit 0
fi

runtime_dir="$(mktemp -d "${TMPDIR:-/tmp}/holo-scsi-e2e.XXXXXX")"
log "runtime evidence directory: ${runtime_dir}"

run iscsiadm -m discovery -t sendtargets -p "${host}:${port}"
run iscsiadm -m node -T "${iqn}" -p "${host}:${port}" --login
logged_in=1

sleep "${HOLO_SCSI_E2E_SETTLE_SECONDS:-2}"

device=""
for candidate in /dev/tape/by-id/* /dev/sg* /dev/st*; do
  [[ -e "${candidate}" ]] || continue
  if sg_inq "${candidate}" >"${runtime_dir}/inq.$(basename "${candidate}").txt" 2>/dev/null; then
    device="${candidate}"
    break
  fi
done

[[ -n "${device}" ]] || fail "no SCSI tape/generic device responded after login"
log "selected device: ${device}"

run sg_inq "${device}" | tee "${runtime_dir}/sg_inq.txt"
run sg_readcap "${device}" | tee "${runtime_dir}/sg_readcap.txt"

if [[ ${write_read_smoke} -eq 1 ]]; then
  smoke_payload="${runtime_dir}/smoke-write.bin"
  smoke_readback="${runtime_dir}/smoke-read.bin"
  printf 'holo-scsi-e2e-smoke\n' >"${smoke_payload}"
  if command -v sg_dd >/dev/null 2>&1; then
    run sg_dd "if=${smoke_payload}" "of=${device}" bs=512 count=1
    run sg_dd "if=${device}" "of=${smoke_readback}" bs=512 count=1
  else
    run dd "if=${smoke_payload}" "of=${device}" bs=512 count=1 conv=fsync
    run dd "if=${device}" "of=${smoke_readback}" bs=512 count=1
  fi
fi

log "real SCSI E2E completed for ${iqn}"
