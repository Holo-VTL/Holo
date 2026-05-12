# Feature Specification: Capacity Accounting Hardening

**Feature Branch**: `codex/048-release-followup-hardening`  
**Created**: 2026-05-11  
**Status**: Draft  
**Input**: User description: "Continue after pausing the RC and troubleshoot capacity usage issues before the next release candidate."

## User Scenarios & Testing

### User Story 1 - Reconcile Pool Usage From Cartridge Usage (Priority: P1)

As an operator, I want storage pool used capacity to reflect cartridge usage reported by the data-plane, so that the pool page and cartridge page do not disagree during real-machine validation.

**Independent Test**: Write shared cartridge metadata with `used_bytes`, request the storage pool through the API, and verify pool `capacity.usedBytes` matches the sum of cartridges bound to that pool.

**Acceptance Scenarios**:

1. **Given** a cartridge belongs to a pool and data-plane metadata reports non-zero `used_bytes`, **When** the control-plane reconciles media state, **Then** the cartridge `UsedBytes` and pool `capacity.usedBytes` both reflect that value.
2. **Given** multiple cartridges belong to one pool, **When** reconciliation runs, **Then** pool used capacity is the bounded sum of non-retired cartridge usage for that pool.
3. **Given** a cartridge is erased or destroyed, **When** the operation succeeds, **Then** the owning pool usage is recomputed from the remaining cartridges.

## Requirements

- **FR-001**: Reconciliation MUST treat cartridge metadata `used_bytes` as the logical pool usage source for non-strict storage views.
- **FR-002**: Storage pool APIs MUST trigger media-state reconciliation before returning pool list, pool detail, or pool capacity.
- **FR-003**: Cartridge erase and destroy operations MUST recompute the owning pool usage after successful metadata/repository updates.
- **FR-004**: Pool usage reconciliation MUST reject negative usage and clamp over-capacity through existing pool snapshot rules.
- **FR-005**: The feature MUST NOT change persisted SQLite schema, cartridge metadata format, or data-plane segment format.

## Success Criteria

- **SC-001**: A control-plane API regression test proves storage pool capacity reconciles from shared cartridge metadata.
- **SC-002**: Memory and SQLite storage pool repos have tests for direct used-byte reconciliation.
- **SC-003**: `cd control-plane && go test ./...` passes.
- **SC-004**: Existing data-plane and guardrail checks continue to pass.
