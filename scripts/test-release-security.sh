#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT

mkdir -p "$test_dir/fixtures" "$test_dir/fake-bin" "$test_dir/repo/scripts" "$test_dir/repo/control-plane" "$test_dir/package/holo-vtl-test"
cp "$repo_root/scripts/verify-govulncheck-json.py" "$test_dir/repo/scripts/"
cp "$repo_root/scripts/verify-go-buildinfo.py" "$test_dir/repo/scripts/"
cp "$repo_root/scripts/verify-sha256.sh" "$test_dir/repo/scripts/"
cp "$repo_root/scripts/release-security-scan.sh" "$test_dir/repo/scripts/"
cat > "$test_dir/fixtures/valid-source.jsonl" <<'EOF'
{"config":{"protocol_version":"v1","scanner_name":"govulncheck","scanner_version":"v1.8.0","db":"https://vuln.go.dev","db_last_modified":"2026-10-04T10:00:00Z","go_version":"go1.26.8","scan_level":"symbol","scan_mode":"source"}}
{"progress":{"message":"scan complete"}}
{"SBOM":{"go_version":"go1.26.8"}}
EOF
cat > "$test_dir/fixtures/valid-binary.jsonl" <<'EOF'
{"config":{"protocol_version":"v1","scanner_name":"govulncheck","scanner_version":"v1.8.0","db":"https://vuln.go.dev","db_last_modified":"2026-10-04T10:00:00Z","scan_level":"symbol","scan_mode":"binary"}}
{"SBOM":{"go_version":"go1.26.8"}}
EOF
cat > "$test_dir/fixtures/high-finding.jsonl" <<'EOF'
{"config":{"protocol_version":"v1","scanner_name":"govulncheck","scanner_version":"v1.8.0","db":"https://vuln.go.dev","db_last_modified":"2026-10-04T10:00:00Z","go_version":"go1.26.8","scan_level":"symbol","scan_mode":"source"}}
{"osv":{"id":"GO-2026-0001","database_specific":{"severity":"HIGH"}}}
{"finding":{"osv":"GO-2026-0001","trace":[]}}
EOF
cat > "$test_dir/fixtures/symbol-finding.jsonl" <<'EOF'
{"config":{"protocol_version":"v1","scanner_name":"govulncheck","scanner_version":"v1.8.0","db":"https://vuln.go.dev","db_last_modified":"2026-10-04T10:00:00Z","go_version":"go1.26.8","scan_level":"symbol","scan_mode":"source"}}
{"finding":{"osv":"GO-2026-0002","trace":[{"function":"example.Vulnerable"}]}}
EOF
cat > "$test_dir/fixtures/missing-db.jsonl" <<'EOF'
{"config":{"protocol_version":"v1","scanner_name":"govulncheck","scanner_version":"v1.8.0","go_version":"go1.26.8","scan_mode":"source"}}
EOF
cat > "$test_dir/fixtures/unknown-message.jsonl" <<'EOF'
{"config":{"protocol_version":"v1","scanner_name":"govulncheck","scanner_version":"v1.8.0","db":"https://vuln.go.dev","db_last_modified":"2026-10-04T10:00:00Z","go_version":"go1.26.8","scan_level":"symbol","scan_mode":"source"}}
{"new_message":{"value":1}}
EOF
cat > "$test_dir/fixtures/windows-only.jsonl" <<'EOF'
{"config":{"protocol_version":"v1","scanner_name":"govulncheck","scanner_version":"v1.8.0","db":"https://vuln.go.dev","db_last_modified":"2026-10-04T10:00:00Z","go_version":"go1.26.8","scan_level":"symbol","scan_mode":"source"}}
{"osv":{"id":"GO-2026-0003","affected":[{"package":{"name":"golang.org/x/sys","ecosystem":"Go"},"ecosystem_specific":{"imports":[{"path":"golang.org/x/sys/windows","goos":["windows"],"symbols":["WindowsOnlyCall"]}]}}]}}
{"finding":{"osv":"GO-2026-0003","trace":[{"module":"golang.org/x/sys","version":"v0.37.0"}]}}
EOF
cat > "$test_dir/fixtures/buildinfo.txt" <<'EOF'
/opt/holo/control-plane: go1.26.8
path github.com/Holo-VTL/Holo/control-plane/cmd/api
build CGO_ENABLED=0
build GOARCH=amd64
build GOOS=linux
EOF

python3 "$repo_root/scripts/verify-govulncheck-json.py" --input "$test_dir/fixtures/valid-source.jsonl" --mode source --go-version 1.26.8 --scanner-version v1.8.0 --goos linux --goarch amd64 > /dev/null
python3 "$repo_root/scripts/verify-govulncheck-json.py" --input "$test_dir/fixtures/valid-binary.jsonl" --mode binary --go-version 1.26.8 --scanner-version v1.8.0 --goos linux --goarch amd64 > /dev/null
python3 "$repo_root/scripts/verify-govulncheck-json.py" --input "$test_dir/fixtures/windows-only.jsonl" --mode source --go-version 1.26.8 --scanner-version v1.8.0 --goos linux --goarch amd64 > /dev/null
python3 "$repo_root/scripts/verify-govulncheck-json.py" --input "$test_dir/fixtures/high-finding.jsonl" --mode source --go-version 1.26.8 --scanner-version v1.8.0 --goos linux --goarch amd64 >/dev/null 2>&1 && { echo "high finding was accepted" >&2; exit 1; }
python3 "$repo_root/scripts/verify-govulncheck-json.py" --input "$test_dir/fixtures/symbol-finding.jsonl" --mode source --go-version 1.26.8 --scanner-version v1.8.0 --goos linux --goarch amd64 >/dev/null 2>&1 && { echo "symbol-reachable finding was accepted" >&2; exit 1; }
python3 "$repo_root/scripts/verify-govulncheck-json.py" --input "$test_dir/fixtures/missing-db.jsonl" --mode source --go-version 1.26.8 --scanner-version v1.8.0 --goos linux --goarch amd64 >/dev/null 2>&1 && { echo "missing database metadata was accepted" >&2; exit 1; }
python3 "$repo_root/scripts/verify-govulncheck-json.py" --input "$test_dir/fixtures/unknown-message.jsonl" --mode source --go-version 1.26.8 --scanner-version v1.8.0 --goos linux --goarch amd64 >/dev/null 2>&1 && { echo "unknown scanner output was accepted" >&2; exit 1; }
printf '%s\n' '{malformed' > "$test_dir/fixtures/malformed.jsonl"
python3 "$repo_root/scripts/verify-govulncheck-json.py" --input "$test_dir/fixtures/malformed.jsonl" --mode source --go-version 1.26.8 --scanner-version v1.8.0 --goos linux --goarch amd64 >/dev/null 2>&1 && { echo "malformed scanner JSON was accepted" >&2; exit 1; }

python3 "$repo_root/scripts/verify-go-buildinfo.py" --input "$test_dir/fixtures/buildinfo.txt" --go-version 1.26.8 --goos linux --goarch amd64 --cgo 0 --module github.com/Holo-VTL/Holo/control-plane/cmd/api >/dev/null
python3 "$repo_root/scripts/verify-go-buildinfo.py" --input "$test_dir/fixtures/buildinfo.txt" --go-version 1.24.3 --goos linux --goarch amd64 --cgo 0 --module github.com/Holo-VTL/Holo/control-plane/cmd/api >/dev/null 2>&1 && { echo "wrong Go version was accepted" >&2; exit 1; }
python3 "$repo_root/scripts/verify-go-buildinfo.py" --input "$test_dir/fixtures/buildinfo.txt" --go-version 1.26.8 --goos linux --goarch arm64 --cgo 0 --module github.com/Holo-VTL/Holo/control-plane/cmd/api >/dev/null 2>&1 && { echo "wrong architecture was accepted" >&2; exit 1; }
python3 "$repo_root/scripts/verify-go-buildinfo.py" --input "$test_dir/fixtures/buildinfo.txt" --go-version 1.26.8 --goos linux --goarch amd64 --cgo 1 --module github.com/Holo-VTL/Holo/control-plane/cmd/api >/dev/null 2>&1 && { echo "wrong cgo setting was accepted" >&2; exit 1; }

printf 'release fixture\n' > "$test_dir/package/holo-vtl-test/control-plane"
tar czf "$test_dir/candidate.tar.gz" -C "$test_dir/package" holo-vtl-test
package_sha="$(sha256sum "$test_dir/candidate.tar.gz" | awk '{print $1}')"
"$repo_root/scripts/verify-sha256.sh" "$test_dir/candidate.tar.gz" "$package_sha" >/dev/null
"$repo_root/scripts/verify-sha256.sh" "$test_dir/candidate.tar.gz" "$(printf '0%.0s' {1..64})" >/dev/null 2>&1 && { echo "wrong artifact SHA was accepted" >&2; exit 1; }

cat > "$test_dir/fake-bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "version" && "$#" -eq 1 ]]; then
  echo "go version ${FAKE_GO_VERSION:-go1.26.8} linux/amd64"
elif [[ "$1" == "version" && "$2" == "-m" ]]; then
  cat "$FAKE_BUILD_INFO"
else
  exit 2
fi
EOF
cat > "$test_dir/fake-bin/govulncheck" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "-version" ]]; then
  echo "govulncheck version: v1.8.0"
elif [[ " $* " == *" -mode=binary "* ]]; then
  cat "$FAKE_BINARY_SCAN"
else
  cat "$FAKE_SOURCE_SCAN"
fi
EOF
chmod +x "$test_dir/fake-bin/go" "$test_dir/fake-bin/govulncheck"
cp "$test_dir/fixtures/buildinfo.txt" "$test_dir/fixtures/buildinfo-live.txt"
FAKE_BUILD_INFO="$test_dir/fixtures/buildinfo-live.txt" \
FAKE_SOURCE_SCAN="$test_dir/fixtures/valid-source.jsonl" \
FAKE_BINARY_SCAN="$test_dir/fixtures/valid-binary.jsonl" \
PATH="$test_dir/fake-bin:$PATH" \
  "$test_dir/repo/scripts/release-security-scan.sh" \
    --repo-root "$test_dir/repo" --tarball "$test_dir/candidate.tar.gz" \
    --scanner "$test_dir/fake-bin/govulncheck" --source-revision test-revision \
    --evidence "$test_dir/security-evidence.json" --expected-tar-sha256 "$package_sha" >/dev/null
python3 - "$test_dir/security-evidence.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as stream:
    evidence = json.load(stream)
assert evidence["schema_version"] == 1
assert evidence["source_scan"]["scan_mode"] == "source"
assert evidence["binary_scan"]["scan_mode"] == "binary"
assert evidence["package_sha256"]
assert evidence["accepted_risk_ids"] == ["RA-001", "RA-002"]
PY

FAKE_BUILD_INFO="$test_dir/fixtures/buildinfo-live.txt" \
FAKE_SOURCE_SCAN="$test_dir/fixtures/valid-source.jsonl" \
FAKE_BINARY_SCAN="$test_dir/fixtures/valid-binary.jsonl" \
PATH="$test_dir/fake-bin:$PATH" \
  "$test_dir/repo/scripts/release-security-scan.sh" \
    --repo-root "$test_dir/repo" --tarball "$test_dir/candidate.tar.gz" \
    --scanner "$test_dir/fake-bin/not-installed" --source-revision test-revision \
    --evidence "$test_dir/missing-scanner.json" >/dev/null 2>&1 && { echo "missing scanner was accepted" >&2; exit 1; }

FAKE_BUILD_INFO="$test_dir/fixtures/buildinfo-live.txt" \
FAKE_SOURCE_SCAN="$test_dir/fixtures/missing-db.jsonl" \
FAKE_BINARY_SCAN="$test_dir/fixtures/valid-binary.jsonl" \
PATH="$test_dir/fake-bin:$PATH" \
  "$test_dir/repo/scripts/release-security-scan.sh" \
    --repo-root "$test_dir/repo" --tarball "$test_dir/candidate.tar.gz" \
    --scanner "$test_dir/fake-bin/govulncheck" --source-revision test-revision \
    --evidence "$test_dir/no-database.json" >/dev/null 2>&1 && { echo "missing scanner database was accepted" >&2; exit 1; }

FAKE_GO_VERSION=go1.24.3 \
FAKE_BUILD_INFO="$test_dir/fixtures/buildinfo-live.txt" \
FAKE_SOURCE_SCAN="$test_dir/fixtures/valid-source.jsonl" \
FAKE_BINARY_SCAN="$test_dir/fixtures/valid-binary.jsonl" \
PATH="$test_dir/fake-bin:$PATH" \
  "$test_dir/repo/scripts/release-security-scan.sh" \
    --repo-root "$test_dir/repo" --tarball "$test_dir/candidate.tar.gz" \
    --scanner "$test_dir/fake-bin/govulncheck" --source-revision test-revision \
    --evidence "$test_dir/wrong-go.json" >/dev/null 2>&1 && { echo "wrong Go toolchain was accepted" >&2; exit 1; }

echo "[test-release-security] all release scan guardrails passed"
