use std::fs;

use super::blk_map::{append_blk_map_record, load_blk_map_records, BlkMapRecord, BlkMapState};
use super::compression::CompressionCodec;
use super::data_path::{read_logical_block, write_logical_block, WriteOptions};
use super::layout::{initialize_layout, load_layout, LayoutPaths};
use super::map_lookup::{
    append_lookup_record, locate_logical_block, rebuild_lookup_from_blk_map, MapLookupRecord,
};
use super::reclaim::{refresh_reclaim_safety, upsert_reclaim_candidate, ReclaimReason};
use super::segment_index::data_segment_path;

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
fn appends_and_locates_map_chain_records() {
    let paths = test_paths("map-chain");
    initialize_layout(&paths).expect("layout init should pass");

    let rec1 = append_blk_map_record(
        &paths.blk_map_file,
        BlkMapRecord {
            record_id: 0,
            logical_start: 0,
            logical_len: 64,
            physical_segment_id: 10,
            physical_offset: 0,
            filemark_count: 0,
            state: BlkMapState::Active,
            dedup_entry_id: 0,
            compression: CompressionCodec::None,
            compressed_len: 64,
            payload_checksum: 0,
        },
    )
    .expect("append rec1 should pass");

    let rec2 = append_blk_map_record(
        &paths.blk_map_file,
        BlkMapRecord {
            record_id: 0,
            logical_start: 64,
            logical_len: 64,
            physical_segment_id: 11,
            physical_offset: 0,
            filemark_count: 1,
            state: BlkMapState::Active,
            dedup_entry_id: 0,
            compression: CompressionCodec::None,
            compressed_len: 64,
            payload_checksum: 0,
        },
    )
    .expect("append rec2 should pass");

    append_lookup_record(
        &paths.lookup_file,
        MapLookupRecord {
            lookup_id: 0,
            logical_min: rec1.logical_start,
            logical_max: rec1.logical_end() - 1,
            blk_map_ref_start: rec1.record_id,
            blk_map_ref_end: rec1.record_id,
        },
    )
    .expect("append lookup rec1 should pass");

    append_lookup_record(
        &paths.lookup_file,
        MapLookupRecord {
            lookup_id: 0,
            logical_min: rec2.logical_start,
            logical_max: rec2.logical_end() - 1,
            blk_map_ref_start: rec2.record_id,
            blk_map_ref_end: rec2.record_id,
        },
    )
    .expect("append lookup rec2 should pass");

    let located = locate_logical_block(&paths.lookup_file, 96)
        .expect("lookup should pass")
        .expect("record should exist");
    assert_eq!(located.blk_map_ref_start, rec2.record_id);
}

#[test]
fn rebuilds_lookup_after_restart() {
    let paths = test_paths("map-rebuild");
    initialize_layout(&paths).expect("layout init should pass");

    append_blk_map_record(
        &paths.blk_map_file,
        BlkMapRecord {
            record_id: 0,
            logical_start: 128,
            logical_len: 32,
            physical_segment_id: 20,
            physical_offset: 512,
            filemark_count: 0,
            state: BlkMapState::Active,
            dedup_entry_id: 0,
            compression: CompressionCodec::None,
            compressed_len: 32,
            payload_checksum: 0,
        },
    )
    .expect("append blk map should pass");

    let rebuilt = rebuild_lookup_from_blk_map(&paths.blk_map_file, &paths.lookup_file)
        .expect("rebuild lookup should pass");
    assert_eq!(rebuilt, 1);

    let _snapshot = load_layout(&paths).expect("layout reload should pass");
    let located = locate_logical_block(&paths.lookup_file, 129)
        .expect("locate should pass")
        .expect("lookup record should exist");
    assert_eq!(located.logical_min, 128);
}

#[test]
fn rebuild_lookup_rejects_zero_length_active_record() {
    let paths = test_paths("map-zero-len");
    initialize_layout(&paths).expect("layout init should pass");

    append_blk_map_record(
        &paths.blk_map_file,
        BlkMapRecord {
            record_id: 0,
            logical_start: 128,
            logical_len: 0,
            physical_segment_id: 20,
            physical_offset: 512,
            filemark_count: 0,
            state: BlkMapState::Active,
            dedup_entry_id: 0,
            compression: CompressionCodec::None,
            compressed_len: 0,
            payload_checksum: 0,
        },
    )
    .expect("append blk map should pass");

    let err = rebuild_lookup_from_blk_map(&paths.blk_map_file, &paths.lookup_file)
        .expect_err("zero-length active range must fail");
    assert!(format!("{err}").contains("zero logical length"));
}

#[test]
fn marks_reclaim_candidate_and_refreshes_safety() {
    let paths = test_paths("reclaim");
    initialize_layout(&paths).expect("layout init should pass");

    let rec = append_blk_map_record(
        &paths.blk_map_file,
        BlkMapRecord {
            record_id: 0,
            logical_start: 10,
            logical_len: 10,
            physical_segment_id: 3,
            physical_offset: 12,
            filemark_count: 0,
            state: BlkMapState::Active,
            dedup_entry_id: 0,
            compression: CompressionCodec::None,
            compressed_len: 10,
            payload_checksum: 0,
        },
    )
    .expect("append blk map should pass");

    append_lookup_record(
        &paths.lookup_file,
        MapLookupRecord {
            lookup_id: 0,
            logical_min: 10,
            logical_max: 19,
            blk_map_ref_start: rec.record_id,
            blk_map_ref_end: rec.record_id,
        },
    )
    .expect("append lookup should pass");

    let candidate = upsert_reclaim_candidate(
        &paths.blk_map_file,
        &paths.lookup_file,
        &paths.reclaim_file,
        rec.record_id,
        ReclaimReason::Superseded,
    )
    .expect("upsert reclaim candidate should pass");
    assert!(!candidate.safe_to_reclaim);

    rebuild_lookup_from_blk_map(&paths.blk_map_file, &paths.lookup_file)
        .expect("rebuild lookup should pass");
    let updated = refresh_reclaim_safety(&paths.lookup_file, &paths.reclaim_file)
        .expect("refresh reclaim safety should pass");
    assert_eq!(updated, 1);

    let (_seq, records) =
        load_blk_map_records(&paths.blk_map_file).expect("load blk maps should pass");
    assert!(records
        .iter()
        .any(|r| r.record_id == rec.record_id && r.state == BlkMapState::Stale));
}

#[test]
fn read_path_refreshes_on_disk_state_when_cache_was_primed() {
    let stale_paths = test_paths("cache-refresh-stale");
    let fresh_paths = test_paths("cache-refresh-fresh");
    initialize_layout(&stale_paths).expect("stale layout init should pass");
    initialize_layout(&fresh_paths).expect("fresh layout init should pass");

    let payload = b"cache-refresh-payload";
    write_logical_block(
        &fresh_paths,
        128,
        payload,
        0,
        WriteOptions::throughput_default(),
        None,
    )
    .expect("write logical block should pass");

    let located_before =
        locate_logical_block(&stale_paths.lookup_file, 128).expect("lookup should pass");
    assert!(located_before.is_none());

    fs::copy(
        data_segment_path(&fresh_paths, 0),
        data_segment_path(&stale_paths, 0),
    )
    .expect("copy data segment");
    fs::copy(
        &fresh_paths.segment_index_file,
        &stale_paths.segment_index_file,
    )
    .expect("copy segment index");
    fs::copy(&fresh_paths.blk_map_file, &stale_paths.blk_map_file).expect("copy blk map segment");
    fs::copy(&fresh_paths.lookup_file, &stale_paths.lookup_file).expect("copy lookup segment");

    let read = read_logical_block(&stale_paths, 128)
        .expect("read logical block should pass")
        .expect("logical block should exist after external rewrite");
    assert_eq!(read.logical_start, 128);
    assert_eq!(read.payload, payload);
}
