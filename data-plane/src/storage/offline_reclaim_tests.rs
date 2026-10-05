use std::fs;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

#[cfg(target_os = "linux")]
use std::io::Read;
#[cfg(target_os = "linux")]
use std::os::unix::fs::MetadataExt;
#[cfg(target_os = "linux")]
use std::time::Instant;

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
#[cfg(target_os = "linux")]
use super::space_guard::FilesystemSnapshot;
use super::{
    load_blk_map_records, read_logical_block, write_logical_block, BlkMapState, WriteOptions,
};
#[cfg(target_os = "linux")]
use sha2::{Digest, Sha256};

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
#[test]
#[ignore = "requires a dedicated Linux filesystem with space for the configured real payload"]
fn linux_large_reclaim_acceptance() {
    const BLOCK_BYTES: usize = 8 * 1024 * 1024;
    const BLOCKS_PER_CHUNK: usize = 64;
    const MAX_SEGMENT_BYTES: u32 = 1024 * 1024 * 1024;
    const PUBLIC_LOCK_LIMIT: std::time::Duration = std::time::Duration::from_millis(250);
    const DEFAULT_TEST_BYTES: u64 = 50 * 1024 * 1024 * 1024;
    const PROBE_BLOCK_BYTES: usize = 256 * 1024;
    const PROBE_BLOCKS_PER_ROUND: usize = 16;
    const ACCEPTANCE_PROBE_ROUNDS: usize = 3;

    let fixture_root = std::env::var_os("HOLO_TAPE_TEST_ROOT")
        .map(std::path::PathBuf::from)
        .expect("test harness must set HOLO_TAPE_TEST_ROOT");
    fs::create_dir_all(&fixture_root).expect("create test fixture root");
    let fixture_root = fs::canonicalize(fixture_root).expect("canonicalize test fixture root");
    let total_write_bytes = std::env::var("HOLO_TAPE_RECLAIM_TEST_BYTES")
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .unwrap_or(DEFAULT_TEST_BYTES);
    assert_eq!(
        total_write_bytes % (2 * BLOCK_BYTES as u64),
        0,
        "configured test bytes must contain whole stale/live block pairs"
    );
    let probe_rounds = if total_write_bytes < 2 * u64::from(MAX_SEGMENT_BYTES) {
        1
    } else {
        ACCEPTANCE_PROBE_ROUNDS
    };
    let pair_count = (total_write_bytes / (2 * BLOCK_BYTES as u64)) as usize;
    assert!(pair_count > 0, "configured payload must not be empty");
    assert!(
        pair_count.saturating_mul(2) <= super::metadata::MAX_MAINTENANCE_METADATA_RECORDS,
        "payload record count exceeds the maintenance metadata budget"
    );
    let live_bytes = total_write_bytes / 2;

    let pool_root = fixture_root.join("pool");
    let runtime_dir = fixture_root.join("runtime");
    let media_state_dir = fixture_root.join("media-state");
    fs::create_dir_all(&pool_root).expect("create isolated pool root");
    fs::create_dir_all(&runtime_dir).expect("create isolated runtime directory");
    fs::create_dir_all(&media_state_dir).expect("create isolated media state directory");
    std::env::set_var("HOLO_MEDIA_STATE_DIR", &media_state_dir);
    std::env::set_var("HOLO_MEDIA_STATE_KEY", "large-reclaim__drive");

    let filesystem = FilesystemSnapshot::probe(&pool_root).expect("probe scratch filesystem");
    let conservative_peak = total_write_bytes
        .checked_add(u64::from(MAX_SEGMENT_BYTES))
        .and_then(|bytes| bytes.checked_add(super::space_guard::SPACE_RESERVE_BYTES))
        .expect("compute scratch capacity requirement");
    assert!(
        filesystem.available_bytes >= conservative_peak,
        "scratch filesystem needs {conservative_peak} bytes free, found {}",
        filesystem.available_bytes
    );

    let paths = LayoutPaths::for_cartridge(&pool_root, "large-library", "large-cart");
    initialize_layout(&paths).expect("initialize large reclaim layout");
    let mut segment_index = load_segment_index(&paths.segment_index_file).expect("load index");
    segment_index.max_segment_size = MAX_SEGMENT_BYTES;
    persist_segment_index(&paths.segment_index_file, &segment_index).expect("set segment size");

    let options = WriteOptions {
        dedup_enabled: false,
        preferred_codec: CompressionCodec::None,
        force_sync: false,
        payload_checksum_enabled: true,
    };
    let mut random = fs::File::open("/dev/urandom").expect("open kernel random source");
    let mut live_hashes = Vec::<[u8; 32]>::with_capacity(pair_count);
    let started_at = Instant::now();

    for chunk_start in (0..pair_count).step_by(BLOCKS_PER_CHUNK) {
        let chunk_blocks = (pair_count - chunk_start).min(BLOCKS_PER_CHUNK);
        let logical_start = (chunk_start * BLOCK_BYTES) as u64;
        for block in chunk_start..chunk_start + chunk_blocks {
            let mut payload = vec![0u8; BLOCK_BYTES];
            random
                .read_exact(&mut payload)
                .expect("generate stale payload");
            write_logical_block(
                &paths,
                (block * BLOCK_BYTES) as u64,
                &payload,
                0,
                options,
                None,
            )
            .expect("write stale payload");
        }
        run_unmap(&paths, logical_start, (chunk_blocks * BLOCK_BYTES) as u32)
            .expect("mark old payloads stale");
        for block in chunk_start..chunk_start + chunk_blocks {
            let mut payload = vec![0u8; BLOCK_BYTES];
            random
                .read_exact(&mut payload)
                .expect("generate live payload");
            live_hashes.push(Sha256::digest(&payload).into());
            write_logical_block(
                &paths,
                (block * BLOCK_BYTES) as u64,
                &payload,
                0,
                options,
                None,
            )
            .expect("write replacement live payload");
        }
    }
    super::data_path::flush_pending_writes(&paths).expect("flush all staged payloads");
    let metadata_paths = [
        paths.metadata_file.clone(),
        paths.blk_map_file.clone(),
        paths.lookup_file.clone(),
        paths.reclaim_file.clone(),
        paths.dedup_file.clone(),
        paths.segment_index_file.clone(),
        paths.root.join("filemarks.state"),
        paths.root.join("usage.counters"),
    ];
    let mut metadata_bytes = 0u64;
    for path in metadata_paths {
        match fs::metadata(path) {
            Ok(metadata) => metadata_bytes += metadata.len(),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => panic!("inspect acceptance metadata file: {error}"),
        }
    }
    assert!(
        metadata_bytes <= super::metadata::MAX_MAINTENANCE_METADATA_BYTES,
        "test fixture metadata exceeds the maintenance budget: {metadata_bytes} bytes"
    );
    let allocated_before = allocated_bytes_in_tree(&paths.root);
    assert!(
        allocated_before >= total_write_bytes * 95 / 100,
        "payload was not physically allocated: wrote={total_write_bytes} allocated={allocated_before}"
    );

    let probe_paths = LayoutPaths::for_cartridge(&pool_root, "large-library", "probe-cart");
    initialize_layout(&probe_paths).expect("initialize concurrent writer cartridge");
    let probe_payload = vec![0x6d; PROBE_BLOCK_BYTES];
    let probe_options = WriteOptions {
        dedup_enabled: false,
        preferred_codec: CompressionCodec::None,
        force_sync: false,
        payload_checksum_enabled: true,
    };
    let write_probe_round = |round: usize| {
        let round_started = Instant::now();
        for block in 0..PROBE_BLOCKS_PER_ROUND {
            let logical_start =
                ((round * PROBE_BLOCKS_PER_ROUND + block) * PROBE_BLOCK_BYTES) as u64;
            write_logical_block(
                &probe_paths,
                logical_start,
                &probe_payload,
                0,
                probe_options,
                None,
            )
            .expect("write concurrent probe block");
        }
        super::data_path::flush_pending_writes(&probe_paths).expect("flush probe round");
        round_started.elapsed().as_nanos()
    };
    for round in 0..probe_rounds {
        write_probe_round(round);
    }
    let baseline_samples = (0..probe_rounds)
        .map(|round| write_probe_round(probe_rounds + round))
        .collect::<Vec<_>>();

    let executable = std::env::current_exe().expect("locate test executable");
    let worker_log = fixture_root.join("reclaim-worker.log");
    let worker_output = fs::File::create(&worker_log).expect("create worker log");
    let mut worker = std::process::Command::new("ionice")
        .args(["-c3", "--"])
        .arg(executable)
        .args([
            "--exact",
            "storage::offline_reclaim_tests::linux_reclaim_worker_child",
            "--ignored",
            "--nocapture",
        ])
        .env("HOLO_TAPE_TEST_ROOT", &fixture_root)
        .env("HOLO_MEDIA_STATE_DIR", &media_state_dir)
        .env("HOLO_MEDIA_STATE_KEY", "large-reclaim__drive")
        .stdout(worker_output)
        .stderr(std::process::Stdio::inherit())
        .spawn()
        .expect("start reclaim worker with idle I/O priority");

    let mut concurrent_samples = Vec::<u128>::with_capacity(probe_rounds);
    for round in 0..probe_rounds {
        let phase_marker = fixture_root.join(format!("copy-phase-{}", round + 1));
        let deadline = Instant::now() + std::time::Duration::from_secs(120);
        while !phase_marker.exists() {
            if let Some(status) = worker.try_wait().expect("check reclaim worker status") {
                let log = fs::read_to_string(&worker_log).unwrap_or_default();
                panic!("reclaim worker exited before probe phase {round}: {status}\n{log}");
            }
            assert!(
                Instant::now() < deadline,
                "reclaim worker did not reach copy phase {} within 120 seconds",
                round + 1
            );
            std::thread::sleep(std::time::Duration::from_millis(10));
        }
        concurrent_samples.push(write_probe_round(probe_rounds * 2 + round));
    }
    let worker_status = worker.wait().expect("wait for reclaim worker");
    let worker_log_contents = fs::read_to_string(&worker_log).unwrap_or_default();
    assert!(
        worker_status.success(),
        "reclaim worker failed: {worker_status}\n{worker_log_contents}"
    );
    let report = fs::read_to_string(fixture_root.join("reclaim-worker-report"))
        .expect("read reclaim worker report");
    let worker_metrics = report
        .split_whitespace()
        .filter_map(|field| field.split_once('='))
        .collect::<std::collections::HashMap<_, _>>();
    let processed_segments = worker_metrics
        .get("processed_segments")
        .expect("worker segment count")
        .parse::<u32>()
        .expect("parse worker segment count");
    let vm_hwm_kib = worker_metrics
        .get("vm_hwm_kib")
        .expect("worker RSS")
        .parse::<u64>()
        .expect("parse worker RSS");
    let max_public_lock_wait_us = worker_metrics
        .get("max_public_lock_wait_us")
        .expect("worker lock wait")
        .parse::<u64>()
        .expect("parse worker lock wait");
    assert_eq!(
        concurrent_samples.len(),
        probe_rounds,
        "concurrent write probes did not overlap reclaim copy"
    );
    assert!(
        std::time::Duration::from_micros(max_public_lock_wait_us) <= PUBLIC_LOCK_LIMIT,
        "public filesystem lock waited {max_public_lock_wait_us}us, limit is {:?}",
        PUBLIC_LOCK_LIMIT
    );

    let allocated_after = allocated_bytes_in_tree(&paths.root);
    let bytes_freed = allocated_before.saturating_sub(allocated_after);
    assert!(processed_segments > 0, "no segment was reclaimed");
    assert!(
        bytes_freed >= live_bytes * 70 / 100,
        "reclaim freed too little storage: before={allocated_before} after={allocated_after} freed={bytes_freed} expected_at_least={}",
        live_bytes * 70 / 100
    );

    for (block, expected_hash) in live_hashes.iter().enumerate() {
        let readback = read_logical_block(&paths, (block * BLOCK_BYTES) as u64)
            .expect("read reclaimed live block")
            .expect("live block survives reclamation");
        assert_eq!(
            <[u8; 32]>::from(Sha256::digest(&readback.payload)),
            *expected_hash,
            "payload changed at logical block {block}"
        );
    }
    assert!(
        vm_hwm_kib <= 128 * 1024,
        "large reclaim exceeded 128 MiB high-water RSS: {vm_hwm_kib} KiB"
    );

    let baseline_median = median_u128(&baseline_samples);
    let concurrent_median = median_u128(&concurrent_samples);
    let regression = concurrent_median as f64 / baseline_median as f64 - 1.0;
    assert!(
        regression <= 0.10,
        "other-cartridge write median regressed {:.2}% during reclaim (baseline={}ns current={}ns)",
        regression * 100.0,
        baseline_median,
        concurrent_median
    );
    println!(
        "TAPE_RECLAIM_ACCEPTANCE payload_bytes={} live_bytes={} allocated_before_bytes={} allocated_after_bytes={} net_freed_bytes={} processed_segments={} vm_hwm_kib={} max_public_lock_wait_us={} probe_warmup_rounds={} baseline_probe_median_ns={} concurrent_probe_median_ns={} other_cart_write_regression_percent={:.2} elapsed_ms={}",
        total_write_bytes,
        live_bytes,
        allocated_before,
        allocated_after,
        bytes_freed,
        processed_segments,
        vm_hwm_kib,
        max_public_lock_wait_us,
        probe_rounds,
        baseline_median,
        concurrent_median,
        regression * 100.0,
        started_at.elapsed().as_millis()
    );
}

#[cfg(target_os = "linux")]
#[test]
#[ignore = "invoked as a child by linux_large_reclaim_acceptance"]
fn linux_reclaim_worker_child() {
    const MAX_ROUNDS: usize = 4096;
    let fixture_root = std::env::var_os("HOLO_TAPE_TEST_ROOT")
        .map(std::path::PathBuf::from)
        .expect("test harness must set HOLO_TAPE_TEST_ROOT");
    let fixture_root = fs::canonicalize(fixture_root).expect("canonicalize fixture");
    let pool_root = fixture_root.join("pool");
    let runtime_dir = fixture_root.join("runtime");
    let paths = LayoutPaths::for_cartridge(&pool_root, "large-library", "large-cart");
    let mut cursor: Option<String> = None;
    let mut processed_segments = 0u32;
    let mut max_public_lock_wait = std::time::Duration::ZERO;
    let mut copy_phase = 0u32;
    let mut lock_probed = false;

    for round in 0..MAX_ROUNDS {
        let mut last_phase = None;
        let mut progress = |phase, _verified_bytes, _verified_records| {
            if phase == super::maintenance_progress::MaintenancePhase::Copy {
                if last_phase != Some(phase) {
                    copy_phase += 1;
                    fs::write(
                        fixture_root.join(format!("copy-phase-{copy_phase}")),
                        b"copy started",
                    )
                    .expect("signal copy phase to parent");
                }
                if !lock_probed {
                    let lock_started = Instant::now();
                    let lock = FilesystemLock::acquire(&pool_root, &runtime_dir)
                        .expect("public filesystem lock must be available during copy");
                    max_public_lock_wait = max_public_lock_wait.max(lock_started.elapsed());
                    drop(lock);
                    lock_probed = true;
                }
            }
            last_phase = Some(phase);
            Ok(())
        };
        let report = super::offline_reclaim::reclaim_one_segment_with_cursor_and_progress(
            &paths,
            &pool_root,
            "large-library",
            "large-cart",
            &runtime_dir,
            cursor.as_deref(),
            &mut progress,
        )
        .expect("reclaim next bounded segment");
        processed_segments = processed_segments.saturating_add(report.processed_segments);
        cursor = report.scan_cursor;
        if !report.has_more {
            break;
        }
        assert!(
            round + 1 < MAX_ROUNDS,
            "reclaimer did not drain the bounded fixture"
        );
    }
    assert!(lock_probed, "no eligible segment reached copy phase");
    let vm_hwm_kib = fs::read_to_string("/proc/self/status")
        .expect("read worker high-water RSS")
        .lines()
        .find_map(|line| line.strip_prefix("VmHWM:"))
        .and_then(|value| value.split_whitespace().next())
        .and_then(|value| value.parse::<u64>().ok())
        .expect("parse worker VmHWM");
    fs::write(
        fixture_root.join("reclaim-worker-report"),
        format!(
            "processed_segments={processed_segments} vm_hwm_kib={vm_hwm_kib} max_public_lock_wait_us={} copy_phases={copy_phase}",
            max_public_lock_wait.as_micros()
        ),
    )
    .expect("write worker report");
}

#[cfg(target_os = "linux")]
fn allocated_bytes_in_tree(root: &std::path::Path) -> u64 {
    let metadata = fs::symlink_metadata(root).expect("inspect allocated test path");
    assert!(
        !metadata.file_type().is_symlink(),
        "test path must not be a link"
    );
    if metadata.is_dir() {
        fs::read_dir(root)
            .expect("read allocated test directory")
            .map(|entry| allocated_bytes_in_tree(&entry.expect("directory entry").path()))
            .sum()
    } else {
        metadata.blocks().saturating_mul(512)
    }
}

#[cfg(target_os = "linux")]
fn median_u128(samples: &[u128]) -> u128 {
    assert!(
        !samples.is_empty(),
        "at least one timing sample is required"
    );
    let mut sorted = samples.to_vec();
    sorted.sort_unstable();
    sorted[sorted.len() / 2]
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
