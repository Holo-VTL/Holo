#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "Usage: verify-sha256.sh FILE EXPECTED_SHA256" >&2
  exit 2
fi

file="$1"
expected="$2"
if [[ ! "$expected" =~ ^[0-9a-f]{64}$ || ! -f "$file" ]]; then
  echo "[sha256] invalid file or expected digest" >&2
  exit 1
fi
actual="$(sha256sum "$file" | awk '{print $1}')"
if [[ "$actual" != "$expected" ]]; then
  echo "[sha256] checksum mismatch" >&2
  exit 1
fi
echo "$actual"
