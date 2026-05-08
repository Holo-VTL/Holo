# Contract Test: Storage Layout Headers

## Scope
- `data-plane/src/storage/layout.rs`
- Segment header contract for data/metadata/blk_map/lookup/reclaim files

## Assertions
- header includes `magic/version/kind/segment_id/sequence/payload_len/checksum`
- loader rejects invalid `magic` or `version`
- checksum mismatch must fail with safe error
