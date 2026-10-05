#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [[ "$(uname -s)" != "Linux" ]]; then
  echo "layout lease integration requires Linux" >&2
  exit 77
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

rust_test_binary="$({
  cd "$repo_root/data-plane"
  "$HOME/.cargo/bin/cargo" test --lib --no-run --message-format=json
} | python3 -c 'import json,sys; arts=[json.loads(line) for line in sys.stdin if line.startswith("{")]; bins=[a.get("executable") for a in arts if a.get("reason")=="compiler-artifact" and a.get("profile",{}).get("test") and a.get("executable") and "lib" in a.get("target",{}).get("kind",[])]; print(bins[-1] if bins else "")')"
if [[ -z "$rust_test_binary" || ! -x "$rust_test_binary" ]]; then
  echo "could not locate the Rust layout lease test binary" >&2
  exit 1
fi

go_test_binary="$tmp_dir/storageutil.test"
(
  cd "$repo_root/control-plane"
  go test -c -o "$go_test_binary" ./internal/storageutil
)

HOLO_RUST_LEASE_TEST_BIN="$rust_test_binary" \
  "$go_test_binary" -test.run='^TestLayoutLeaseSerializesAliasAgainstRust$' -test.v
