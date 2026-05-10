# Research: Cache Invalidation And Gated SCSI E2E

## Decision: Treat Post-Commit Cache Invalidation Failure As Degraded Cache Health

**Rationale**: Once the tape write has reached durable storage and metadata state, returning SCSI CHECK CONDITION solely because cleanup of an internal read cache failed can cause initiators to retry already-committed data. The protocol response should reflect the durable write outcome, while the cache path must degrade safely and become observable.

**Alternatives considered**:
- Fail the SCSI write when invalidation fails: rejected because it confuses post-commit cleanup with write durability and risks duplicate writes.
- Ignore invalidation failure after logging once: rejected because stale cache reuse would remain possible.

## Decision: Bypass Read Prefetch After Invalidation Failure

**Rationale**: The current stale-read risk is the read prefetch queue. The smallest safe behavior is to mark the layout root degraded, count the failure, avoid consuming queued prefetched reads, and stop scheduling new prefetches for that root until cache state is explicitly reset by unload/discard. Normal direct reads continue through storage metadata.

**Alternatives considered**:
- Persist a cache poison marker: rejected because no persisted format migration is required.
- Rebuild all storage indexes immediately: rejected as over-broad for an in-memory prefetch failure.

## Decision: Keep Observability Bounded And Testable

**Rationale**: Repeated invalidation failures must not allocate unbounded memory. A per-layout counter/status map keyed by layout root is sufficient for tests and operator logs; counters saturate and state is cleared when layout caches are discarded.

**Alternatives considered**:
- Emit only logs: rejected because tests need deterministic state.
- Add metrics exporter integration now: rejected as larger than the review-fix scope.

## Decision: Gated E2E Is A Script With Dry-Run/Prerequisite Modes

**Rationale**: Real `open-iscsi` and `sg_*` validation requires root and mutates host initiator state. A script can be explicit about opt-in, prerequisites, evidence, and cleanup while staying out of normal CI.

**Alternatives considered**:
- Add to default CI: rejected because normal PR hosts are not privileged and should not mutate iSCSI state.
- Write a Go/Rust test harness: rejected because shell commands are the interface under validation and Bash keeps the release check auditable.
