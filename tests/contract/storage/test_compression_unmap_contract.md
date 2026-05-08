# Contract Test: Compression and UNMAP

## Purpose
Validate Spec-B-02 compression switch and UNMAP lifecycle contract.

## Checks
1. Compression-enabled writes MUST be readable back with exact payload parity.
2. Compression-disabled writes MUST stay pass-through.
3. UNMAP MUST stale affected blk_map records and update reclaim safety.
4. UNMAP on dedup-backed records MUST decrement dedup refcounts.
