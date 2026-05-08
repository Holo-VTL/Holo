#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

printf '[integration] control-plane contract smoke\n'
(
  cd "$ROOT_DIR/control-plane"
  go test ./internal/api -run 'TestAuthMiddleware_ProtectsManagementRoutes|TestPolicyHandler_AccessPolicyRejectsNilBody' -count=1
)

printf '[integration] data-plane contract smoke\n'
(
  cd "$ROOT_DIR/data-plane"
  cargo test test_drive_persistent_reserve_out_updates_reservation_state -- --nocapture
)

printf '[integration] smoke suite passed\n'
