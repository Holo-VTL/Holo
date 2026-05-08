# Contract Test: Durability Flush Order

## Scope
- `data-plane/src/storage/metadata.rs`
- `data-plane/src/storage/segment.rs`

## Assertions
- flush order is deterministic: `data -> blk_map -> lookup -> metadata`
- failpoint interruption does not silently promote incomplete metadata checkpoint
- invalid metadata can be quarantined and not activated
