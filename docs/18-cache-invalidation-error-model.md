# Cache Invalidation Error Model

This model defines how Holo handles internal cache cleanup failures around durable tape writes. It exists to keep SCSI protocol status tied to durable media state, not to best-effort cache maintenance.

## Write Outcome Boundaries

### Before Commit

If validation, authorization, WORM enforcement, capacity checks, segment append, map update, checkpoint update, or other durability work fails before the write is committed, the write may fail normally. The initiator can receive CHECK CONDITION or another existing error response because the requested data is not known to be durable.

### During Commit

If the data-plane cannot prove that data and required metadata reached the cartridge layout consistently, the command must fail through the existing storage error path. Recovery remains responsible for dirty checkpoint handling and map repair.

### After Commit

If the write has committed successfully and an internal cache invalidation or cleanup step fails afterward, the data-plane must not report the SCSI write as failed solely because of that cleanup failure. Asking the host to retry a write that is already durable can duplicate data or move tape position incorrectly.

## Cache Families

### Read Prefetch

Read prefetch is an optional in-memory optimization. It may hold future block reads keyed by cartridge layout root. Mutating commands that can change the readable view must invalidate this cache.

If invalidation fails:

- The affected layout root enters degraded prefetch state.
- Existing prefetched jobs for that root are not trusted for subsequent reads.
- Future prefetch scheduling for that root is bypassed.
- Direct reads continue through the authoritative storage map.
- A bounded in-memory failure counter and deterministic log line record the event.

### Storage Map Caches

`blk_map`, `lookup`, and `dedup` caches mirror authoritative segment files and update during their own write operations. Failures in those update paths are commit-path failures unless the caller has already recorded a durable state and intentionally treats the cache update as best-effort. This feature does not change persisted map semantics.

## Observability

Each read prefetch invalidation failure emits a log line containing the operation, layout root, failure count, and safe error string. Tests may assert the same bounded runtime status through data-plane helpers.

Counters are saturating. Repeated failures keep the cache degraded and do not allocate unbounded memory.

## Safe Degradation

The safe degraded behavior is bypass, not retry-looping invalidation. Bypass avoids stale cached reads while preserving the durable data path:

1. Do not consume prefetched reads for a degraded layout.
2. Do not schedule new prefetch jobs for a degraded layout.
3. Continue direct reads from storage metadata.
4. Clear degraded state when the layout cache is explicitly discarded or the media is unloaded.

## Rollback

Rollback is code-only. No persisted data format, SQLite schema, audit schema, or cartridge metadata changes are introduced. Reverting this feature restores previous in-memory prefetch behavior, while existing cartridge data remains compatible.

## Review Checklist

- Write-before failures can still fail SCSI commands.
- Post-commit invalidation failures do not turn durable writes into failed SCSI writes.
- Stale prefetched data is bypassed after invalidation failure.
- Repeated failures remain observable and bounded.
- Recovery and rollback require no data migration.
