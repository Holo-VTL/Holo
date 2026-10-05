#!/usr/bin/env bash
set -euo pipefail

CURRENT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BASELINE_ROOT="${HOLO_TAPE_BENCH_BASELINE_ROOT:-}"
OUT_DIR="${HOLO_TAPE_BENCH_OUT_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/holo-space-guard-bench.XXXXXX")}"
OWN_OUT_DIR=0
[[ -n "${HOLO_TAPE_BENCH_OUT_DIR:-}" ]] || OWN_OUT_DIR=1
RECLAIM_FIXTURE_DIR=""

cleanup() {
  if [[ -n "${RECLAIM_FIXTURE_DIR}" ]]; then rm -rf -- "${RECLAIM_FIXTURE_DIR}"; fi
  if (( OWN_OUT_DIR )); then rm -rf -- "${OUT_DIR}"; fi
}
trap cleanup EXIT

if [[ "${1:-}" == "--reclaim-acceptance" ]]; then
  test_binary="${HOLO_TAPE_RECLAIM_TEST_BINARY:-}"
  scratch_root="${HOLO_TAPE_RECLAIM_TEST_ROOT:-}"
  test_bytes="${HOLO_TAPE_RECLAIM_TEST_BYTES:-53687091200}"
  command -v ionice >/dev/null 2>&1 || { echo "ionice is required to run the reclaim worker with idle I/O priority." >&2; exit 127; }
  [[ -x "${test_binary}" ]] || { echo "Set HOLO_TAPE_RECLAIM_TEST_BINARY to the Linux Rust test binary." >&2; exit 2; }
  [[ -d "${scratch_root}" && ! -L "${scratch_root}" ]] || { echo "Set HOLO_TAPE_RECLAIM_TEST_ROOT to a real directory on a dedicated scratch mount." >&2; exit 2; }
  [[ "${test_bytes}" =~ ^[0-9]+$ ]] || { echo "HOLO_TAPE_RECLAIM_TEST_BYTES must be an integer." >&2; exit 2; }
  scratch_root="$(realpath -e -- "${scratch_root}")"
  [[ "${scratch_root}" != "/" && "${scratch_root}" != /var/lib/holo* ]] || {
    echo "Refusing a root or product-data path; use a dedicated scratch mount." >&2
    exit 2
  }
  mount_info="$(findmnt -n -o SOURCE,FSTYPE,TARGET --target "${scratch_root}")"
  read -r mount_source mount_fstype mount_target <<<"${mount_info}"
  [[ "${mount_target}" != "/" && "${mount_fstype}" =~ ^(xfs|ext4|btrfs)$ ]] || {
    echo "Reclaim acceptance requires a dedicated XFS/ext4/Btrfs mount: ${mount_info}" >&2
    exit 2
  }
  minimum_free=$((test_bytes + 1024 * 1024 * 1024 + 64 * 1024 * 1024))
  available_bytes="$(df -B1 --output=avail "${scratch_root}" | awk 'NR == 2 {print $1}')"
  (( available_bytes >= minimum_free )) || {
    echo "Scratch mount needs ${minimum_free} bytes free; found ${available_bytes}." >&2
    exit 2
  }
  test_name="storage::offline_reclaim_tests::linux_large_reclaim_acceptance"
  "${test_binary}" --list | grep -Fq "${test_name}: test" || {
    echo "Linux test binary does not contain ${test_name}." >&2
    exit 2
  }
  RECLAIM_FIXTURE_DIR="$(mktemp -d "${scratch_root%/}/holo-reclaim-acceptance.XXXXXX")"
  echo "Running ${test_bytes}-byte reclaim acceptance on ${mount_source} (${mount_fstype}, ${mount_target})."
  mkdir -p "${OUT_DIR}"
  HOLO_TAPE_TEST_ROOT="${RECLAIM_FIXTURE_DIR}" \
    HOLO_TAPE_RECLAIM_TEST_BYTES="${test_bytes}" \
    "${test_binary}" "${test_name}" --exact --ignored --nocapture \
    2>&1 | tee "${OUT_DIR}/reclaim-acceptance.log"
  echo "Large-payload tape reclaim acceptance passed."
  exit 0
fi

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  echo "Usage: tape_space_guard.sh [--reclaim-acceptance]"
  echo "Default mode compares the bounded single/dual-drive write path."
  echo "--reclaim-acceptance runs the ignored Linux real-allocation reclaim test; set HOLO_TAPE_RECLAIM_TEST_BINARY and HOLO_TAPE_RECLAIM_TEST_ROOT."
  exit 0
fi

if [[ -z "${BASELINE_ROOT}" ]]; then
  command -v git >/dev/null 2>&1 || { echo "git is required to create the baseline snapshot." >&2; exit 2; }
  BASELINE_ROOT="${OUT_DIR}/baseline"
  mkdir -p "${BASELINE_ROOT}"
  git -C "${CURRENT_ROOT}" archive HEAD | tar -x -C "${BASELINE_ROOT}"
  mkdir -p "${BASELINE_ROOT}/data-plane/tests"
  cp "${CURRENT_ROOT}/data-plane/tests/tape_space_guard_perf.rs" "${BASELINE_ROOT}/data-plane/tests/"
  if [[ -d "${CURRENT_ROOT}/data-plane/.cargo" ]]; then
    cp -a "${CURRENT_ROOT}/data-plane/.cargo" "${BASELINE_ROOT}/data-plane/"
  fi
fi
[[ -f "${BASELINE_ROOT}/data-plane/Cargo.toml" ]] || { echo "Baseline Rust project not found: ${BASELINE_ROOT}" >&2; exit 2; }
[[ -f "${CURRENT_ROOT}/data-plane/tests/tape_space_guard_perf.rs" ]] || { echo "Performance harness source is missing." >&2; exit 2; }
command -v cargo >/dev/null 2>&1 || { echo "cargo is required." >&2; exit 127; }

run_benchmark() {
  local label="$1"
  local project="$2"
  local test_name="$3"
  local root="${OUT_DIR}/${label}-data"
  local output="${OUT_DIR}/${label}.log"
  (
    cd "${project}/data-plane"
    HOLO_TAPE_BENCH_ROOT="${root}" \
      cargo test --locked --release \
      --test tape_space_guard_perf "${test_name}" -- --ignored --exact --nocapture
  ) >"${output}" 2>&1 || { cat "${output}"; return 1; }
  cat "${output}"
}

mkdir -p "${OUT_DIR}"
run_benchmark baseline-single "${BASELINE_ROOT}" tape_write_path_benchmark
run_benchmark current-single "${CURRENT_ROOT}" tape_write_path_benchmark
run_benchmark baseline-dual "${BASELINE_ROOT}" tape_dual_write_path_benchmark
run_benchmark current-dual "${CURRENT_ROOT}" tape_dual_write_path_benchmark

python3 - "${OUT_DIR}" <<'PY'
import re
import statistics
import sys
from pathlib import Path

out_dir = Path(sys.argv[1])

def median_elapsed(path, marker):
    pattern = re.compile(rf"{marker} round=(\d+) ops=\d+ bytes=\d+ elapsed_ns=(\d+)")
    with open(path, encoding="utf-8") as handle:
        samples = [(int(round_id), int(elapsed)) for round_id, elapsed in pattern.findall(handle.read())]
    samples = [elapsed for round_id, elapsed in samples if round_id > 0]
    if len(samples) != 4:
        raise SystemExit(f"expected four measured rounds after warm-up in {path}, found {len(samples)}")
    return statistics.median(samples)

results = []
for mode, marker in (("single", "TAPE_SPACE_GUARD_BENCH"), ("dual", "TAPE_SPACE_GUARD_DUAL_BENCH")):
    baseline = median_elapsed(out_dir / f"baseline-{mode}.log", marker)
    current = median_elapsed(out_dir / f"current-{mode}.log", marker)
    regression = (current / baseline) - 1
    results.append((mode, baseline, current, regression))
    print(f"{mode} drive baseline median: {baseline / 1e6:.2f} ms")
    print(f"{mode} drive current median:  {current / 1e6:.2f} ms")
    print(f"{mode} drive latency change:  {regression * 100:+.2f}%")
if any(regression > 0.10 for _, _, _, regression in results):
    raise SystemExit("space-guard write-path regression exceeds the 10% acceptance limit")
PY
