# Implementation Plan: Release Follow-Up Hardening

**Branch**: `codex/048-release-followup-hardening`  
**Spec**: `/Users/lei/AI_CC_Home/vtlix/specs/048-release-followup-hardening/spec.md`  
**Date**: 2026-05-11

## Summary

Implement a narrow release-follow-up batch on top of the v1.0.1 RC validation branch. This batch closes low-risk review NITs, promotes currently clean static analysis to CI, tightens the Rust cast baseline after mechanical cleanup, and adds web-console CI/default-host hardening.

## Technical Context

- **Control-plane**: Go 1.24+, existing stdlib tests and guardrails.
- **Data-plane**: Rust stable, existing unit test suite and cast scanner.
- **Web-console**: React + TypeScript + Vite + Vitest, existing `package-lock.json`.
- **CI**: Existing `infra/ci/mvp-ci.yml` MVP workflow.

## Scope

In scope:

- Round-2 NIT follow-ups that are behavior-preserving or observability-only.
- `go vet`, `cargo clippy -D warnings`, and web-console CI checks.
- Mechanical Clippy cleanup required to make the CI gate viable.
- Vite loopback binding defaults.

Out of scope:

- Deep privileged SCSI scenario expansion.
- New observability metric families.
- Storage-map cache read-only degradation policy.
- Dedup incremental rebuild, fsync/tokio changes, and repository architecture changes.
- Web-console CSP/SRI redesign beyond existing server security headers.

## Verification

Run:

```bash
cd control-plane && go test ./...
cd control-plane && go vet ./...
cd data-plane && cargo test --locked
cd data-plane && cargo clippy --locked --all-targets -- -D warnings
bash scripts/lint-guardrails.sh
bash scripts/scan-rust-casts.sh
bash tests/integration/test_gated_scsi_e2e.sh
cd web-console && npm audit --audit-level=high
cd web-console && npm test
cd web-console && npm run build
```
