# Integration Scenario: UNMAP Reclaim

1. Write mapped payload range.
2. Execute UNMAP on overlapping logical range.
3. Verify blk_map entries become stale and lookup is rebuilt.
4. Verify dedup refcount decrement and reclaim candidate safety refresh.
