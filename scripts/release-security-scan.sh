#!/usr/bin/env bash
set -euo pipefail

repo_root=""
tarball=""
scanner=""
evidence=""
source_revision=""
expected_tar_sha256=""
go_version="1.26.8"
scanner_version="v1.8.0"
goos="linux"
goarch="amd64"
cgo="0"
main_module="github.com/Holo-VTL/Holo/control-plane/cmd/api"

usage() {
  echo "Usage: release-security-scan.sh --repo-root PATH --tarball PATH --scanner PATH --evidence PATH --source-revision REVISION"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo-root) repo_root="$2"; shift 2 ;;
    --tarball) tarball="$2"; shift 2 ;;
    --scanner) scanner="$2"; shift 2 ;;
    --evidence) evidence="$2"; shift 2 ;;
    --source-revision) source_revision="$2"; shift 2 ;;
    --expected-tar-sha256) expected_tar_sha256="$2"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) echo "[release-security] unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ -z "$repo_root" || -z "$tarball" || -z "$scanner" || -z "$evidence" || -z "$source_revision" ]]; then
  usage >&2
  exit 2
fi
repo_root="$(cd "$repo_root" && pwd)"
[[ -f "$tarball" && -x "$scanner" ]] || { echo "[release-security] tarball or scanner is unavailable" >&2; exit 1; }
command -v go >/dev/null 2>&1 || { echo "[release-security] Go is required" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "[release-security] Python 3 is required" >&2; exit 1; }

export GOTOOLCHAIN=local
go_actual="$(go version)"
[[ "$go_actual" == *"go${go_version}"* ]] || {
  echo "[release-security] expected Go ${go_version}; found ${go_actual}" >&2
  exit 1
}
scanner_info="$("$scanner" -version 2>&1)" || {
  echo "[release-security] pinned govulncheck could not report its version" >&2
  exit 1
}
[[ "$scanner_info" == *"${scanner_version}"* ]] || {
  echo "[release-security] expected govulncheck ${scanner_version}" >&2
  exit 1
}

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
source_summary="$work_dir/source-summary.json"
binary_summary="$work_dir/binary-summary.json"
build_info="$work_dir/control-plane.buildinfo"

# Reject absolute paths and parent traversal before extracting the candidate package.
tar_listing="$(tar -tzf "$tarball")" || { echo "[release-security] package archive cannot be read" >&2; exit 1; }
while IFS= read -r member; do
  [[ "$member" != /* && "/$member/" != *"/../"* ]] || {
    echo "[release-security] package contains an unsafe path" >&2
    exit 1
  }
done <<< "$tar_listing"
tar -xzf "$tarball" -C "$work_dir"
control_plane=""
binary_count=0
while IFS= read -r binary_path; do
  control_plane="$binary_path"
  binary_count=$((binary_count + 1))
done < <(find "$work_dir" -type f -path '*/control-plane' -print)
[[ $binary_count -eq 1 ]] || { echo "[release-security] package must contain one control-plane binary" >&2; exit 1; }

go version -m "$control_plane" >"$build_info" || {
  echo "[release-security] Go build information is unavailable in the packaged binary" >&2
  exit 1
}
python3 "$repo_root/scripts/verify-go-buildinfo.py" \
  --input "$build_info" \
  --go-version "$go_version" \
  --goos "$goos" \
  --goarch "$goarch" \
  --cgo "$cgo" \
  --module "$main_module" \
  --output "$work_dir/build-summary.json"

echo "[release-security] scanning source with ${go_actual}"
if ! (cd "$repo_root/control-plane" && env GOTOOLCHAIN=local GOOS="$goos" GOARCH="$goarch" CGO_ENABLED="$cgo" "$scanner" -json ./...) \
  >"$work_dir/source.jsonl" 2>"$work_dir/source.stderr"; then
  echo "[release-security] source scan failed; scanner or vulnerability database may be unavailable" >&2
  exit 1
fi
if [[ -s "$work_dir/source.stderr" ]]; then
  echo "[release-security] source scan emitted diagnostics; scanner or vulnerability database may be unavailable" >&2
  exit 1
fi
python3 "$repo_root/scripts/verify-govulncheck-json.py" \
  --input "$work_dir/source.jsonl" --mode source --go-version "$go_version" \
  --scanner-version "$scanner_version" --goos "$goos" --goarch "$goarch" --output "$source_summary"

echo "[release-security] scanning packaged binary"
if ! env GOTOOLCHAIN=local GOOS="$goos" GOARCH="$goarch" CGO_ENABLED="$cgo" \
  "$scanner" -mode=binary -json "$control_plane" \
  >"$work_dir/binary.jsonl" 2>"$work_dir/binary.stderr"; then
  echo "[release-security] packaged binary scan failed; scanner or vulnerability database may be unavailable" >&2
  exit 1
fi
if [[ -s "$work_dir/binary.stderr" ]]; then
  echo "[release-security] packaged binary scan emitted diagnostics; scanner or vulnerability database may be unavailable" >&2
  exit 1
fi
python3 "$repo_root/scripts/verify-govulncheck-json.py" \
  --input "$work_dir/binary.jsonl" --mode binary --go-version "$go_version" \
  --scanner-version "$scanner_version" --goos "$goos" --goarch "$goarch" --output "$binary_summary"

tar_sha="$(sha256sum "$tarball" | awk '{print $1}')"
if [[ -n "$expected_tar_sha256" && "$tar_sha" != "$expected_tar_sha256" ]]; then
  "$repo_root/scripts/verify-sha256.sh" "$tarball" "$expected_tar_sha256" >/dev/null
fi
binary_sha="$(sha256sum "$control_plane" | awk '{print $1}')"
source_tree_sha="$(python3 - "$repo_root" <<'PY'
import hashlib
import os
import sys

root = os.path.realpath(sys.argv[1])
excluded_dirs = {".git", "target", "node_modules", "bin", "output", "package-root", "release", "dist"}
digest = hashlib.sha256()
for current, dirs, files in os.walk(root):
    dirs[:] = sorted(name for name in dirs if name not in excluded_dirs)
    for name in sorted(files):
        if name == ".DS_Store" or name.endswith((".tar.gz", ".security.json")):
            continue
        path = os.path.join(current, name)
        rel = os.path.relpath(path, root).replace(os.sep, "/").encode()
        digest.update(len(rel).to_bytes(4, "big"))
        digest.update(rel)
        if os.path.islink(path):
            content = os.readlink(path).encode()
        else:
            with open(path, "rb") as source:
                content = source.read()
        digest.update(len(content).to_bytes(8, "big"))
        digest.update(content)
print(digest.hexdigest())
PY
)"

python3 - "$source_summary" "$binary_summary" "$work_dir/build-summary.json" "$evidence" "$source_revision" "$tar_sha" "$binary_sha" "$source_tree_sha" <<'PY'
import json
from pathlib import Path
import sys

source_scan, binary_scan, build, output, revision, package_sha, binary_sha, source_sha = sys.argv[1:]
evidence = {
    "schema_version": 1,
    "accepted_risk_ids": ["RA-001", "RA-002"],
    "source_revision": revision,
    "source_tree_sha256": source_sha,
    "package_sha256": package_sha,
    "control_plane_sha256": binary_sha,
    "build": json.loads(Path(build).read_text()),
    "source_scan": json.loads(Path(source_scan).read_text()),
    "binary_scan": json.loads(Path(binary_scan).read_text()),
}
Path(output).write_text(json.dumps(evidence, sort_keys=True, separators=(",", ":")) + "\n")
PY

echo "[release-security] source=${source_tree_sha} binary=${binary_sha} package=${tar_sha}"
echo "[release-security] evidence=${evidence}"
