use std::fs;

use super::blk_map::{
    load_blk_map_records, persist_blk_map_records, BlkMapState, PayloadChecksumAlgorithm,
};
use super::compression::CompressionCodec;
use super::data_path::{
    current_checkpoint, current_dedup_refcounts, discard_layout_caches, flush_pending_writes,
    mark_checkpoint_dirty, read_logical_block, recover_dirty_state, run_unmap, write_logical_block,
    IngestFailpoint, WriteOptions,
};
use super::dedup::{fingerprint128, load_dedup_index, persist_dedup_entries};
use super::layout::checksum32;
use super::layout::{initialize_layout, LayoutPaths};
use super::maintenance_progress::MaintenancePhase;
use super::metadata::CheckpointFlags;
use super::segment_index::{data_segment_path, load_segment_index, persist_segment_index};

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
fn write_and_read_roundtrip_with_dedup_reuse() {
    let paths = test_paths("write-read-dedup");
    initialize_layout(&paths).expect("layout init should pass");

    let options = WriteOptions {
        dedup_enabled: true,
        preferred_codec: CompressionCodec::Rle,
        force_sync: false,
        payload_checksum_enabled: true,
    };

    let payload = b"AAAAAAAAAABBBBBBBBBB";
    let first =
        write_logical_block(&paths, 0, payload, 0, options, None).expect("first write should pass");
    assert!(!first.dedup_hit);

    let second = write_logical_block(&paths, payload.len() as u64, payload, 0, options, None)
        .expect("second write should pass");
    assert!(second.dedup_hit);
    assert_eq!(second.dedup_entry_id, first.dedup_entry_id);

    let readback = read_logical_block(&paths, 2)
        .expect("read should pass")
        .expect("record should exist");
    assert_eq!(readback.payload, payload);

    let refs = current_dedup_refcounts(&paths).expect("dedup refs should load");
    assert_eq!(refs.len(), 1);
    assert_eq!(refs[0].1, 2);
}

#[test]
fn dedup_hit_verifies_candidate_payload_before_reuse() {
    let paths = test_paths("dedup-verify-candidate");
    initialize_layout(&paths).expect("layout init should pass");

    let options = WriteOptions {
        dedup_enabled: true,
        preferred_codec: CompressionCodec::None,
        force_sync: false,
        payload_checksum_enabled: true,
    };

    let first_payload = b"payload-one";
    let first = write_logical_block(&paths, 0, first_payload, 0, options, None)
        .expect("first write should pass");
    assert!(!first.dedup_hit);

    let second_payload = b"payload-two";
    let (_seq, mut entries) = load_dedup_index(&paths.dedup_file).expect("load dedup");
    assert_eq!(entries.len(), 1);
    let (fp_hi, fp_lo) = fingerprint128(second_payload);
    entries[0].fingerprint_hi = fp_hi;
    entries[0].fingerprint_lo = fp_lo;
    entries[0].payload_checksum = checksum32(second_payload);
    entries[0].logical_len = second_payload.len() as u32;
    entries[0].identity_version = 0;
    persist_dedup_entries(&paths.dedup_file, &entries).expect("persist mutated dedup entry");

    let second = write_logical_block(
        &paths,
        first_payload.len() as u64,
        second_payload,
        0,
        options,
        None,
    )
    .expect("second write should pass");
    assert!(
        !second.dedup_hit,
        "mismatched candidate must not be reused as a dedup hit"
    );
    assert_ne!(second.dedup_entry_id, first.dedup_entry_id);

    let readback = read_logical_block(&paths, first_payload.len() as u64)
        .expect("read should pass")
        .expect("record should exist");
    assert_eq!(readback.payload, second_payload);
}

#[test]
fn default_write_uses_lz4_and_reads_back() {
    let paths = test_paths("default-lz4-roundtrip");
    initialize_layout(&paths).expect("layout init should pass");

    let payload = vec![b'L'; 4096];
    let report = write_logical_block(&paths, 0, &payload, 0, WriteOptions::default(), None)
        .expect("write should pass");

    assert_eq!(report.codec_used, CompressionCodec::Lz4);
    let read = read_logical_block(&paths, 0)
        .expect("read should pass")
        .expect("block should exist");
    assert_eq!(read.payload, payload);
    assert_eq!(read.codec_used, CompressionCodec::Lz4);
}

#[test]
fn storage_format_v2_detects_data_blob_payload_corruption() {
    let paths = test_paths("v2-blob-corrupt");
    initialize_layout(&paths).expect("layout init should pass");

    let payload = b"blob-integrity-payload";
    write_logical_block(
        &paths,
        0,
        payload,
        0,
        WriteOptions {
            dedup_enabled: false,
            preferred_codec: CompressionCodec::None,
            force_sync: true,
            payload_checksum_enabled: false,
        },
        None,
    )
    .expect("write should pass");

    discard_layout_caches(&paths);
    let path = data_segment_path(&paths, 0);
    let mut bytes = fs::read(&path).expect("read data segment");
    let last = bytes.last_mut().expect("data segment has payload");
    *last ^= 0x5A;
    fs::write(&path, bytes).expect("rewrite corrupted segment");
    discard_layout_caches(&paths);

    let err = read_logical_block(&paths, 0).expect_err("corrupt blob must fail closed");
    assert!(format!("{err}").contains("data blob integrity checksum mismatch"));
}

#[test]
fn zlib_write_reads_back_through_data_path() {
    let paths = test_paths("zlib-roundtrip");
    initialize_layout(&paths).expect("layout init should pass");

    let payload = vec![b'Z'; 4096];
    let options = WriteOptions {
        dedup_enabled: false,
        preferred_codec: CompressionCodec::Zlib,
        force_sync: true,
        payload_checksum_enabled: true,
    };
    let report =
        write_logical_block(&paths, 0, &payload, 0, options, None).expect("write should pass");

    assert_eq!(report.codec_used, CompressionCodec::Zlib);
    let read = read_logical_block(&paths, 0)
        .expect("read should pass")
        .expect("block should exist");
    assert_eq!(read.payload, payload);
    assert_eq!(read.codec_used, CompressionCodec::Zlib);
}

#[test]
fn rotates_and_reads_across_data_segments() {
    let paths = test_paths("rotated-read");
    initialize_layout(&paths).expect("layout init should pass");
    set_segment_size(&paths, 256);

    let first = vec![b'A'; 180];
    let second = vec![b'B'; 180];
    let options = WriteOptions {
        dedup_enabled: false,
        preferred_codec: CompressionCodec::None,
        force_sync: true,
        payload_checksum_enabled: true,
    };
    let first_report =
        write_logical_block(&paths, 0, &first, 0, options, None).expect("first write");
    let second_report =
        write_logical_block(&paths, 1000, &second, 0, options, None).expect("second write");

    assert_ne!(
        first_report.record_id, second_report.record_id,
        "separate writes should create distinct map records"
    );
    let index = load_segment_index(&paths.segment_index_file).expect("load index");
    assert!(
        index.descriptors.len() >= 2,
        "expected rotation, got {:?}",
        index.descriptors
    );
    assert!(data_segment_path(&paths, 0).exists());
    assert!(data_segment_path(&paths, 1).exists());

    let read_first = read_logical_block(&paths, 0)
        .expect("read first")
        .expect("first present");
    let read_second = read_logical_block(&paths, 1000)
        .expect("read second")
        .expect("second present");
    assert_eq!(read_first.payload, first);
    assert_eq!(read_second.payload, second);
}

#[test]
fn unmap_stales_records_and_decrements_refcount() {
    let paths = test_paths("unmap-decrement");
    initialize_layout(&paths).expect("layout init should pass");

    let options = WriteOptions {
        dedup_enabled: true,
        preferred_codec: CompressionCodec::Rle,
        force_sync: false,
        payload_checksum_enabled: true,
    };

    let payload = b"CCCCCCCCCCCCCCCC";
    let report =
        write_logical_block(&paths, 100, payload, 0, options, None).expect("write should pass");
    assert!(report.dedup_entry_id > 0);

    let unmap = run_unmap(&paths, 100, payload.len() as u32).expect("unmap should pass");
    assert_eq!(unmap.staled_record_ids.len(), 1);
    assert_eq!(unmap.dedup_refcount_decrements, 1);

    let refs = current_dedup_refcounts(&paths).expect("dedup refs should load");
    assert_eq!(refs[0].1, 0);
}

fn set_segment_size(paths: &LayoutPaths, max_segment_size: u32) {
    let mut index = load_segment_index(&paths.segment_index_file).expect("load index");
    index.max_segment_size = max_segment_size;
    persist_segment_index(&paths.segment_index_file, &index).expect("persist index");
}

#[test]
fn recovers_dirty_checkpoint_after_failpoint() {
    let paths = test_paths("recover-dirty");
    initialize_layout(&paths).expect("layout init should pass");

    let options = WriteOptions {
        dedup_enabled: true,
        preferred_codec: CompressionCodec::Rle,
        force_sync: false,
        payload_checksum_enabled: true,
    };

    let payload = b"DDDDDDDDDDDDDDDD";
    let err = write_logical_block(
        &paths,
        200,
        payload,
        0,
        options,
        Some(IngestFailpoint::AfterLookupAppend),
    )
    .expect_err("failpoint write should fail");
    assert!(format!("{err}").contains("interrupted"));

    let before = current_checkpoint(&paths).expect("checkpoint should load");
    assert_eq!(before.flags, CheckpointFlags::Dirty);

    let recovery = recover_dirty_state(&paths).expect("recovery should pass");
    assert!(recovery.dirty_detected);

    let after = current_checkpoint(&paths).expect("checkpoint should load");
    assert_eq!(after.flags, CheckpointFlags::Clean);

    let readback = read_logical_block(&paths, 200)
        .expect("read should pass")
        .expect("record should exist after recovery");
    assert_eq!(readback.payload, payload);
}

#[test]
fn cancelled_recovery_keeps_dirty_checkpoint_and_source_segment() {
    let paths = test_paths("recover-cancelled-progress");
    initialize_layout(&paths).expect("layout init should pass");
    let payload = vec![b'R'; 2 * 1024 * 1024];
    let error = write_logical_block(
        &paths,
        0,
        &payload,
        0,
        WriteOptions::throughput_default(),
        Some(IngestFailpoint::AfterLookupAppend),
    )
    .expect_err("failpoint should leave the checkpoint dirty");
    assert!(format!("{error}").contains("interrupted"));
    let source = data_segment_path(&paths, 0);
    let source_before = fs::read(&source).expect("read source segment");
    let mut saw_verified_work = false;
    let result = super::data_path::recover_dirty_state_with_progress(
        &paths,
        &mut |phase, bytes, _records| {
            if phase == MaintenancePhase::Scan && bytes > 0 {
                saw_verified_work = true;
                return Err(super::metadata::StorageError::Cancelled);
            }
            Ok(())
        },
    );
    assert!(result.is_err(), "recovery should observe cancellation");
    assert!(saw_verified_work, "recovery did not report checked work");
    assert_eq!(
        current_checkpoint(&paths)
            .expect("checkpoint after cancellation")
            .flags,
        CheckpointFlags::Dirty,
        "cancelled recovery must not mark the layout clean"
    );
    assert_eq!(fs::read(&source).expect("source remains"), source_before);
    let _ = fs::remove_dir_all(&paths.root);
}

#[test]
fn recovery_rebuilds_unflushed_segment_index_after_restart() {
    let paths = test_paths("recover-unflushed-segment-index");
    initialize_layout(&paths).expect("layout init should pass");

    let payload = b"FFFFFFFFFFFFFFFF";
    let err = write_logical_block(
        &paths,
        320,
        payload,
        0,
        WriteOptions::throughput_default(),
        Some(IngestFailpoint::AfterLookupAppend),
    )
    .expect_err("failpoint write should fail");
    assert!(format!("{err}").contains("interrupted"));

    discard_layout_caches(&paths);
    let stale_index = load_segment_index(&paths.segment_index_file).expect("load stale index");
    assert!(
        !stale_index
            .descriptors
            .iter()
            .any(|descriptor| descriptor.contains_blob(1)),
        "segment index should not rely on an in-memory dirty cache"
    );

    let recovery = recover_dirty_state(&paths).expect("recovery should rebuild segment index");
    assert!(recovery.dirty_detected);
    discard_layout_caches(&paths);

    let rebuilt_index = load_segment_index(&paths.segment_index_file).expect("load rebuilt index");
    assert!(
        rebuilt_index
            .descriptors
            .iter()
            .any(|descriptor| descriptor.contains_blob(1)),
        "recovery must persist a segment descriptor for the written blob"
    );
    let readback = read_logical_block(&paths, 320)
        .expect("read should pass")
        .expect("record should exist after recovery");
    assert_eq!(readback.payload, payload);
}

#[test]
fn flush_pending_writes_marks_checkpoint_clean() {
    let paths = test_paths("flush-pending");
    initialize_layout(&paths).expect("layout init should pass");

    let payload = b"EEEEEEEEEEEEEEEE";
    write_logical_block(
        &paths,
        0,
        payload,
        0,
        WriteOptions::throughput_default(),
        None,
    )
    .expect("write should pass");

    let before = current_checkpoint(&paths).expect("checkpoint should load");
    assert_eq!(before.flags, CheckpointFlags::Dirty);

    let flushed = flush_pending_writes(&paths).expect("flush should pass");
    assert!(flushed);

    let after = current_checkpoint(&paths).expect("checkpoint should load");
    assert_eq!(after.flags, CheckpointFlags::Clean);
}

#[test]
fn recovery_keeps_checkpoint_dirty_when_an_active_blob_reference_is_missing() {
    let paths = test_paths("recover-missing-active-blob");
    initialize_layout(&paths).expect("layout init should pass");
    write_logical_block(
        &paths,
        0,
        b"active-reference",
        0,
        WriteOptions::throughput_default(),
        None,
    )
    .expect("write active record");
    let original_segment = fs::read(data_segment_path(&paths, 0)).expect("read source segment");
    let (_, mut records) = load_blk_map_records(&paths.blk_map_file).expect("load block map");
    let active = records
        .iter_mut()
        .find(|record| record.state == BlkMapState::Active)
        .expect("active record");
    active.physical_offset = active.physical_offset.saturating_add(1000);
    persist_blk_map_records(&paths.blk_map_file, &records).expect("persist broken reference");
    mark_checkpoint_dirty(&paths).expect("mark dirty");
    discard_layout_caches(&paths);

    let result = recover_dirty_state(&paths);

    assert!(result.is_err(), "missing active blob must fail recovery");
    assert_eq!(
        current_checkpoint(&paths)
            .expect("checkpoint after failure")
            .flags,
        CheckpointFlags::Dirty,
        "recovery must not mark invalid references clean"
    );
    assert_eq!(
        fs::read(data_segment_path(&paths, 0)).expect("source segment remains"),
        original_segment
    );
    let _ = fs::remove_dir_all(&paths.root);
}

#[test]
fn recovery_rejects_active_codec_and_decoded_length_mismatches() {
    for (suffix, mutate) in [
        (
            "codec",
            Box::new(|record: &mut super::blk_map::BlkMapRecord| {
                record.compression = CompressionCodec::Lz4;
            }) as Box<dyn Fn(&mut super::blk_map::BlkMapRecord)>,
        ),
        (
            "decoded-length",
            Box::new(|record: &mut super::blk_map::BlkMapRecord| {
                record.logical_len = record.logical_len.saturating_add(1);
            }),
        ),
    ] {
        let paths = test_paths(&format!("recover-invalid-{suffix}"));
        initialize_layout(&paths).expect("layout init should pass");
        write_logical_block(
            &paths,
            0,
            b"active-reference-shape",
            0,
            WriteOptions::throughput_default(),
            None,
        )
        .expect("write active record");
        let (_, mut records) = load_blk_map_records(&paths.blk_map_file).expect("load block map");
        let active = records
            .iter_mut()
            .find(|record| record.state == BlkMapState::Active)
            .expect("active record");
        mutate(active);
        persist_blk_map_records(&paths.blk_map_file, &records).expect("persist invalid reference");
        mark_checkpoint_dirty(&paths).expect("mark dirty");
        discard_layout_caches(&paths);

        assert!(
            recover_dirty_state(&paths).is_err(),
            "invalid {suffix} reference must not recover"
        );
        assert_eq!(
            current_checkpoint(&paths)
                .expect("checkpoint after invalid reference")
                .flags,
            CheckpointFlags::Dirty
        );
        let _ = fs::remove_dir_all(&paths.root);
    }
}

#[test]
fn recovery_accepts_legacy_v1_fnv_payload_and_zero_segment_checksum_sentinel() {
    let paths = test_paths("recover-legacy-v1-fnv-zero-sentinel");
    initialize_layout(&paths).expect("layout init should pass");
    let payload = b"legacy-v1-payload";
    write_logical_block(
        &paths,
        0,
        payload,
        0,
        WriteOptions::throughput_default(),
        None,
    )
    .expect("write active record");
    let (_, mut records) = load_blk_map_records(&paths.blk_map_file).expect("load block map");
    let active = records
        .iter_mut()
        .find(|record| record.state == BlkMapState::Active)
        .expect("active record");
    active.payload_checksum_algorithm = PayloadChecksumAlgorithm::Fnv1a32;
    active.payload_checksum = checksum32(payload);
    persist_blk_map_records(&paths.blk_map_file, &records).expect("persist FNV checksum");

    let segment = data_segment_path(&paths, 0);
    let original = fs::read(&segment).expect("read segment");
    let header = super::segment::read_segment_header(&segment, super::layout::SegmentKind::Data)
        .expect("read v2 header");
    let payload_offset = super::segment::segment_payload_offset(&header) as usize;
    let segment_payload = &original[payload_offset..];
    let legacy_header = super::layout::SegmentHeader {
        version: super::layout::STORAGE_LAYOUT_VERSION_V1,
        checksum: 0,
        payload_len: segment_payload.len() as u64,
        ..header
    };
    let mut legacy = legacy_header.encode();
    legacy.extend_from_slice(segment_payload);
    fs::write(&segment, legacy).expect("rewrite as v1 with zero sentinel");
    mark_checkpoint_dirty(&paths).expect("mark dirty");
    discard_layout_caches(&paths);

    let report = recover_dirty_state(&paths).expect("legacy v1 should remain readable");
    assert!(report.dirty_detected);
    assert_eq!(
        current_checkpoint(&paths)
            .expect("checkpoint after v1 recovery")
            .flags,
        CheckpointFlags::Clean
    );
    let readback = read_logical_block(&paths, 0)
        .expect("legacy read succeeds")
        .expect("legacy data exists");
    assert_eq!(readback.payload, payload);
    let _ = fs::remove_dir_all(&paths.root);
}

#[test]
fn recovery_streams_raw_blob_larger_than_compressed_decode_limit() {
    let paths = test_paths("recover-large-raw-active");
    initialize_layout(&paths).expect("layout init should pass");
    let payload = vec![b'Q'; 20 * 1024 * 1024];
    write_logical_block(
        &paths,
        0,
        &payload,
        0,
        WriteOptions::throughput_default(),
        None,
    )
    .expect("write large raw blob");
    mark_checkpoint_dirty(&paths).expect("mark dirty");
    discard_layout_caches(&paths);

    let report = recover_dirty_state(&paths).expect("large raw data is streamed");
    assert!(report.dirty_detected);
    assert_eq!(
        current_checkpoint(&paths)
            .expect("checkpoint after large raw recovery")
            .flags,
        CheckpointFlags::Clean
    );
    let readback = read_logical_block(&paths, 0)
        .expect("read large raw data")
        .expect("large raw data exists");
    assert_eq!(readback.payload, payload);
    let _ = fs::remove_dir_all(&paths.root);
}

#[test]
fn recovery_keeps_checkpoint_dirty_when_active_payload_integrity_fails() {
    let paths = test_paths("recover-corrupt-active-payload");
    initialize_layout(&paths).expect("layout init should pass");
    write_logical_block(
        &paths,
        0,
        b"active-payload-integrity",
        0,
        WriteOptions::throughput_default(),
        None,
    )
    .expect("write active record");
    let segment = data_segment_path(&paths, 0);
    let mut original = fs::read(&segment).expect("read source segment");
    let last = original
        .last_mut()
        .expect("segment contains encoded payload");
    *last ^= 0x55;
    fs::write(&segment, &original).expect("corrupt payload");
    mark_checkpoint_dirty(&paths).expect("mark dirty");
    discard_layout_caches(&paths);

    let result = recover_dirty_state(&paths);

    assert!(result.is_err(), "corrupt active payload must fail recovery");
    assert_eq!(
        current_checkpoint(&paths)
            .expect("checkpoint after failure")
            .flags,
        CheckpointFlags::Dirty,
        "corrupt payload must never be committed as clean"
    );
    assert_eq!(
        fs::read(&segment).expect("source segment remains"),
        original,
        "failed recovery preserves its source segment"
    );
    let _ = fs::remove_dir_all(&paths.root);
}
