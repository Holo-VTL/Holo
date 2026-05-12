# Implementation Plan: Capacity Accounting Hardening

**Branch**: `codex/048-release-followup-hardening`  
**Spec**: `/Users/lei/AI_CC_Home/vtlix/specs/049-capacity-accounting-hardening/spec.md`  
**Date**: 2026-05-11

## Summary

Close the immediate capacity troubleshooting gap where cartridge `used_bytes` can be reconciled from data-plane shared metadata but storage pool `usedBytes` remains stale. Add an internal pool used-byte reconciliation method, call it from resource media-state reconciliation, and make storage pool read/delete paths reconcile before decisions.

## Scope

In scope:

- Logical pool usage reconciliation from current cartridges.
- Storage API reconciliation before pool list/detail/capacity/delete/detach decisions.
- Memory/SQLite repo tests and API regression coverage.

Out of scope:

- New schema fields for separating logical cartridge usage from physical filesystem usage.
- Deep capacity observability dashboards.
- Changing the data-plane cartridge metadata format.
- Reworking strict storage mode `df` snapshot semantics.

## Verification

Run:

```bash
cd control-plane && go test ./...
cd control-plane && go vet ./...
cd data-plane && cargo test --locked
bash scripts/lint-guardrails.sh
bash scripts/scan-rust-casts.sh
```
