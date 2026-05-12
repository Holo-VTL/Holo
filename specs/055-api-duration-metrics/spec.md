# Feature Specification: API Duration Metrics

**Feature Branch**: `codex/048-release-followup-hardening`
**Created**: 2026-05-11
**Status**: Draft
**Input**: User description: "Continue remaining release hardening after pausing the RC."

## User Scenario

As an operator troubleshooting RC behavior, I want Prometheus-compatible API request duration metrics, so that slow control-plane calls are visible without attaching a profiler.

## Requirements

- **FR-001**: Control-plane HTTP requests MUST be recorded in an aggregate duration histogram.
- **FR-002**: The histogram MUST expose Prometheus text-format bucket, sum, and count samples.
- **FR-003**: Histogram labels MUST avoid request path or object id cardinality.
- **FR-004**: Metrics collection MUST preserve existing routing, auth, and health behavior.

## Success Criteria

- **SC-001**: Registry unit tests cover bucket/count/sum updates.
- **SC-002**: Metrics handler tests cover exported histogram samples.
- **SC-003**: Router tests prove requests are recorded before a subsequent scrape.
- **SC-004**: `cd control-plane && go test ./... && go vet ./...` passes.
