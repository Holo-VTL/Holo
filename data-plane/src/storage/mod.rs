pub mod blk_map;
pub mod compaction;
pub mod compression;
pub mod data_path;
pub mod dedup;
pub mod layout;
pub mod map_lookup;
pub mod metadata;
pub mod reclaim;
pub mod runtime_state;
pub mod segment;
pub mod segment_index;

pub use blk_map::{
    append_blk_map_record, load_blk_map_records, mark_blk_map_stale, mark_blk_map_stale_batch,
    persist_blk_map_records, BlkMapRecord, BlkMapState,
};
pub use compaction::{compact_segment, CompactionReport};
pub use compression::{compress_payload, decompress_payload, CompressionCodec, CompressionStats};
pub use data_path::{
    current_checkpoint, current_dedup_refcounts, discard_layout_caches, flush_pending_writes,
    read_logical_block, recover_dirty_state, reset_layout_for_overwrite, run_unmap,
    write_logical_block, IngestFailpoint, LogicalReadResult, RecoveryReport, UnmapReport,
    WriteOptions, WriteReport,
};
pub use dedup::{
    decrement_ref_counts, fingerprint128, load_dedup_index, persist_dedup_entries,
    rebuild_ref_counts, DedupIndexEntry, DedupLookup, DedupUpsertResult,
};
pub use layout::{
    bootstrap_for_mount, initialize_layout, load_layout, LayoutPaths, LayoutSnapshot,
    SegmentHeader, SegmentKind, STORAGE_LAYOUT_MAGIC, STORAGE_LAYOUT_VERSION,
};
pub use map_lookup::{
    append_lookup_record, load_lookup_records, locate_logical_block, rebuild_lookup_from_blk_map,
    MapLookupRecord,
};
pub use metadata::{
    flush_chain, load_checkpoint_page, persist_checkpoint_page, storage_root_dir, CheckpointFlags,
    MetadataCheckpoint, StorageError,
};
pub use reclaim::{
    load_reclaim_candidates, refresh_reclaim_safety, upsert_reclaim_candidate,
    upsert_reclaim_candidates, ReclaimCandidate, ReclaimReason,
};
pub use runtime_state::{
    load_filemarks, load_retention_state, persist_filemarks, persist_retention_state,
    RetentionRuntimeState,
};
pub use segment::{read_segment_file, write_segment_file};
pub use segment_index::{
    active_segment_path, data_segment_path, load_segment_index, persist_segment_index,
    SegmentDescriptor, SegmentIndex, SegmentState,
};

#[cfg(test)]
mod compaction_tests;
#[cfg(test)]
mod compression_tests;
#[cfg(test)]
mod dedup_tests;
#[cfg(test)]
mod durability_tests;
#[cfg(test)]
mod layout_tests;
#[cfg(test)]
mod map_chain_tests;
#[cfg(test)]
mod recovery_tests;
#[cfg(test)]
mod segment_index_tests;
#[cfg(test)]
mod segment_tests;
