# Integration Test: Reclaim Candidate Marking

## Steps
1. Append active blk_map entries and corresponding lookup references.
2. Mark one record as stale reclaim candidate.
3. Refresh reclaim safety after lookup rebuild.

## Expected
- stale record is tracked as reclaim candidate
- candidate is not reclaim-safe while referenced
- candidate becomes reclaim-safe after active references are removed
