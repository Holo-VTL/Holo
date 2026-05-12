# Feature Specification: Storage Length Cast Cleanup

**Feature Branch**: `codex/048-release-followup-hardening`
**Created**: 2026-05-11
**Status**: Draft
**Input**: User description: "Continue §3.3 cast Stage 2 mechanical cleanup."

## User Scenario

As a maintainer reducing risky Rust numeric casts, I want persisted storage length fields to use explicit checked conversions before comparison, decompression, or allocation, so the cast baseline keeps moving downward without changing data formats.

## Requirements

- **FR-001**: Storage decompression and data blob read paths MUST avoid direct `as usize` casts for persisted logical/stored length fields.
- **FR-002**: Replacements MUST preserve the existing corruption error model and on-disk formats.
- **FR-003**: The production cast scanner baseline MUST be tightened when production casts decrease.

## Success Criteria

- **SC-001**: LZ4 declared lengths, logical read lengths, dedup verification lengths, and data blob stored lengths use checked conversions.
- **SC-002**: Rust tests and clippy pass.
- **SC-003**: `scripts/scan-rust-casts.sh` passes with a reduced production baseline.
