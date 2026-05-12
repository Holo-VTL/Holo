# Feature Specification: Extended Static Analysis

**Feature Branch**: `codex/048-release-followup-hardening`
**Created**: 2026-05-11
**Status**: Draft
**Input**: User description: "Continue remaining release hardening after pausing the RC."

## User Scenario

As a release maintainer, I want repeatable static-analysis checks beyond unit tests, so that shell scripting defects and known dependency advisories are caught by CI before release artifacts are built.

## Requirements

- **FR-001**: CI MUST run ShellCheck over repository shell scripts.
- **FR-002**: CI MUST run `govulncheck ./...` for the Go control-plane.
- **FR-003**: CI MUST run `cargo audit` for the Rust data-plane.
- **FR-004**: Static-analysis script MUST explicitly report N/A checks for Dockerfile linting when no Dockerfiles exist.
- **FR-005**: Static-analysis script MUST avoid silently passing missing required tools unless an explicit local development escape hatch is set.

## Success Criteria

- **SC-001**: `bash -n scripts/static-analysis.sh` passes.
- **SC-002**: `HOLO_STATIC_ANALYSIS_ALLOW_MISSING=1 bash scripts/static-analysis.sh` reports unavailable local tools instead of hiding them.
- **SC-003**: `infra/ci/mvp-ci.yml` invokes the extended static-analysis script after installing required tools.
