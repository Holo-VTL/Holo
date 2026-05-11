# Feature Specification: TCMU Poll Cancel Hardening

**Feature Branch**: `codex/048-release-followup-hardening`  
**Created**: 2026-05-11  
**Status**: Draft  
**Input**: User description: "Continue remaining release hardening after pausing the RC."

## User Scenario

As an operator publishing a TCMU-backed target, I want startup waits to honor cancellation promptly, so that failed or canceled publication requests do not linger in fixed sleep loops.

## Requirements

- **FR-001**: TCMU handler socket readiness polling MUST use context-aware select/ticker waiting instead of fixed sleep loops.
- **FR-002**: If the request context is canceled while waiting for the handler socket, the adapter MUST kill the spawned handler and return a context-wrapped error.
- **FR-003**: Existing timeout and early-process-exit behavior MUST remain unchanged.

## Success Criteria

- **SC-001**: A unit test proves socket waiting returns immediately when context is canceled.
- **SC-002**: `cd control-plane && go test ./internal/orchestration` passes.
