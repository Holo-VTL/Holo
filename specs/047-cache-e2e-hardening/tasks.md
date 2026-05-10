# Tasks: Cache Invalidation And Gated SCSI E2E

**Input**: Design documents from `/Users/lei/AI_CC_Home/vtlix/specs/047-cache-e2e-hardening/`
**Prerequisites**: `plan.md`, `spec.md`, `research.md`, `data-model.md`, `contracts/gated-scsi-e2e.md`, `quickstart.md`

## Phase 1: Setup

- [X] T001 Create cache invalidation error model document in `/Users/lei/AI_CC_Home/vtlix/docs/18-cache-invalidation-error-model.md`
- [X] T002 [P] Add gated SCSI E2E contract references to `/Users/lei/AI_CC_Home/vtlix/specs/047-cache-e2e-hardening/quickstart.md`

## Phase 2: Foundational

- [X] T003 Add read prefetch degradation status/counter helpers in `/Users/lei/AI_CC_Home/vtlix/data-plane/src/scsi_tape/command_chain.rs`
- [X] T004 Add test-only read prefetch invalidation failure injection in `/Users/lei/AI_CC_Home/vtlix/data-plane/src/scsi_tape/command_chain.rs`

## Phase 3: User Story 1 - Define Cache Failure Semantics (P1)

**Goal**: Maintainers can review write-before, write-after, post-commit invalidation, cache discard/bypass, observability, and rollback behavior without reading code.

**Independent Test**: Review `/Users/lei/AI_CC_Home/vtlix/docs/18-cache-invalidation-error-model.md`.

- [X] T005 [US1] Document write-before/write-after/post-commit semantics in `/Users/lei/AI_CC_Home/vtlix/docs/18-cache-invalidation-error-model.md`
- [X] T006 [US1] Document observability, safe degradation, and rollback semantics in `/Users/lei/AI_CC_Home/vtlix/docs/18-cache-invalidation-error-model.md`

## Phase 4: User Story 2 - Implement Cache Invalidation Guardrails (P1)

**Goal**: Committed writes stay successful when read prefetch invalidation degrades, stale prefetch is bypassed, and counters are assertable.

**Independent Test**: Run targeted `cargo test` cases from `quickstart.md`.

- [X] T007 [P] [US2] Add regression test for committed fixed-block write success after injected prefetch invalidation failure in `/Users/lei/AI_CC_Home/vtlix/data-plane/src/scsi_tape/command_chain_tests.rs`
- [X] T008 [P] [US2] Add regression test proving degraded prefetch bypass avoids stale cached reads in `/Users/lei/AI_CC_Home/vtlix/data-plane/src/scsi_tape/command_chain_tests.rs`
- [X] T009 [US2] Wire invalidation failure handling so writes do not fail solely due to prefetch cleanup in `/Users/lei/AI_CC_Home/vtlix/data-plane/src/scsi_tape/command_chain.rs`
- [X] T010 [US2] Wire degraded prefetch bypass for take/schedule paths in `/Users/lei/AI_CC_Home/vtlix/data-plane/src/scsi_tape/command_chain.rs`
- [X] T011 [US2] Expose bounded test-visible prefetch degradation status in `/Users/lei/AI_CC_Home/vtlix/data-plane/src/scsi_tape/command_chain.rs`

## Phase 5: User Story 3 - Provide Gated Real SCSI E2E Harness (P2)

**Goal**: Maintainers have an opt-in Linux privileged real initiator smoke harness with safe prerequisite checks and cleanup.

**Independent Test**: Run `/Users/lei/AI_CC_Home/vtlix/tests/integration/test_gated_scsi_e2e.sh` on a normal dev host and run the privileged command from `quickstart.md` on an approved Linux host.

- [X] T012 [P] [US3] Add shell tests for default gating and check-only behavior in `/Users/lei/AI_CC_Home/vtlix/tests/integration/test_gated_scsi_e2e.sh`
- [X] T013 [US3] Implement opt-in, Linux, root, and command prerequisite checks in `/Users/lei/AI_CC_Home/vtlix/scripts/gated-scsi-e2e.sh`
- [X] T014 [US3] Implement discovery/login, `sg_inq`, `sg_readcap`, optional write-read smoke, and cleanup trap in `/Users/lei/AI_CC_Home/vtlix/scripts/gated-scsi-e2e.sh`
- [X] T015 [US3] Document privileged release invocation and normal CI exclusion in `/Users/lei/AI_CC_Home/vtlix/specs/047-cache-e2e-hardening/quickstart.md`

## Phase 6: Polish & Cross-Cutting

- [X] T016 Update legacy capability evidence in `/Users/lei/AI_CC_Home/vtlix/docs/08-legacy-capability-matrix.md`
- [X] T017 Run `cd /Users/lei/AI_CC_Home/vtlix/data-plane && cargo test`
- [X] T018 Run `bash /Users/lei/AI_CC_Home/vtlix/tests/integration/test_gated_scsi_e2e.sh`
- [X] T019 Run `bash /Users/lei/AI_CC_Home/vtlix/scripts/lint-guardrails.sh`
- [X] T020 Add `e2e-scsi` release target in `/Users/lei/AI_CC_Home/vtlix/Makefile`
- [X] T021 Add gated GitHub Actions workflow in `/Users/lei/AI_CC_Home/vtlix/.github/workflows/e2e-scsi.yml`

## Dependencies

- Phase 1 and Phase 2 complete before user stories.
- US1 completes before US2 implementation review.
- US2 and US3 are independent after foundational tasks.
- Polish tasks require implementation tasks complete.

## Parallel Execution Examples

- T001 and T002 can run in parallel.
- T007 and T008 can be drafted together because they target independent test cases in the same test module, then reconciled before implementation.
- T012 can run before T013/T014 because it defines safe shell behavior first.

## Implementation Strategy

1. Complete US1 documentation.
2. Add failing US2 tests, implement data-plane guardrails, run targeted cargo tests.
3. Add failing US3 shell checks, implement the gated harness, run shell tests.
4. Update legacy evidence and run the full validation set.
