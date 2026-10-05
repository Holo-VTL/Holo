#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

printf '[integration] control-plane contract smoke\n'
(
  cd "$ROOT_DIR/control-plane"
  go test ./internal/api -run 'TestAuthMiddleware_ProtectsManagementRoutes|TestISCSISecurityBindingAPIUsesOfflineGuardAndReturnsSources' -count=1
)

printf '[integration] data-plane contract smoke\n'
(
  cd "$ROOT_DIR/data-plane"
  cargo test test_drive_persistent_reserve_out_updates_reservation_state -- --nocapture
)

printf '[integration] 055 bounded maintenance and recovery smoke\n'
(
  cd "$ROOT_DIR/control-plane"
  go test ./internal/orchestration -run 'TestStorageMaintenance|TestDecodeMaintenance|TestMaintenanceProgress' -count=1
)
(
  cd "$ROOT_DIR/data-plane"
  cargo test --bin holo_storage_maintenance
  cargo test --lib storage::space_guard_tests:: -- --test-threads=1
  cargo test --lib storage::offline_reclaim_tests:: -- --test-threads=1
  cargo test --lib storage::recovery_tests:: -- --test-threads=1
)

printf '[integration] smoke suite passed\n'
