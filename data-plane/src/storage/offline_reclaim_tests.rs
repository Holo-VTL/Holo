use std::fs;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

use super::data_path::run_unmap;
use super::layout::{initialize_layout, LayoutPaths, SegmentKind};
use super::metadata::StorageError;
use super::offline_reclaim::reclaim_one_segment;
use super::runtime_state::persist_retention_state;
use super::segment::write_segment_file;
use super::segment_index::{
    data_segment_path, load_segment_index, persist_segment_index, SegmentDescriptor, SegmentState,
};
use super::space_guard::FilesystemLock;
use super::{
    load_blk_map_records, read_logical_block, write_logical_block, BlkMapState, WriteOptions,
};

static TEST_DIR_SEQUENCE: AtomicU64 = AtomicU64::new(1);

use super::CompressionCodec;

fn test_paths() -> (
    std::path::PathBuf,
    std::path::PathBuf,
    std::path::PathBuf,
    LayoutPaths,
) {
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("clock")
        .as_nanos();
    let sequence = TEST_DIR_SEQUENCE.fetch_add(1, Ordering::Relaxed);
    let base = std::env::temp_dir().join(format!(
        "holo-offline-reclaim-{}-{nonce}-{sequence}",
        std::process::id()
    ));
    let pool_root = base.join("pool");
    let runtime_dir = base.join("run");
    fs::create_dir_all(&pool_root).expect("create pool root");
    fs::create_dir_all(&runtime_dir).expect("create runtime dir");
    let paths = LayoutPaths::for_cartridge(&pool_root, "library-a", "cart-a");
    initialize_layout(&paths).expect("initialize layout");
    (base, pool_root, runtime_dir, paths)
}

fn set_segment_size(paths: &LayoutPaths, max_segment_size: u32) {
    let mut index = load_segment_index(&paths.segment_index_file).expect("load segment index");
    index.max_segment_size = max_segment_size;
    persist_segment_index(&paths.segment_index_file, &index).expect("persist segment limit");
}

#[test]
fn partial_reclaim_preserves_live_blob_id_codec_and_payload() {
    let (base, pool_root, runtime_dir, paths) = test_paths();
    let options = WriteOptions {
        dedup_enabled: false,
        force_sync: true,
        ..WriteOptions::default()
    };
    let old_payload = vec![0x31; 1024 * 1024];
    let live_payload = vec![0x72; 1024 * 1024];
    write_logical_block(&paths, 0, &old_payload, 0, options, None).expect("write stale blob");
    run_unmap(&paths, 0, old_payload.len() as u32).expect("stale first record");
    write_logical_block(&paths, 0, &live_payload, 0, options, None).expect("write live blob");
    let (_, before_records) = load_blk_map_records(&paths.blk_map_file).expect("load map before");
    let before = before_records
        .iter()
        .find(|record| record.state == BlkMapState::Active)
        .expect("active record")
        .clone();
    let report = reclaim_one_segment(&paths, &pool_root, "library-a", "cart-a", &runtime_dir)
        .expect("reclaim one segment");

    assert_eq!(report.processed_segments, 1);
    let (_, after_records) = load_blk_map_records(&paths.blk_map_file).expect("load map after");
    let after = after_records
        .iter()
        .find(|record| record.state == BlkMapState::Active)
        .expect("active record after reclaim");
    assert_eq!(after.physical_offset, before.physical_offset);
    assert_eq!(after.physical_segment_id, before.physical_segment_id);
    assert_eq!(
        read_logical_block(&paths, 0)
            .expect("read after reclaim")
            .expect("live record")
            .payload,
        live_payload
    );
    let segment_path = data_segment_path(&paths, before.physical_segment_id as u32);
    assert!(segment_path.exists());
    fs::remove_dir_all(base).expect("remove fixture");
}

#[test]
fn no_candidate_is_a_zero_work_result() {
    let (base, pool_root, runtime_dir, paths) = test_paths();
    let options = WriteOptions {
        dedup_enabled: false,
        force_sync: true,
        ..WriteOptions::default()
    };
    write_logical_block(&paths, 0, b"only-live-record", 0, options, None).expect("write live");
    let report = reclaim_one_segment(&paths, &pool_root, "library-a", "cart-a", &runtime_dir)
        .expect("no-op reclaim");
    assert_eq!(report.processed_segments, 0);
    assert_eq!(report.net_freed_bytes, 0);
    fs::remove_dir_all(base).expect("remove fixture");
}

#[test]
fn offline_reclaim_reservation_coexists_with_other_writes_without_holding_filesystem_lock() {
    let (base, pool_root, runtime_dir, paths) = test_paths();
    let options = WriteOptions {
        dedup_enabled: false,
        force_sync: true,
        ..WriteOptions::default()
    };
    let old_payload = vec![0x31; 1024 * 1024];
    let live_payload = vec![0x72; 1024 * 1024];
    write_logical_block(&paths, 0, &old_payload, 0, options, None).expect("write stale blob");
    run_unmap(&paths, 0, old_payload.len() as u32).expect("stale first record");
    write_logical_block(&paths, 0, &live_payload, 0, options, None).expect("write live blob");
    let reservation = {
        let lock = FilesystemLock::acquire(&pool_root, &runtime_dir).expect("lock filesystem");
        lock.reserve(1024 * 1024)
            .expect("reserve active write capacity")
    };

    let mut saw_copy = false;
    let result = super::offline_reclaim::reclaim_one_segment_with_cursor_and_progress(
        &paths,
        &pool_root,
        "library-a",
        "cart-a",
        &runtime_dir,
        None,
        &mut |phase, _bytes, _records| {
            if phase == super::maintenance_progress::MaintenancePhase::Copy {
                saw_copy = true;
                let lock = FilesystemLock::acquire(&pool_root, &runtime_dir)
                    .expect("copy phase must not hold shared filesystem lock");
                assert!(
                    lock.active_reservation_bytes()
                        .expect("read concurrent reservations")
                        >= 1024 * 1024,
                    "other writer reservation remains accounted"
                );
            }
            Ok(())
        },
    );
    assert!(result.is_ok(), "unexpected reclaim result: {result:?}");
    assert!(
        saw_copy,
        "eligible data should reach the streamed copy phase"
    );
    assert_eq!(result.expect("reclaim completes").processed_segments, 1);
    assert_eq!(
        super::read_logical_block(&paths, 0)
            .expect("read after reclaim")
            .expect("live record")
            .payload,
        live_payload
    );

    drop(reservation);
    fs::remove_dir_all(base).expect("remove fixture");
}

#[test]
fn cancelled_or_failed_reclaim_releases_capacity_reservation_before_mutation() {
    let (base, pool_root, runtime_dir, paths) = test_paths();
    let options = WriteOptions {
        dedup_enabled: false,
        force_sync: true,
        ..WriteOptions::default()
    };
    let old_payload = vec![0x31; 1024 * 1024];
    let live_payload = vec![0x72; 1024 * 1024];
    write_logical_block(&paths, 0, &old_payload, 0, options, None).expect("write stale blob");
    run_unmap(&paths, 0, old_payload.len() as u32).expect("stale first record");
    write_logical_block(&paths, 0, &live_payload, 0, options, None).expect("write live blob");
    let metadata_before = fs::read(&paths.metadata_file).expect("read checkpoint before");
    let (_, records) = load_blk_map_records(&paths.blk_map_file).expect("load map before");
    let active = records
        .iter()
        .find(|record| record.state == BlkMapState::Active)
        .expect("active record");
    let source = data_segment_path(&paths, active.physical_segment_id as u32);
    assert!(source.exists(), "source segment must exist before reclaim");

    for inject_failure in [false, true] {
        let result = super::offline_reclaim::reclaim_one_segment_with_cursor_and_progress(
            &paths,
            &pool_root,
            "library-a",
            "cart-a",
            &runtime_dir,
            None,
            &mut |phase, _bytes, _records| {
                if phase == super::maintenance_progress::MaintenancePhase::Commit {
                    return if inject_failure {
                        Err(StorageError::Corrupt("injected test failure".to_string()))
                    } else {
                        Err(StorageError::Cancelled)
                    };
                }
                Ok(())
            },
        );
        if inject_failure {
            assert!(matches!(result, Err(StorageError::Corrupt(_))));
        } else {
            assert!(matches!(result, Err(StorageError::Cancelled)));
        }

        let lock = FilesystemLock::acquire(&pool_root, &runtime_dir)
            .expect("acquire lock after interrupted reclaim");
        assert_eq!(
            lock.active_reservation_bytes()
                .expect("read reservations after interruption"),
            0,
            "interrupted maintenance must release its reservation"
        );
        drop(lock);
        assert_eq!(
            fs::read(&paths.metadata_file).expect("checkpoint after interruption"),
            metadata_before,
            "cancellation/failure before commit must not mutate checkpoint"
        );
        assert!(source.exists(), "interrupted reclaim must preserve source");
    }

    fs::remove_dir_all(base).expect("remove fixture");
}

#[test]
fn offline_reclaim_skips_worm_media_even_when_retention_is_not_locked() {
    let (base, pool_root, runtime_dir, paths) = test_paths();
    let options = WriteOptions {
        dedup_enabled: false,
        force_sync: true,
        ..WriteOptions::default()
    };
    write_logical_block(&paths, 0, b"stale-data", 0, options, None).expect("write stale blob");
    run_unmap(&paths, 0, 10).expect("mark blob stale");
    write_logical_block(&paths, 0, b"live-data", 0, options, None).expect("write live blob");
    persist_retention_state(&paths.root, true, false).expect("mark unlocked WORM media");
    let checkpoint_before = fs::read(&paths.metadata_file).expect("read checkpoint");
    let map_before = fs::read(&paths.blk_map_file).expect("read block map");
    let result = reclaim_one_segment(&paths, &pool_root, "library-a", "cart-a", &runtime_dir);

    assert!(
        matches!(&result, Err(StorageError::Conflict(message)) if message.contains("WORM")),
        "unlocked WORM media must be deferred, got {result:?}"
    );
    assert_eq!(
        fs::read(&paths.metadata_file).expect("checkpoint after"),
        checkpoint_before
    );
    assert_eq!(
        fs::read(&paths.blk_map_file).expect("map after"),
        map_before
    );
    assert_eq!(
        super::read_logical_block(&paths, 0)
            .expect("read live data")
            .expect("live record")
            .payload,
        b"live-data"
    );
    fs::remove_dir_all(base).expect("remove fixture");
}

#[test]
fn offline_reclaim_skips_a_cartridge_reported_loaded_by_shared_state() {
    let (base, pool_root, runtime_dir, paths) = test_paths();
    let options = WriteOptions {
        dedup_enabled: false,
        force_sync: true,
        ..WriteOptions::default()
    };
    write_logical_block(&paths, 0, b"stale-data", 0, options, None).expect("write stale blob");
    run_unmap(&paths, 0, 10).expect("mark blob stale");
    write_logical_block(&paths, 0, b"live-data", 0, options, None).expect("write live blob");
    let media_state_dir = base.join("media-state");
    fs::create_dir_all(&media_state_dir).expect("create media state dir");
    fs::write(
        media_state_dir.join("library-a__drive-a.state"),
        b"cartridge=cart-a\n",
    )
    .expect("write loaded state");
    let checkpoint_before = fs::read(&paths.metadata_file).expect("checkpoint before");
    let map_before = fs::read(&paths.blk_map_file).expect("map before");

    let result = super::offline_reclaim::reclaim_one_segment_with_media_state_dir(
        &paths,
        &pool_root,
        "library-a",
        "cart-a",
        &runtime_dir,
        &media_state_dir,
        None,
    );

    assert!(
        matches!(&result, Err(StorageError::Conflict(message)) if message.contains("loaded")),
        "loaded media must not be reclaimed: {result:?}"
    );
    assert_eq!(
        fs::read(&paths.metadata_file).expect("checkpoint after"),
        checkpoint_before
    );
    assert_eq!(
        fs::read(&paths.blk_map_file).expect("map after"),
        map_before
    );
    assert_eq!(
        super::read_logical_block(&paths, 0)
            .expect("read live data")
            .expect("live record")
            .payload,
        b"live-data"
    );
    fs::remove_dir_all(base).expect("remove fixture");
}

#[test]
fn offline_reclaim_rejects_oversized_metadata_before_recovery_cleanup() {
    const MAX_METADATA_BYTES: u64 = 8 * 1024 * 1024;
    let (base, pool_root, runtime_dir, paths) = test_paths();
    let options = WriteOptions {
        dedup_enabled: false,
        force_sync: true,
        ..WriteOptions::default()
    };
    write_logical_block(&paths, 0, b"stale-data", 0, options, None).expect("write stale blob");
    run_unmap(&paths, 0, 10).expect("mark blob stale");
    write_logical_block(&paths, 0, b"live-data", 0, options, None).expect("write live blob");
    super::data_path::mark_checkpoint_dirty(&paths).expect("mark dirty checkpoint");
    let temporary = paths.root.join("lookup.tmp");
    fs::write(&temporary, b"preserve until budget validation").expect("create temp artifact");
    let file = std::fs::OpenOptions::new()
        .write(true)
        .open(&paths.lookup_file)
        .expect("open lookup metadata");
    file.set_len(MAX_METADATA_BYTES + 1)
        .expect("grow metadata past budget");
    let checkpoint_before = fs::read(&paths.metadata_file).expect("read checkpoint before");
    let map_before = fs::read(&paths.blk_map_file).expect("read block map before");
    let lookup_before = fs::read(&paths.lookup_file).expect("read lookup before");
    let result = reclaim_one_segment(&paths, &pool_root, "library-a", "cart-a", &runtime_dir);

    assert!(
        matches!(result, Err(StorageError::MetadataBudgetExceeded)),
        "oversized metadata must defer before loading, got {result:?}"
    );
    assert_eq!(
        fs::read(&paths.metadata_file).expect("checkpoint after"),
        checkpoint_before
    );
    assert_eq!(
        fs::read(&paths.blk_map_file).expect("map after"),
        map_before
    );
    assert_eq!(
        fs::read(&paths.lookup_file).expect("lookup after"),
        lookup_before
    );
    assert!(
        temporary.exists(),
        "preflight must happen before tmp cleanup"
    );
    fs::remove_dir_all(base).expect("remove fixture");
}

#[test]
fn offline_reclaim_rejects_excessive_metadata_records_before_recovery_cleanup() {
    let (base, pool_root, runtime_dir, paths) = test_paths();
    let mut payload = vec![0u8; 24];
    payload[..4].copy_from_slice(b"SDI1");
    payload[4..8].copy_from_slice(&(256 * 1024 * 1024u32).to_le_bytes());
    payload[8..12].copy_from_slice(&1u32.to_le_bytes());
    payload[12..16].copy_from_slice(&65_537u32.to_le_bytes());
    payload[16..24].copy_from_slice(&1u64.to_le_bytes());
    write_segment_file(
        &paths.segment_index_file,
        SegmentKind::SegmentIndex,
        7,
        1,
        &payload,
    )
    .expect("write over-budget metadata fixture");
    super::data_path::mark_checkpoint_dirty(&paths).expect("mark dirty checkpoint");
    let temporary = paths.root.join("lookup.tmp");
    fs::write(&temporary, b"preserve until record validation").expect("create temp artifact");
    let checkpoint_before = fs::read(&paths.metadata_file).expect("read checkpoint before");
    let index_before = fs::read(&paths.segment_index_file).expect("read index before");

    let result = reclaim_one_segment(&paths, &pool_root, "library-a", "cart-a", &runtime_dir);

    assert!(
        matches!(result, Err(StorageError::MetadataBudgetExceeded)),
        "record count over the maintenance budget must defer: {result:?}"
    );
    assert_eq!(
        fs::read(&paths.metadata_file).expect("checkpoint after"),
        checkpoint_before
    );
    assert_eq!(
        fs::read(&paths.segment_index_file).expect("index after"),
        index_before
    );
    assert!(
        temporary.exists(),
        "preflight must preserve temporary files"
    );
    fs::remove_dir_all(base).expect("remove fixture");
}

#[test]
fn maintenance_scan_cursor_advances_after_256_descriptors() {
    let (base, pool_root, runtime_dir, paths) = test_paths();
    let mut index = load_segment_index(&paths.segment_index_file).expect("load segment index");
    for segment_seq in 1..=256u32 {
        index.descriptors.push(SegmentDescriptor {
            segment_seq,
            payload_bytes: 0,
            first_blob_id: 0,
            last_blob_id: 0,
            state: SegmentState::Reclaimable,
            compression: CompressionCodec::None,
            live_bytes: 0,
        });
    }
    index.next_segment_seq = 257;
    persist_segment_index(&paths.segment_index_file, &index).expect("persist descriptor fixture");

    let first = super::offline_reclaim::reclaim_one_segment_with_cursor(
        &paths,
        &pool_root,
        "library-a",
        "cart-a",
        &runtime_dir,
        None,
    )
    .expect("first bounded scan");

    assert_eq!(first.processed_segments, 0);
    assert!(first.has_more);
    let cursor = first.scan_cursor.expect("cursor after 256 descriptors");
    let cursor_value: serde_json::Value =
        serde_json::from_str(&cursor).expect("parse internal cursor");
    assert_eq!(cursor_value["segment_ordinal"], 256);

    let second = super::offline_reclaim::reclaim_one_segment_with_cursor(
        &paths,
        &pool_root,
        "library-a",
        "cart-a",
        &runtime_dir,
        Some(&cursor),
    )
    .expect("resume bounded scan");
    assert_eq!(second.processed_segments, 0);
    assert!(!second.has_more);
    assert!(second.scan_cursor.is_none());
    fs::remove_dir_all(base).expect("remove fixture");
}

#[test]
fn maintenance_scan_cursor_advances_after_65536_blob_headers() {
    const HEADER_COUNT: u64 = 65_537;
    let (base, pool_root, runtime_dir, paths) = test_paths();
    let mut payload = Vec::with_capacity(4 + HEADER_COUNT as usize * 24);
    payload.extend_from_slice(b"DTV2");
    for blob_id in 1..=HEADER_COUNT {
        payload.extend_from_slice(&super::data_path::encode_data_blob_header(
            blob_id,
            CompressionCodec::None,
            0,
            0,
            0,
        ));
    }
    let segment = data_segment_path(&paths, 1);
    write_segment_file(&segment, SegmentKind::Data, 1, 1, &payload)
        .expect("write header-only data segment");
    let mut index = load_segment_index(&paths.segment_index_file).expect("load segment index");
    index.descriptors.push(SegmentDescriptor {
        segment_seq: 1,
        payload_bytes: payload.len() as u64,
        first_blob_id: 1,
        last_blob_id: HEADER_COUNT,
        state: SegmentState::Sealed,
        compression: CompressionCodec::None,
        live_bytes: 0,
    });
    index.next_segment_seq = 2;
    persist_segment_index(&paths.segment_index_file, &index).expect("persist descriptor fixture");

    let first = super::offline_reclaim::reclaim_one_segment_with_cursor(
        &paths,
        &pool_root,
        "library-a",
        "cart-a",
        &runtime_dir,
        None,
    )
    .expect("scan first 65536 headers");
    assert_eq!(first.processed_segments, 0);
    assert!(first.has_more);
    assert!(
        segment.exists(),
        "partial scan must preserve source segment"
    );
    let cursor = first.scan_cursor.expect("cursor within header run");
    let cursor_value: serde_json::Value =
        serde_json::from_str(&cursor).expect("parse internal cursor");
    assert_eq!(cursor_value["verified_headers"], 65_536);

    let second = super::offline_reclaim::reclaim_one_segment_with_cursor(
        &paths,
        &pool_root,
        "library-a",
        "cart-a",
        &runtime_dir,
        Some(&cursor),
    )
    .expect("resume final header and reclaim candidate");
    assert_eq!(second.processed_segments, 1);
    assert!(!second.has_more);
    assert!(second.scan_cursor.is_none());
    assert!(!segment.exists(), "fully dead sealed segment is removed");
    write_logical_block(
        &paths,
        0,
        b"after-high-water",
        0,
        WriteOptions::throughput_default(),
        None,
    )
    .expect("write after reclaim");
    let (_, records) = load_blk_map_records(&paths.blk_map_file).expect("load new block map");
    let new_blob_id = records
        .iter()
        .find(|record| record.state == BlkMapState::Active)
        .expect("new active block map record")
        .physical_offset;
    assert_eq!(new_blob_id, HEADER_COUNT + 1);
    fs::remove_dir_all(base).expect("remove fixture");
}

#[test]
fn dirty_recovery_preserves_a_stale_blob_id_high_water_witness() {
    let (base, pool_root, runtime_dir, paths) = test_paths();
    let options = WriteOptions {
        dedup_enabled: false,
        force_sync: true,
        ..WriteOptions::default()
    };
    write_logical_block(&paths, 0, b"live-first", 0, options, None).expect("write live blob");
    write_logical_block(&paths, 100, b"dead-highest", 0, options, None).expect("write high blob");
    run_unmap(&paths, 100, 11).expect("unmap high blob");

    let report = reclaim_one_segment(&paths, &pool_root, "library-a", "cart-a", &runtime_dir)
        .expect("reclaim stale high blob");
    assert_eq!(report.processed_segments, 1);

    super::data_path::mark_checkpoint_dirty(&paths).expect("mark recovery fixture dirty");
    super::recover_dirty_state(&paths).expect("recover dirty layout");
    write_logical_block(&paths, 100, b"new-after-recovery", 0, options, None)
        .expect("append after recovery");
    let (_, records) = load_blk_map_records(&paths.blk_map_file).expect("load recovered map");
    let new_record = records
        .iter()
        .find(|record| record.state == BlkMapState::Active && record.logical_start == 100)
        .expect("new active record");
    assert_eq!(new_record.physical_offset, 3);
    fs::remove_dir_all(base).expect("remove fixture");
}

#[test]
fn offline_reclaim_keeps_memory_bounded_for_32_segments_and_96_payloads() {
    const PAYLOAD_BYTES: usize = 2 * 1024 * 1024;
    const SEGMENT_LIMIT: u32 = 7 * 1024 * 1024;
    let (base, pool_root, runtime_dir, paths) = test_paths();
    set_segment_size(&paths, SEGMENT_LIMIT);
    let old_payload = vec![0x31; PAYLOAD_BYTES];
    let second_dead_payload = vec![0x54; PAYLOAD_BYTES];
    let live_payload = vec![0x72; PAYLOAD_BYTES];
    let options = WriteOptions {
        dedup_enabled: false,
        preferred_codec: CompressionCodec::None,
        force_sync: false,
        payload_checksum_enabled: true,
    };
    for index in 0u64..32 {
        let logical_start = index * (PAYLOAD_BYTES as u64 * 3);
        write_logical_block(&paths, logical_start, &old_payload, 0, options, None)
            .expect("write first dead blob");
        run_unmap(&paths, logical_start, PAYLOAD_BYTES as u32).expect("unmap first dead blob");
        write_logical_block(
            &paths,
            logical_start,
            &second_dead_payload,
            0,
            options,
            None,
        )
        .expect("write second dead blob");
        run_unmap(&paths, logical_start, PAYLOAD_BYTES as u32).expect("unmap second dead blob");
        write_logical_block(&paths, logical_start, &live_payload, 0, options, None)
            .expect("write live blob");
    }

    let report = reclaim_one_segment(&paths, &pool_root, "library-a", "cart-a", &runtime_dir)
        .expect("reclaim bounded candidate");

    assert_eq!(report.processed_segments, 1);
    assert!(
        report.has_more,
        "remaining eligible segments must be reported"
    );
    assert_eq!(
        super::read_logical_block(&paths, 0)
            .expect("read live block")
            .expect("live record")
            .payload,
        live_payload
    );
    if let Ok(status) = fs::read_to_string("/proc/self/status") {
        let vm_hwm_kib = status
            .lines()
            .find_map(|line| line.strip_prefix("VmHWM:"))
            .and_then(|value| value.split_whitespace().next())
            .and_then(|value| value.parse::<u64>().ok())
            .expect("parse VmHWM");
        assert!(
            vm_hwm_kib <= 128 * 1024,
            "reclaim exceeded 128 MiB high-water RSS: {vm_hwm_kib} KiB"
        );
    }
    fs::remove_dir_all(base).expect("remove fixture");
}

#[cfg(target_os = "linux")]
#[test]
#[ignore = "requires a dedicated small Linux filesystem mounted by tape_space_reclaim_smoke.sh"]
fn linux_small_filesystem_reclaim_smoke() {
    let fixture_root = std::env::var_os("HOLO_TAPE_TEST_ROOT")
        .map(std::path::PathBuf::from)
        .expect("test harness must set HOLO_TAPE_TEST_ROOT");
    let fixture_root = fs::canonicalize(fixture_root).expect("canonicalize dedicated test mount");
    let pool_root = fixture_root.join("pool");
    let runtime_dir = fixture_root.join("runtime");
    let media_state_dir = fixture_root.join("media-state");
    fs::create_dir_all(&pool_root).expect("create isolated pool");
    fs::create_dir_all(&runtime_dir).expect("create isolated runtime dir");
    fs::create_dir_all(&media_state_dir).expect("create isolated media-state dir");
    std::env::set_var("HOLO_MEDIA_STATE_DIR", &media_state_dir);
    std::env::set_var("HOLO_MEDIA_STATE_KEY", "library-smoke__drive-smoke");

    let paths = LayoutPaths::for_cartridge(&pool_root, "library-smoke", "cart-smoke");
    initialize_layout(&paths).expect("initialize isolated layout");
    let options = WriteOptions {
        dedup_enabled: false,
        preferred_codec: CompressionCodec::None,
        force_sync: true,
        ..WriteOptions::default()
    };
    let old_payload = linux_smoke_payload(24 * 1024 * 1024, 0x34);
    let live_payload = linux_smoke_payload(24 * 1024 * 1024, 0xa7);
    write_logical_block(&paths, 0, &old_payload, 0, options, None).expect("write stale blob");
    run_unmap(&paths, 0, old_payload.len() as u32).expect("make first blob stale");
    write_logical_block(&paths, 0, &live_payload, 0, options, None).expect("write live blob");

    let report = super::offline_reclaim::reclaim_one_segment(
        &paths,
        &pool_root,
        "library-smoke",
        "cart-smoke",
        &runtime_dir,
    )
    .expect("reclaim on the constrained Linux filesystem");
    assert_eq!(report.processed_segments, 1);
    assert!(
        report.net_freed_bytes >= 16 * 1024 * 1024,
        "unexpected reclaim report: {report:?}"
    );
    assert_eq!(
        super::read_logical_block(&paths, 0)
            .expect("read live data after reclaim")
            .expect("live record is present")
            .payload,
        live_payload
    );
}

#[cfg(target_os = "linux")]
fn linux_smoke_payload(len: usize, seed: u8) -> Vec<u8> {
    let mut value = u64::from(seed) | 1;
    (0..len)
        .map(|_| {
            value ^= value << 13;
            value ^= value >> 7;
            value ^= value << 17;
            value as u8
        })
        .collect()
}
