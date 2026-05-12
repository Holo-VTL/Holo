# Feature Specification: Round-2 NIT Error Context

**Feature Branch**: `codex/048-release-followup-hardening`
**Created**: 2026-05-11
**Status**: Draft
**Input**: User description: "Continue §2.x/§3.x minor defects and Round-2 NIT follow-up items."

## User Scenario

As a maintainer reviewing release hardening, I want handler errors with concrete internal causes to preserve those causes in server logs, so that public responses remain safe while diagnostics do not lose the triggering error.

## Requirements

- **FR-001**: API handlers MUST pass concrete parse/build/stat/validation errors to `respondError` when such an error is available.
- **FR-002**: Public response messages MUST remain safe and unchanged for clients.
- **FR-003**: Method, auth, rate-limit, and not-found responses without a concrete internal error MAY continue passing nil.

## Success Criteria

- **SC-001**: Audit timestamp, audit cursor, support bundle, UI dist stat, and target publication validation paths preserve internal errors.
- **SC-002**: `cd control-plane && go test ./... && go vet ./...` passes.
