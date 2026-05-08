# Contract Test: blk_map/map_lookup Chain

## Scope
- `data-plane/src/storage/blk_map.rs`
- `data-plane/src/storage/map_lookup.rs`

## Assertions
- blk_map append rejects active logical-range overlap
- lookup locate resolves logical block to expected blk_map reference
- lookup rebuild from blk_map preserves active mappings after restart
