use std::fs;
use std::time::{SystemTime, UNIX_EPOCH};

use super::blk_map::{load_blk_map_records, BlkMapState};
use super::compaction::compact_segment;
use super::compression::CompressionCodec;
use super::data_path::{
    current_checkpoint, mark_checkpoint_dirty, read_logical_block, recover_dirty_state,
    write_logical_block, WriteOptions,
};
use super::layout::{initialize_layout, LayoutPaths};
use super::metadata::CheckpointFlags;
use super::reclaim::{load_reclaim_candidates, ReclaimReason};
use super::segment_index::{
    data_segment_path, load_segment_index, persist_segment_index, SegmentState,
};

fn test_paths(name: &str) -> LayoutPaths {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("clock")
        .as_nanos();
    let root = std::env::temp_dir().join(format!("holo-compaction-{name}-{nanos}"));
    fs::create_dir_all(&root).expect("create root");
    LayoutPaths::for_cartridge(&root, "drive-a", "cart-a")
}

#[test]
fn compact_segment_moves_live_records_and_preserves_reads() {
    let paths = test_paths("basic");
    initialize_layout(&paths).expect("layout init");
    set_segment_size(&paths, 256);

    let first = vec![b'A'; 180];
    let second = vec![b'B'; 180];
    let options = WriteOptions {
        dedup_enabled: false,
        preferred_codec: CompressionCodec::None,
        force_sync: true,
        payload_checksum_enabled: true,
    };
    write_logical_block(&paths, 0, &first, 0, options, None).expect("first write");
    write_logical_block(&paths, 1000, &second, 0, options, None).expect("second write");
    assert!(data_segment_path(&paths, 0).exists());
    assert!(data_segment_path(&paths, 1).exists());

    let report = compact_segment(&paths, 0, CompressionCodec::Zlib).expect("compact segment");

    assert_eq!(report.source_seq, 0);
    assert_eq!(report.blobs_moved, 1);
    assert!(report.bytes_reclaimed > 0);
    assert!(!data_segment_path(&paths, 0).exists());
    let index = load_segment_index(&paths.segment_index_file).expect("load index");
    assert!(index
        .descriptors
        .iter()
        .all(|descriptor| descriptor.segment_seq != 0));

    let read_first = read_logical_block(&paths, 0)
        .expect("read first")
        .expect("first present");
    let read_second = read_logical_block(&paths, 1000)
        .expect("read second")
        .expect("second present");
    assert_eq!(read_first.payload, first);
    assert_eq!(read_second.payload, second);

    let reclaim = load_reclaim_candidates(&paths.reclaim_file).expect("load reclaim");
    assert!(reclaim
        .iter()
        .any(|candidate| candidate.reason == ReclaimReason::Compacted));
}

#[test]
fn compact_segment_upgrades_lz4_blobs_to_zlib() {
    let paths = test_paths("codec-upgrade");
    initialize_layout(&paths).expect("layout init");
    set_segment_size(&paths, 32);

    let first = patterned_payload(2048, 4);
    let second = patterned_payload(2048, 5);
    let third = patterned_payload(2048, 6);
    let options = WriteOptions {
        dedup_enabled: false,
        preferred_codec: CompressionCodec::Lz4,
        force_sync: true,
        payload_checksum_enabled: true,
    };
    assert_eq!(
        write_logical_block(&paths, 0, &first, 0, options, None)
            .expect("first write")
            .codec_used,
        CompressionCodec::Lz4
    );
    assert_eq!(
        write_logical_block(&paths, 3000, &second, 0, options, None)
            .expect("second write")
            .codec_used,
        CompressionCodec::Lz4
    );
    write_logical_block(&paths, 6000, &third, 0, options, None).expect("third write");

    let source_seq = first_sealed_segment(&paths);
    let source_logical_starts = active_logical_starts_in_segment(&paths, source_seq);
    let report =
        compact_segment(&paths, source_seq, CompressionCodec::Zlib).expect("compact segment");

    assert!(report.codec_upgrades > 0);
    assert_eq!(
        read_logical_block(&paths, 0)
            .expect("read first")
            .expect("first present")
            .payload,
        first
    );
    let (_seq, records) = load_blk_map_records(&paths.blk_map_file).expect("load blk map");
    let active_source_records = records
        .iter()
        .filter(|record| {
            record.state == BlkMapState::Active
                && source_logical_starts.contains(&record.logical_start)
                && record.physical_segment_id != source_seq as u64
        })
        .collect::<Vec<_>>();
    assert!(!active_source_records.is_empty());
    assert!(active_source_records
        .iter()
        .all(|record| record.compression == CompressionCodec::Zlib));
}

#[test]
fn compact_segment_preserves_rle_when_requested() {
    let paths = test_paths("rle-preserve");
    initialize_layout(&paths).expect("layout init");
    set_segment_size(&paths, 29);

    let first = vec![b'R'; 512];
    let second = vec![b'S'; 512];
    let third = vec![b'T'; 512];
    let options = WriteOptions {
        dedup_enabled: false,
        preferred_codec: CompressionCodec::Rle,
        force_sync: true,
        payload_checksum_enabled: true,
    };
    write_logical_block(&paths, 0, &first, 0, options, None).expect("first write");
    write_logical_block(&paths, 1000, &second, 0, options, None).expect("second write");
    write_logical_block(&paths, 2000, &third, 0, options, None).expect("third write");

    let source_seq = first_sealed_segment(&paths);
    compact_segment(&paths, source_seq, CompressionCodec::Rle).expect("compact segment");

    let read_first = read_logical_block(&paths, 0)
        .expect("read first")
        .expect("first present");
    assert_eq!(read_first.payload, first);
    assert_eq!(read_first.codec_used, CompressionCodec::Rle);
    let (_seq, records) = load_blk_map_records(&paths.blk_map_file).expect("load blk map");
    assert!(records.iter().any(|record| {
        record.state == BlkMapState::Active
            && record.logical_start == 0
            && record.compression == CompressionCodec::Rle
    }));
}

#[test]
fn compact_segment_rejects_active_source() {
    let paths = test_paths("active");
    initialize_layout(&paths).expect("layout init");

    let err = compact_segment(&paths, 0, CompressionCodec::Zlib)
        .expect_err("active source must be rejected");
    assert!(format!("{err}").contains("cannot compact active"));
}

#[test]
fn recover_dirty_compacting_segment_rebuilds_clean_index() {
    let paths = test_paths("recover-compacting");
    initialize_layout(&paths).expect("layout init");
    set_segment_size(&paths, 128);

    let options = WriteOptions {
        dedup_enabled: false,
        preferred_codec: CompressionCodec::None,
        force_sync: true,
        payload_checksum_enabled: true,
    };
    write_logical_block(&paths, 0, &[b'A'; 96], 0, options, None).expect("first write");
    write_logical_block(&paths, 200, &[b'B'; 96], 0, options, None).expect("second write");
    let source_seq = first_sealed_segment(&paths);

    let _ = mark_checkpoint_dirty(&paths).expect("mark dirty");
    let mut index = load_segment_index(&paths.segment_index_file).expect("load index");
    index
        .descriptors
        .iter_mut()
        .find(|descriptor| descriptor.segment_seq == source_seq)
        .expect("source descriptor")
        .state = SegmentState::Compacting;
    persist_segment_index(&paths.segment_index_file, &index).expect("persist compacting index");

    let recovery = recover_dirty_state(&paths).expect("recover dirty compacting state");
    assert!(recovery.dirty_detected);
    let checkpoint = current_checkpoint(&paths).expect("load checkpoint");
    assert_eq!(checkpoint.flags, CheckpointFlags::Clean);
    let rebuilt = load_segment_index(&paths.segment_index_file).expect("load rebuilt index");
    assert!(rebuilt
        .descriptors
        .iter()
        .all(|descriptor| descriptor.state != SegmentState::Compacting));
    assert_eq!(
        read_logical_block(&paths, 0)
            .expect("read first")
            .expect("first present")
            .payload,
        vec![b'A'; 96]
    );
}

fn patterned_payload(len: usize, period: usize) -> Vec<u8> {
    (0..len).map(|i| b'A' + (i % period) as u8).collect()
}

fn first_sealed_segment(paths: &LayoutPaths) -> u32 {
    load_segment_index(&paths.segment_index_file)
        .expect("load index")
        .descriptors
        .into_iter()
        .find(|descriptor| descriptor.state == SegmentState::Sealed)
        .expect("expected at least one sealed segment")
        .segment_seq
}

fn active_logical_starts_in_segment(paths: &LayoutPaths, segment_seq: u32) -> Vec<u64> {
    let (_seq, records) = load_blk_map_records(&paths.blk_map_file).expect("load blk map");
    records
        .into_iter()
        .filter(|record| {
            record.state == BlkMapState::Active && record.physical_segment_id == segment_seq as u64
        })
        .map(|record| record.logical_start)
        .collect()
}

fn set_segment_size(paths: &LayoutPaths, max_segment_size: u32) {
    let mut index = load_segment_index(&paths.segment_index_file).expect("load index");
    index.max_segment_size = max_segment_size;
    persist_segment_index(&paths.segment_index_file, &index).expect("persist index");
}
