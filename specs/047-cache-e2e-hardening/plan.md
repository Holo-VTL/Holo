# Implementation Plan: Cache Invalidation And Gated SCSI E2E

**Branch**: `047-cache-e2e-hardening` | **Date**: 2026-05-11 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `/Users/lei/AI_CC_Home/vtlix/specs/047-cache-e2e-hardening/spec.md`

## Summary

Implement Batch C/D hardening without changing persisted formats or normal CI requirements. Batch C documents the cache invalidation failure model and hardens the data-plane read prefetch cache so invalidation failure is observable, disables risky prefetch reuse, and does not turn already-committed writes into failed SCSI writes. Batch D adds a gated Linux privileged SCSI E2E harness with explicit opt-in, prerequisite checks, cleanup traps, and release-maintainer documentation.

## Technical Context

**Language/Version**: Rust stable for `data-plane/`; Bash 4+ for scripts; Markdown for design and evidence  
**Primary Dependencies**: Rust stdlib only for cache guardrails; shell tools `targetcli`, `iscsiadm`, `sg_inq`, `sg_readcap`, optional `sg_dd`/`dd` for smoke  
**Storage**: Existing filesystem-backed cartridge layout; no persisted segment, metadata, SQLite, or audit schema change  
**Testing**: `cargo test` for data-plane regression; shell dry-run/prerequisite checks for the gated harness  
**Target Platform**: Runtime Linux; E2E harness Linux privileged hosts only, disabled by default elsewhere  
**Project Type**: Multi-domain storage product: data-plane library plus release validation script  
**Performance Goals**: No new hot-path allocations beyond bounded counters/status on invalidation failure; prefetch remains optional and off by default  
**Constraints**: No stale cached reads after invalidation failure; no host mutation unless explicit E2E opt-in is set; cleanup on success/failure/interruption  
**Scale/Scope**: One cache family in data-plane read prefetch, one gated release harness, design docs and Speckit artifacts  
**Legacy Baseline**: Supports legacy durable tape write/read and real initiator compatibility goals tracked in `/Users/lei/AI_CC_Home/quadstorvtl`, `docs/08-legacy-capability-matrix.md`, and `docs/11-legacy-device-model-matrix.md`

## Constitution Check

- **Boundary Gate**: PASS. Data-plane cache behavior stays under `data-plane/`; release validation stays under `scripts/` and `tests/integration/`; docs under `docs/` and `specs/`.
- **Spec Workflow Gate**: PASS. This plan is derived from the 047 spec user stories and success criteria.
- **Test & Compatibility Gate**: PASS. Add fail-first Rust regression tests for invalidation failure semantics and shell checks for non-opt-in/non-Linux/prerequisite behavior.
- **Observability & Audit Gate**: PASS. Add bounded runtime cache failure counters/status and deterministic stderr log lines; no new management action means no audit schema change.
- **Security & Rollback Gate**: PASS. E2E requires explicit opt-in and root checks before host mutation; cache rollback is reverting in-memory guardrails with no data migration.
- **Dependency Mirror Gate**: PASS. No new Go/Cargo/NPM runtime dependencies. E2E docs list OS packages and relies on existing installer mirror practices.
- **Legacy Parity Gate**: PASS. This is additive hardening for existing durable tape I/O and initiator validation. Update the legacy capability matrix with evidence links.

## Project Structure

### Documentation (this feature)

```text
specs/047-cache-e2e-hardening/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   └── gated-scsi-e2e.md
└── tasks.md

docs/
└── 18-cache-invalidation-error-model.md
```

### Source Code

```text
data-plane/src/scsi_tape/
├── command_chain.rs
└── command_chain_tests.rs

scripts/
└── gated-scsi-e2e.sh

tests/integration/
└── test_gated_scsi_e2e.sh
```

**Structure Decision**: Keep implementation in the owning runtime domains. Cache guardrails are data-plane only; the E2E harness is a release-maintainer script plus shell tests for safe gating.

## Complexity Tracking

No constitution violations or complexity exceptions.
