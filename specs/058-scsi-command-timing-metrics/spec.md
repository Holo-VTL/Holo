# Feature Specification: SCSI Command Timing Metrics

**Feature Branch**: `codex/048-release-followup-hardening`
**Created**: 2026-05-11
**Status**: Draft
**Input**: User description: "Continue remaining release hardening after pausing the RC."

## User Scenario

As an operator validating real initiator behavior, I want SCSI command counts and latency totals in `/metrics`, so that read/write command flow and slow data-plane handling are visible during RC tests.

## Requirements

- **FR-001**: Control-plane publication runtime MUST pass a per-publication SCSI timing metrics file path to `tcmu_handler`.
- **FR-002**: `tcmu_handler` MUST write Prometheus text timing counters for read, write, and other SCSI command buckets when timing metrics are enabled.
- **FR-003**: Control-plane `/metrics` MUST aggregate per-publication SCSI timing metrics files without publication labels.
- **FR-004**: Missing timing metrics files or directories MUST export zero SCSI timing values rather than failing the scrape.

## Success Criteria

- **SC-001**: Rust unit tests prove `tcmu_handler` writes timing metrics.
- **SC-002**: Go unit tests prove timing files are aggregated.
- **SC-003**: Go tests prove runtime env includes the timing metrics file.
- **SC-004**: `cd control-plane && go test ./... && go vet ./...` and `cd data-plane && cargo test --locked --bin tcmu_handler` pass.
