# Contract Test: Dirty Checkpoint Crash Recovery

## Purpose
Validate Spec-B-02 crash recovery contract for dirty checkpoints.

## Checks
1. Write critical section MUST set checkpoint to dirty before final commit.
2. Recovery MUST rebuild lookup and dedup refcounts from durable active mappings.
3. Recovery completion MUST persist clean checkpoint state.
4. Corrupt metadata MUST trigger quarantine-safe failure behavior.
