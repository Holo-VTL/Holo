# Feature Specification: Audit Journal Ops Metrics

**Feature Branch**: `codex/048-release-followup-hardening`
**Created**: 2026-05-11
**Status**: Draft
**Input**: User description: "Continue remaining release hardening after pausing the RC."

## User Scenario

As an operator troubleshooting RC behavior, I want audit journal size and last-write lag metrics, so that I can spot audit persistence stalls or unexpected journal growth from Prometheus output.

## Requirements

- **FR-001**: Successful audit journal writes MUST update a current journal size gauge.
- **FR-002**: Successful audit journal writes MUST update a last-write timestamp used to expose lag seconds.
- **FR-003**: Metrics export MUST include `holo_audit_journal_size_bytes` and `holo_audit_journal_lag_seconds`.
- **FR-004**: Audit journal persistence format MUST remain unchanged.

## Success Criteria

- **SC-001**: Writer tests prove journal size and last-write metrics are recorded after a successful persistent write.
- **SC-002**: Metrics handler tests prove the new gauges are exported.
- **SC-003**: `cd control-plane && go test ./... && go vet ./...` passes.
