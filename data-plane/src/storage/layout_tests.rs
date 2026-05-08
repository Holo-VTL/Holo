use std::fs;

use super::layout::{
    initialize_layout, load_layout, LayoutPaths, SegmentHeader, SegmentKind, STORAGE_LAYOUT_MAGIC,
    STORAGE_LAYOUT_VERSION,
};
use super::segment::{encode_v2_header_copy, read_segment_file, SEGMENT_HEADER_V2_TOTAL_SIZE};

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
fn initializes_and_loads_layout_with_valid_headers() {
    let paths = test_paths("layout-init");
    let snapshot = initialize_layout(&paths).expect("layout init should pass");
    assert_eq!(snapshot.checkpoint.page_id, 1);

    let loaded = load_layout(&paths).expect("layout load should pass");
    assert_eq!(loaded.checkpoint.epoch, 1);

    let (header, _) = read_segment_file(&paths.blk_map_file, SegmentKind::BlkMap)
        .expect("blk map segment should be readable");
    assert_eq!(header.kind, SegmentKind::BlkMap);
}

#[test]
fn load_layout_does_not_read_large_data_payload() {
    let paths = test_paths("layout-large-data");
    initialize_layout(&paths).expect("layout init should pass");

    let payload_len = 2 * 1024 * 1024 * 1024u64;
    let header = SegmentHeader {
        magic: STORAGE_LAYOUT_MAGIC,
        version: STORAGE_LAYOUT_VERSION,
        kind: SegmentKind::Data,
        segment_id: 1,
        sequence: 2,
        payload_len,
        checksum: 0,
    };
    let copy = encode_v2_header_copy(&header);
    let mut encoded = Vec::with_capacity(SEGMENT_HEADER_V2_TOTAL_SIZE);
    encoded.extend_from_slice(&copy);
    encoded.extend_from_slice(&copy);
    fs::write(&paths.data_file, encoded).expect("rewrite data header");
    let file = fs::OpenOptions::new()
        .write(true)
        .open(&paths.data_file)
        .expect("open data segment");
    file.set_len(SEGMENT_HEADER_V2_TOTAL_SIZE as u64 + payload_len)
        .expect("make sparse data payload");

    let loaded = load_layout(&paths).expect("layout load should only validate data shape");
    assert_eq!(loaded.checkpoint.epoch, 1);
}

#[test]
fn detects_corrupt_segment_header_copies() {
    let paths = test_paths("layout-corrupt-headers");
    initialize_layout(&paths).expect("layout init should pass");

    let mut bytes = fs::read(&paths.lookup_file).expect("lookup segment should exist");
    bytes[44] ^= 0xAA;
    bytes[44 + super::segment::SEGMENT_HEADER_V2_COPY_SIZE] ^= 0xAA;
    fs::write(&paths.lookup_file, bytes).expect("rewrite should pass");

    let err = load_layout(&paths).expect_err("corrupt headers should fail");
    assert!(format!("{err}").contains("corrupt state"));
}

#[test]
fn storage_format_v2_checksum_zero_is_not_disabled_for_payloads() {
    let paths = test_paths("v2-zero-checksum");
    fs::create_dir_all(&paths.root).expect("create root");
    let payload = b"non-empty";
    let header = SegmentHeader {
        magic: STORAGE_LAYOUT_MAGIC,
        version: STORAGE_LAYOUT_VERSION,
        kind: SegmentKind::Lookup,
        segment_id: 4,
        sequence: 1,
        payload_len: payload.len() as u64,
        checksum: 0,
    };
    let copy = encode_v2_header_copy(&header);
    let mut bytes = Vec::new();
    bytes.extend_from_slice(&copy);
    bytes.extend_from_slice(&copy);
    bytes.extend_from_slice(payload);
    fs::write(&paths.lookup_file, bytes).expect("write malformed v2 segment");

    let err = read_segment_file(&paths.lookup_file, SegmentKind::Lookup)
        .expect_err("v2 checksum zero must not disable payload validation");
    assert!(format!("{err}").contains("checksum"));
}
