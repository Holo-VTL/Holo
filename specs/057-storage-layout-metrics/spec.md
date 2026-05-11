# Feature Specification: Storage Layout Metrics

**Feature Branch**: `codex/048-release-followup-hardening`
**Created**: 2026-05-11
**Status**: Draft
**Input**: User description: "Continue remaining release hardening after pausing the RC."

## User Scenario

As an operator troubleshooting RC storage behavior, I want `/metrics` to expose storage layout file counts and bytes, so that I can confirm segment, dedup, and reclaim artifacts are growing under the expected pool root.

## Requirements

- **FR-001**: `/metrics` MUST include aggregate storage segment file count and byte gauges.
- **FR-002**: `/metrics` MUST include aggregate data segment, dedup segment, and reclaim segment byte gauges.
- **FR-003**: Storage layout metrics MUST scan the configured pool root base without path or cartridge labels.
- **FR-004**: Missing or unreadable pool roots MUST export zero values rather than failing the metrics scrape.

## Success Criteria

- **SC-001**: Unit tests cover segment/dedup/reclaim file aggregation.
- **SC-002**: Unit tests cover missing root zero-value behavior.
- **SC-003**: Metrics handler output includes the storage layout gauges.
- **SC-004**: `cd control-plane && go test ./... && go vet ./...` passes.
