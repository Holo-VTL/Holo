#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT_DIR}/data-plane"
IMAGE_BYTES=$((256 * 1024 * 1024))
MIN_FREE_BYTES=$((512 * 1024 * 1024))
SCRATCH_BASE="${HOLO_TAPE_TEST_SCRATCH_BASE:-${TMPDIR:-/var/tmp}}"

if [[ "$(uname -s)" != Linux ]]; then
  echo "This smoke test requires Linux." >&2
  exit 2
fi
for cmd in cargo findmnt losetup mkfs.ext4 mountpoint df fallocate; do
  command -v "${cmd}" >/dev/null 2>&1 || { echo "Missing required command: ${cmd}" >&2; exit 2; }
done
sudo -n true || { echo "Passwordless sudo is required for the temporary loopback mount." >&2; exit 2; }
mkdir -p "${SCRATCH_BASE}"
available_kib="$(df -Pk "${SCRATCH_BASE}" | awk 'NR == 2 {print $4}')"
(( available_kib * 1024 >= MIN_FREE_BYTES + IMAGE_BYTES )) || {
  echo "Scratch filesystem needs at least 768 MiB free; refusing to create the fixture." >&2
  exit 2
}

BASE_DIR="$(mktemp -d "${SCRATCH_BASE%/}/holo-space-reclaim.XXXXXX")"
IMAGE_PATH="${BASE_DIR}/fixture.ext4"
MOUNT_PATH="${BASE_DIR}/mount"
LOOP_DEVICE=""
MOUNTED=0
cleanup() {
  if (( MOUNTED )); then sudo umount "${MOUNT_PATH}" || true; fi
  if [[ -n "${LOOP_DEVICE}" ]]; then sudo losetup -d "${LOOP_DEVICE}" || true; fi
  rm -rf -- "${BASE_DIR}"
}
trap cleanup EXIT

mkdir -p "${MOUNT_PATH}"
fallocate -l "${IMAGE_BYTES}" "${IMAGE_PATH}"
LOOP_DEVICE="$(sudo losetup --find --show "${IMAGE_PATH}")"
sudo mkfs.ext4 -q -F -m 0 "${LOOP_DEVICE}"
sudo mount "${LOOP_DEVICE}" "${MOUNT_PATH}"
MOUNTED=1
sudo chown "$(id -u):$(id -g)" "${MOUNT_PATH}"

mount_info="$(findmnt -n -o SOURCE,FSTYPE,TARGET --target "${MOUNT_PATH}")"
read -r source fstype target <<<"${mount_info}"
[[ "${source}" == "${LOOP_DEVICE}" && "${fstype}" == ext4 && "${target}" == "${MOUNT_PATH}" ]] || {
  echo "Fixture is not mounted from the expected isolated loop device: ${mount_info}" >&2
  exit 1
}

echo "Running the reclaim smoke test on ${source} (${fstype}, ${IMAGE_BYTES} bytes)."
HOLO_TAPE_TEST_ROOT="${MOUNT_PATH}" \
  cargo test --locked --lib \
  storage::offline_reclaim_tests::linux_small_filesystem_reclaim_smoke \
  -- --ignored --exact --nocapture
echo "Small-filesystem tape reclaim smoke test passed."
