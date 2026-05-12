# Tasks: Capacity Accounting Hardening

**Input**: `/Users/lei/AI_CC_Home/vtlix/specs/049-capacity-accounting-hardening/spec.md`

## Phase 1: Reconciliation Contract

- [X] T001 Add storage pool used-byte reconciliation API to domain, memory repo, SQLite repo, and storage service.
- [X] T002 Add memory repo test for used-byte reconciliation.
- [X] T003 Add SQLite repo test for persisted used-byte reconciliation.

## Phase 2: API Wiring

- [X] T004 Recompute pool usage from cartridge `UsedBytes` during media-state reconciliation.
- [X] T005 Recompute owning pool usage after cartridge create, erase, and destroy paths.
- [X] T006 Trigger media-state reconciliation before storage pool list/detail/capacity/delete/detach paths.

## Phase 3: Regression

- [X] T007 Add API regression proving pool capacity follows shared cartridge metadata `used_bytes`.
- [X] T008 Run validation commands and record outcome.
