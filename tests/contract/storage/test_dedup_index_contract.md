# Contract Test: Dedup Index and Refcount

## Purpose
Validate Spec-B-02 dedup identity and reference counting contract.

## Checks
1. Identical payload writes MUST reuse existing dedup entry and increment `ref_count`.
2. Fingerprint collision with mismatched payload identity MUST create isolated entry.
3. Recovery rebuild MUST derive refcounts from active blk_map records.
