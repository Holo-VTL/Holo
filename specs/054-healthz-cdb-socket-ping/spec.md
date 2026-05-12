# Feature Specification: Healthz CDB Socket Ping

**Feature Branch**: `codex/048-release-followup-hardening`
**Created**: 2026-05-11
**Status**: Draft
**Input**: User description: "Continue remaining release hardening after pausing the RC."

## User Scenario

As an operator running RC validation, I want `/healthz` to report whether the data-plane CDB socket is actually reachable, so that stale socket files and crashed handlers do not look healthy.

## Requirements

- **FR-001**: Data-plane health MUST attempt a short Unix socket connection to discovered `*.sock` entries under `HOLO_RUN_DIR`.
- **FR-002**: Data-plane health MUST report `ok` when at least one CDB socket accepts a connection.
- **FR-003**: Data-plane health MUST report `down` when socket-looking files exist but none are reachable.
- **FR-004**: Existing `unknown` behavior for missing run directories and no active publications MUST remain unchanged.

## Success Criteria

- **SC-001**: Unit tests cover a reachable Unix listener.
- **SC-002**: Unit tests cover a stale `.sock` path.
- **SC-003**: `cd control-plane && go test ./... && go vet ./...` passes.
