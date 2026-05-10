# Feature Specification: Cache Invalidation And Gated SCSI E2E

**Feature Branch**: `047-cache-e2e-hardening`  
**Created**: 2026-05-11  
**Status**: Draft  
**Input**: User description: "Implement Batch C and Batch D review hardening: write the cache invalidation error model, implement approved cache invalidation behavior so post-commit invalidation failure does not report a failed SCSI write but is observable and degrades safely, and add a gated real SCSI E2E harness for Linux privileged hosts covering target publication/start, open-iscsi discovery/login, sg_inq, sg_readcap, and a simple write-read smoke where feasible."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Define Cache Failure Semantics (Priority: P1)

As a data-plane maintainer, I want a written cache invalidation error model before code changes, so that SCSI write success, post-commit cleanup failure, and safe degradation are not confused.

**Why this priority**: Cache failure semantics affect protocol correctness. The design must be agreed before implementation to avoid reporting already-committed writes as failed.

**Independent Test**: Can be reviewed by reading the design document and verifying it names write-before, write-after, post-commit invalidation, cache poison/discard, metrics, logs, and rollback behavior.

**Acceptance Scenarios**:

1. **Given** a write fails before data is committed, **When** the design is reviewed, **Then** it states that the SCSI command may fail normally.
2. **Given** a write commits successfully but cache invalidation fails afterward, **When** the design is reviewed, **Then** it states that the host must not be asked to retry the committed write because of the post-commit cleanup failure.
3. **Given** repeated cache invalidation failures, **When** the design is reviewed, **Then** it defines observable degradation and how cached data is discarded or bypassed.

---

### User Story 2 - Implement Cache Invalidation Guardrails (Priority: P1)

As a backup operator, I want committed writes to remain protocol-successful even when internal post-commit cache cleanup has problems, so that initiators do not retry data that is already durable.

**Why this priority**: It protects data-plane correctness and avoids duplicate-write risk while still surfacing degraded cache health.

**Independent Test**: Can be tested by injecting cache invalidation failure after a successful write and asserting the write remains successful, stale cache entries are not reused, and observability records the failure.

**Acceptance Scenarios**:

1. **Given** a write is durably committed, **When** post-commit cache invalidation fails, **Then** the write response remains successful and the affected cache path is disabled or bypassed.
2. **Given** cache invalidation has failed, **When** subsequent reads occur, **Then** stale cached data is not returned.
3. **Given** repeated invalidation failures, **When** the data path continues, **Then** logs or counters make the degraded state visible for operations.

---

### User Story 3 - Provide Gated Real SCSI E2E Harness (Priority: P2)

As a release maintainer, I want an explicit Linux-only real SCSI smoke harness, so that v1.0.1 can validate at least one real initiator path without making privileged tests mandatory for every PR.

**Why this priority**: Real `open-iscsi`/`sg_*` validation catches integration gaps that unit tests cannot, but it needs privilege and host dependencies.

**Independent Test**: Can be tested by running the harness on a Linux host with required tools and by running a dry-run/prerequisite mode on normal developer machines.

**Acceptance Scenarios**:

1. **Given** a non-Linux or unprivileged host, **When** the E2E target is run without explicit opt-in, **Then** it exits with a clear skip/error message and does not mutate the host.
2. **Given** a Linux privileged host with dependencies installed, **When** the E2E target is run with opt-in, **Then** it performs discovery/login and runs `sg_inq` plus capacity checks against a Holo target.
3. **Given** the harness exits early or fails, **When** cleanup runs, **Then** iSCSI sessions, target publications, and temporary files are cleaned up as far as possible.

### Edge Cases

- Post-commit invalidation failure must not mask a prior actual write failure.
- Cache bypass must avoid stale reads without requiring a persisted format change.
- Observability must not allocate unbounded memory on repeated cache failures.
- E2E must not run by accident on developer laptops or unprivileged CI.
- E2E must document required tools (`targetcli`, `iscsiadm`, `sg_inq`, `sg_readcap`, and privilege) and skip clearly when missing.
- E2E cleanup must run on failure and interruption.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The repository MUST include a cache invalidation error model document for write-before, write-after, post-commit invalidation, cache discard/bypass, observability, and rollback semantics.
- **FR-002**: The data path MUST not report an already committed SCSI write as failed solely because post-commit cache invalidation failed.
- **FR-003**: The data path MUST prevent stale cached reads after an invalidation failure by discarding, poisoning, or bypassing affected cache state.
- **FR-004**: Cache invalidation failures MUST be observable through deterministic logs and bounded in-memory counters or status that tests can assert.
- **FR-005**: Repeated cache invalidation failures MUST degrade cache behavior safely instead of repeatedly reusing risky cached data.
- **FR-006**: The repository MUST expose a gated real SCSI E2E command that is disabled by default and requires explicit opt-in.
- **FR-007**: The E2E harness MUST check Linux, privilege, and required command prerequisites before mutating host iSCSI state.
- **FR-008**: The E2E harness MUST cover target startup/publication, open-iscsi discovery/login, `sg_inq`, `sg_readcap`, and a simple read/write smoke where feasible.
- **FR-009**: The E2E harness MUST clean up sessions, publications, processes, and temporary files on success, failure, or interruption.
- **FR-010**: The E2E harness MUST be documented as release/maintainer validation and not required in normal PR CI.

### Key Entities

- **Cache Invalidation Event**: A post-commit cache cleanup outcome with affected key/path, result, and failure count.
- **Cache Degradation State**: Runtime state that records whether cache reads should continue, bypass affected entries, or bypass the cache entirely.
- **E2E Harness Run**: A privileged validation session with prerequisites, temporary runtime paths, target identity, initiator session, command evidence, and cleanup status.

## Constitution Alignment *(mandatory)*

### Boundary & Contract Impact

- **Impacted Layers**: `data-plane` for cache invalidation guardrails; `tests`/`scripts`/`Makefile` for gated E2E; `docs` for the design model.
- **Cross-Layer Contracts**: No management API schema change is intended. Real SCSI E2E validates existing target publication and SCSI behavior but does not change the public protocol contract.

### Verification & Compatibility Plan

- **Fail-First Tests**: Add data-plane tests that inject post-commit invalidation failure and prove successful writes do not become failed responses; add harness dry-run/prerequisite tests or shell checks for non-Linux/unprivileged behavior.
- **Compatibility Impact**: Valid SCSI write/read behavior must remain unchanged except safer cache degradation after internal cleanup failures. The E2E harness is gated and does not run in normal PR CI.

### Legacy Capability Alignment

- **Legacy Reference Scope**: Legacy reference is operational behavior around durable tape writes and real initiator compatibility in `/Users/lei/AI_CC_Home/quadstorvtl`.
- **Capability Mapping**: This feature adds validation evidence rather than a new legacy capability; it supports existing SCSI/data durability capability rows in the legacy matrices.
- **Gap Policy**: No missing capability is introduced. E2E coverage is additive and gated; failures should reveal gaps rather than change runtime behavior.

### Observability, Audit & Security

- **Observability**: Add cache invalidation failure logs/counters and E2E command evidence output. No new audit event is required for internal data-plane cache state.
- **Auditability**: E2E uses existing management APIs and should preserve existing audit behavior for publication operations if the API server is used.
- **Security & Rollback**: E2E requires explicit opt-in and privilege checks. Cache changes are rollback-safe by reverting to prior cache behavior; no secrets or new credentials are introduced.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: The cache error model document is complete enough that Claude/Codex review can verify post-commit invalidation semantics without reading code.
- **SC-002**: A data-plane regression test proves post-commit cache invalidation failure does not make a committed write return failure.
- **SC-003**: A data-plane regression test proves stale cached data is not returned after injected invalidation failure.
- **SC-004**: Cache invalidation failures are visible in a bounded counter or status object that tests can assert.
- **SC-005**: The E2E harness has a documented opt-in command and prerequisite checks that skip/error safely on non-Linux or unprivileged hosts.
- **SC-006**: On a prepared Linux host, the E2E harness documents the exact sequence for publish/start, discovery/login, `sg_inq`, `sg_readcap`, and read/write smoke.
- **SC-007**: Relevant `cargo test`, shell guard checks, and generated task checklists pass.

## Assumptions

- Batch A/B PR is separate and not required to be merged before this feature is reviewed.
- The E2E harness may be implemented as scripts plus a Makefile target rather than a normal CI job.
- A simple read/write smoke may use common Linux block/SCSI tools available on the approved runner; if unavailable, the harness must clearly report the missing tool.
- No persisted data-plane format migration is intended for the cache guardrail.
