use std::fs;

use super::compression::CompressionCodec;
use super::dedup::{
    fingerprint128, load_dedup_index, persist_dedup_entries, rebuild_ref_counts,
    upsert_dedup_entry, DedupIndexEntry, DedupUpsertResult, DEDUP_IDENTITY_BLAKE3_128,
};
use super::layout::{initialize_layout, LayoutPaths};

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

fn build_entry(payload: &[u8], stored_blob_id: u64) -> DedupIndexEntry {
    let (fp_hi, fp_lo) = fingerprint128(payload);
    DedupIndexEntry {
        entry_id: 0,
        fingerprint_hi: fp_hi,
        fingerprint_lo: fp_lo,
        payload_checksum: super::layout::checksum32(payload),
        logical_len: payload.len() as u32,
        stored_blob_id,
        stored_len: payload.len() as u32,
        compression: CompressionCodec::None,
        identity_version: DEDUP_IDENTITY_BLAKE3_128,
        ref_count: 1,
    }
}

#[test]
fn upsert_hit_increments_refcount() {
    let paths = test_paths("dedup-hit");
    initialize_layout(&paths).expect("layout init should pass");

    let payload = b"same-payload";
    let inserted =
        upsert_dedup_entry(&paths.dedup_file, build_entry(payload, 1)).expect("insert should pass");
    let inserted_id = match inserted {
        DedupUpsertResult::Inserted(entry) => entry.entry_id,
        _ => panic!("expected inserted result"),
    };

    let hit =
        upsert_dedup_entry(&paths.dedup_file, build_entry(payload, 1)).expect("hit should pass");
    match hit {
        DedupUpsertResult::Hit(entry) => {
            assert_eq!(entry.entry_id, inserted_id);
            assert_eq!(entry.ref_count, 2);
        }
        _ => panic!("expected dedup hit"),
    }
}

#[test]
fn upsert_hit_rejects_refcount_overflow() {
    let paths = test_paths("dedup-overflow");
    initialize_layout(&paths).expect("layout init should pass");

    let payload = b"same-payload";
    let inserted =
        upsert_dedup_entry(&paths.dedup_file, build_entry(payload, 1)).expect("insert should pass");
    let mut entries = match inserted {
        DedupUpsertResult::Inserted(entry) => vec![entry],
        _ => panic!("expected inserted result"),
    };
    entries[0].ref_count = u32::MAX;
    persist_dedup_entries(&paths.dedup_file, &entries).expect("persist saturated entry");

    let err = upsert_dedup_entry(&paths.dedup_file, build_entry(payload, 1))
        .expect_err("overflow must fail");
    assert!(format!("{err}").contains("refcount overflow"));
}

#[test]
fn collision_path_creates_isolated_entry() {
    let paths = test_paths("dedup-collision");
    initialize_layout(&paths).expect("layout init should pass");

    let mut first = build_entry(b"payload-a", 1);
    first.fingerprint_hi = 7;
    first.fingerprint_lo = 11;
    upsert_dedup_entry(&paths.dedup_file, first).expect("insert first should pass");

    let mut second = build_entry(b"payload-b", 2);
    second.fingerprint_hi = 7;
    second.fingerprint_lo = 11;
    let result =
        upsert_dedup_entry(&paths.dedup_file, second).expect("insert collision should pass");

    match result {
        DedupUpsertResult::CollisionInserted(entry) => {
            assert_eq!(entry.stored_blob_id, 2);
        }
        _ => panic!("expected collision insertion"),
    }

    let (_seq, entries) = load_dedup_index(&paths.dedup_file).expect("load dedup should pass");
    assert_eq!(entries.len(), 2);
}

#[test]
fn rebuild_refcount_repairs_state_from_active_refs() {
    let paths = test_paths("dedup-rebuild");
    initialize_layout(&paths).expect("layout init should pass");

    let inserted = upsert_dedup_entry(&paths.dedup_file, build_entry(b"payload-c", 1))
        .expect("insert should pass");
    let entry_id = match inserted {
        DedupUpsertResult::Inserted(entry) => entry.entry_id,
        _ => panic!("expected inserted result"),
    };

    let updated = rebuild_ref_counts(&paths.dedup_file, &[entry_id, entry_id, entry_id])
        .expect("rebuild should pass");
    assert_eq!(updated, 1);

    let (_seq, entries) = load_dedup_index(&paths.dedup_file).expect("load dedup should pass");
    assert_eq!(entries[0].ref_count, 3);
}
