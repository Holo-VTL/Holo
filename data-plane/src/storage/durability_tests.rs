use std::fs;

use super::blk_map::BlkMapRecord;
use super::compression::CompressionCodec;
use super::layout::{initialize_layout, LayoutPaths};
use super::map_lookup::MapLookupRecord;
use super::metadata::{
    flush_chain, load_checkpoint_page, quarantine_invalid_metadata, CheckpointFlags, FlushStep,
    MetadataCheckpoint,
};

fn test_paths(case: &str) -> LayoutPaths {
    let root = std::env::temp_dir().join(format!("holo-storage-tests-{case}"));
    let _ = fs::remove_dir_all(&root);
    LayoutPaths {
        root: root.clone(),
        data_file: root.join("data.segment"),
        metadata_file: root.join("metadata.segment"),
        blk_map_file: root.join("blk_map.segment"),
        lookup_file: root.join("lookup.segment"),
        reclaim_file: root.join("reclaim.segment"),
        dedup_file: root.join("dedup.segment"),
        segment_index_file: root.join("segment_index.segment"),
    }
}

#[test]
fn flush_chain_respects_deterministic_order() {
    let paths = test_paths("flush-order");
    initialize_layout(&paths).expect("layout init should pass");

    let checkpoint = MetadataCheckpoint {
        page_id: 2,
        epoch: 2,
        active_blk_map_root: 2,
        active_lookup_root: 3,
        flags: CheckpointFlags::Clean,
    };

    let blk_payload = {
        let rec = BlkMapRecord {
            record_id: 1,
            logical_start: 0,
            logical_len: 8,
            physical_segment_id: 1,
            physical_offset: 0,
            filemark_count: 0,
            state: super::blk_map::BlkMapState::Active,
            dedup_entry_id: 0,
            compression: CompressionCodec::None,
            compressed_len: 8,
            payload_checksum: 0,
        };
        let mut out = Vec::new();
        out.extend_from_slice(&1u64.to_le_bytes());
        out.extend_from_slice(&rec.encode());
        out
    };

    let lookup_payload = {
        let rec = MapLookupRecord {
            lookup_id: 1,
            logical_min: 0,
            logical_max: 7,
            blk_map_ref_start: 1,
            blk_map_ref_end: 1,
        };
        let mut out = Vec::new();
        out.extend_from_slice(&1u64.to_le_bytes());
        out.extend_from_slice(&rec.encode());
        out
    };

    let report = flush_chain(
        &paths.data_file,
        &paths.blk_map_file,
        &paths.lookup_file,
        &paths.metadata_file,
        b"abc",
        &blk_payload,
        &lookup_payload,
        &checkpoint,
        None,
    )
    .expect("flush chain should pass");

    assert_eq!(
        report.steps,
        vec![
            FlushStep::Data,
            FlushStep::BlkMap,
            FlushStep::Lookup,
            FlushStep::Metadata
        ]
    );

    let loaded = load_checkpoint_page(&paths.metadata_file).expect("checkpoint load should pass");
    assert_eq!(loaded.epoch, 2);
}

#[test]
fn quarantine_invalid_metadata_file() {
    let paths = test_paths("quarantine");
    initialize_layout(&paths).expect("layout init should pass");

    let mut bytes = fs::read(&paths.metadata_file).expect("metadata file should exist");
    let len = bytes.len();
    bytes[len - 1] ^= 0x33;
    fs::write(&paths.metadata_file, bytes).expect("rewrite metadata should pass");

    let quarantined =
        quarantine_invalid_metadata(&paths.metadata_file).expect("quarantine should pass");
    assert!(quarantined.exists());
    assert!(!paths.metadata_file.exists());
}

#[test]
fn flush_chain_failpoint_stops_before_metadata() {
    let paths = test_paths("flush-failpoint");
    initialize_layout(&paths).expect("layout init should pass");

    let checkpoint = MetadataCheckpoint {
        page_id: 3,
        epoch: 3,
        active_blk_map_root: 2,
        active_lookup_root: 3,
        flags: CheckpointFlags::Dirty,
    };

    let err = flush_chain(
        &paths.data_file,
        &paths.blk_map_file,
        &paths.lookup_file,
        &paths.metadata_file,
        b"payload",
        &0u64.to_le_bytes(),
        &0u64.to_le_bytes(),
        &checkpoint,
        Some(FlushStep::Lookup),
    )
    .expect_err("failpoint should fail");

    assert!(format!("{err}").contains("flush interrupted"));
    let loaded =
        load_checkpoint_page(&paths.metadata_file).expect("checkpoint load should still pass");
    assert_eq!(loaded.epoch, 1);
}
