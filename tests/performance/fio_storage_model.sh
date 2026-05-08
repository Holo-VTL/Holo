#!/usr/bin/env bash
set -euo pipefail

OUT_DIR="${OUT_DIR:-/tmp/holo-fio-results-$(date -u +%Y%m%dT%H%M%SZ)}"
RUNTIME_SEC="${RUNTIME_SEC:-30}"
SIZE="${SIZE:-4G}"
SSD_PATH="${SSD_PATH:-}"
HDD_PATH="${HDD_PATH:-}"

usage() {
  cat <<'EOF'
Usage:
  fio_storage_model.sh --ssd-path <path> --hdd-path <path> [--out-dir <dir>]

Environment:
  RUNTIME_SEC  Per-test runtime in seconds (default: 30)
  SIZE         Per-test file size (default: 4G)
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --ssd-path)
      SSD_PATH="$2"
      shift 2
      ;;
    --hdd-path)
      HDD_PATH="$2"
      shift 2
      ;;
    --out-dir)
      OUT_DIR="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

[[ -n "${SSD_PATH}" && -n "${HDD_PATH}" ]] || {
  usage >&2
  exit 2
}
command -v fio >/dev/null 2>&1 || {
  echo "fio not found" >&2
  exit 127
}

mkdir -p "${OUT_DIR}" "${SSD_PATH}" "${HDD_PATH}"

SUMMARY="${OUT_DIR}/summary.tsv"
printf 'target\ttest\tread_mib_s\twrite_mib_s\tread_iops\twrite_iops\tp99_read_ms\tp99_write_ms\n' >"${SUMMARY}"

run_case() {
  local target="$1"
  local base="$2"
  local name="$3"
  local rw="$4"
  local bs="$5"
  local iodepth="$6"
  local extra="$7"
  local file="${base}/fio-${name}.dat"
  local json="${OUT_DIR}/${target}-${name}.json"

  fio \
    --name="${target}-${name}" \
    --filename="${file}" \
    --rw="${rw}" \
    --bs="${bs}" \
    --iodepth="${iodepth}" \
    --numjobs=1 \
    --size="${SIZE}" \
    --time_based=1 \
    --runtime="${RUNTIME_SEC}" \
    --direct=1 \
    --ioengine=libaio \
    --group_reporting=1 \
    --output-format=json \
    ${extra} \
    --output="${json}"

  python3 - "${json}" "${target}" "${name}" >>"${SUMMARY}" <<'PY'
import json
import sys

path, target, name = sys.argv[1:4]
with open(path, "r", encoding="utf-8") as f:
    data = json.load(f)
job = data["jobs"][0]

def bw_mib(op):
    return op.get("bw_bytes", 0) / 1024 / 1024

def p99_ms(op):
    pct = op.get("clat_ns", {}).get("percentile", {})
    return pct.get("99.000000", 0) / 1_000_000

read = job.get("read", {})
write = job.get("write", {})
print(
    f"{target}\t{name}\t{bw_mib(read):.2f}\t{bw_mib(write):.2f}\t"
    f"{read.get('iops', 0):.2f}\t{write.get('iops', 0):.2f}\t"
    f"{p99_ms(read):.3f}\t{p99_ms(write):.3f}"
)
PY

  rm -f "${file}"
}

for target in ssd hdd; do
  if [[ "${target}" == "ssd" ]]; then
    base="${SSD_PATH}"
  else
    base="${HDD_PATH}"
  fi
  mkdir -p "${base}"
  run_case "${target}" "${base}" seqwrite-1m write 1m 16 "--fsync_on_close=1"
  run_case "${target}" "${base}" randwrite-64k randwrite 64k 16 "--fsync_on_close=1"
  run_case "${target}" "${base}" randread-64k randread 64k 16 ""
  run_case "${target}" "${base}" syncwrite-1m write 1m 1 "--fdatasync=1"
done

cat "${SUMMARY}"
echo "results: ${OUT_DIR}"
