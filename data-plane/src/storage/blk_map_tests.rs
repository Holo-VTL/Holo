use super::{
    append_blk_map_record, load_blk_map_records, mark_blk_map_stale, read_segment_file,
    persist_blk_map_records, BlkMapRecord, BlkMapState, PayloadChecksumAlgorithm,
    EXTENDED_RECORD_V2_SIZE, LOG_PREFIX, LOG_PREFIX_V2,
};
use crate::storage::compression::CompressionCodec;
use crate::storage::layout::SegmentKind;
use crate::storage::segment::write_segment_file;
use std::fs;
use std::path::PathBuf;
use std::time::{SystemTime, UNIX_EPOCH};

fn test_path(name: &str) -> PathBuf {
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("clock")
        .as_nanos();
    let root = std::env::temp_dir().join(format!("holo-blk-map-{name}-{nonce}"));
    fs::create_dir_all(&root).expect("create temporary directory");
    root.join("blk_map.segment")
}

fn record(
    record_id: u64,
    logical_start: u64,
    checksum: u32,
    algorithm: PayloadChecksumAlgorithm,
) -> BlkMapRecord {
    BlkMapRecord {
        record_id,
        logical_start,
        logical_len: 8,
        physical_segment_id: 1,
        physical_offset: record_id,
        filemark_count: 0,
        state: BlkMapState::Active,
        dedup_entry_id: 0,
        compression: CompressionCodec::None,
        compressed_len: 8,
        payload_checksum: checksum,
        payload_checksum_algorithm: algorithm,
    }
}

fn encode_v2_record(record: &BlkMapRecord) -> Vec<u8> {
    record.encode()[..EXTENDED_RECORD_V2_SIZE].to_vec()
}

#[test]
fn decodes_v2_checksum_algorithm_from_legacy_record_shape() {
    let legacy_fnv = record(1, 0, 0x1234_5678, PayloadChecksumAlgorithm::Fnv1a32);
    let decoded = BlkMapRecord::decode(&encode_v2_record(&legacy_fnv)).expect("decode V2 record");
    assert_eq!(decoded.payload_checksum, 0x1234_5678);
    assert_eq!(decoded.payload_checksum_algorithm, PayloadChecksumAlgorithm::Fnv1a32);

    let legacy_disabled = record(2, 8, 0, PayloadChecksumAlgorithm::None);
    let decoded = BlkMapRecord::decode(&encode_v2_record(&legacy_disabled))
        .expect("decode V2 record with disabled checksum");
    assert_eq!(decoded.payload_checksum_algorithm, PayloadChecksumAlgorithm::None);
}

#[test]
fn v3_checksum_tag_accepts_zero_value_and_rejects_unknown_or_inconsistent_tags() {
    let checksum = record(1, 0, 0, PayloadChecksumAlgorithm::Crc32c);
    let decoded = BlkMapRecord::decode(&checksum.encode()).expect("decode tagged zero checksum");
    assert_eq!(decoded.payload_checksum_algorithm, PayloadChecksumAlgorithm::Crc32c);

    let mut unknown = checksum.encode();
    *unknown.last_mut().expect("algorithm byte") = 255;
    let err = BlkMapRecord::decode(&unknown).expect_err("unknown tag must fail");
    assert!(format!("{err}").contains("unknown blk map checksum algorithm"));

    let inconsistent = record(1, 0, 1, PayloadChecksumAlgorithm::None);
    let err = BlkMapRecord::decode(&inconsistent.encode())
        .expect_err("disabled checksum value must be zero");
    assert!(format!("{err}").contains("disabled blk map checksum"));
}

#[test]
fn appending_to_v2_preserves_old_checksum_tags_in_v3_log() {
    let path = test_path("mixed-v2-v3");
    let legacy = record(1, 0, 0x1234_5678, PayloadChecksumAlgorithm::Fnv1a32);
    let mut payload = LOG_PREFIX_V2.to_vec();
    payload.extend_from_slice(&encode_v2_record(&legacy));
    write_segment_file(&path, SegmentKind::BlkMap, 2, 1, &payload).expect("write BMV2 fixture");

    let (_, before) = load_blk_map_records(&path).expect("load BMV2 fixture");
    assert_eq!(before[0].payload_checksum_algorithm, PayloadChecksumAlgorithm::Fnv1a32);

    let new_record = record(0, 8, 0, PayloadChecksumAlgorithm::Crc32c);
    append_blk_map_record(&path, new_record).expect("append CRC32C record");

    let (_, payload) =
        read_segment_file(&path, SegmentKind::BlkMap).expect("read upgraded block map");
    assert!(payload.starts_with(LOG_PREFIX));
    let (_, records) = load_blk_map_records(&path).expect("load mixed BMV3 records");
    assert_eq!(records.len(), 2);
    assert_eq!(records[0].payload_checksum_algorithm, PayloadChecksumAlgorithm::Fnv1a32);
    assert_eq!(records[1].payload_checksum_algorithm, PayloadChecksumAlgorithm::Crc32c);

    let stale = mark_blk_map_stale(&path, 1).expect("mark legacy record stale");
    assert_eq!(stale.payload_checksum_algorithm, PayloadChecksumAlgorithm::Fnv1a32);
    let (_, records) = load_blk_map_records(&path).expect("reload stale mixed records");
    assert_eq!(records[0].payload_checksum_algorithm, PayloadChecksumAlgorithm::Fnv1a32);
    assert_eq!(records[0].state, BlkMapState::Stale);

    persist_blk_map_records(&path, &records).expect("persist mixed records");
    let (_, records) = load_blk_map_records(&path).expect("reload persisted mixed records");
    assert_eq!(records[0].payload_checksum_algorithm, PayloadChecksumAlgorithm::Fnv1a32);
    assert_eq!(records[1].payload_checksum_algorithm, PayloadChecksumAlgorithm::Crc32c);

    let _ = fs::remove_dir_all(path.parent().expect("fixture has parent"));
}
