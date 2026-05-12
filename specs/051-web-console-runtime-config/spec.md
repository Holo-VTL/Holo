# Feature Specification: Web Console Runtime Config

**Feature Branch**: `codex/048-release-followup-hardening`
**Created**: 2026-05-11
**Status**: Draft
**Input**: User description: "Continue remaining release hardening after pausing the RC."

## User Scenario

As an operator or release maintainer, I want the Web Console API endpoint to be configurable at runtime, so that the same built artifact can run against same-origin appliance deployments or an explicitly configured backend without rebuilding.

## Requirements

- **FR-001**: Web Console MUST default to same-origin API calls when no runtime config is present.
- **FR-002**: Web Console MUST load runtime config from the deployed UI base path `config.json`.
- **FR-003**: Runtime config MUST support an `apiBaseUrl` value that prefixes `/healthz` and `/v1/*` calls.
- **FR-004**: Missing or unreadable runtime config MUST fall back to same-origin behavior.
- **FR-005**: Existing API-key session behavior MUST be preserved.

## Success Criteria

- **SC-001**: Unit tests prove default same-origin behavior.
- **SC-002**: Unit tests prove configured `apiBaseUrl` prefixes API calls.
- **SC-003**: `cd web-console && npm test && npm run build` passes.
