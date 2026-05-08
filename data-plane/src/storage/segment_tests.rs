use std::fs;
use std::time::{SystemTime, UNIX_EPOCH};

use super::layout::SegmentKind;
use super::segment::{append_segment_payload, read_segment_file, SEGMENT_HEADER_V2_COPY_SIZE};

fn test_segment_path(case: &str) -> std::path::PathBuf {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("clock")
        .as_nanos();
    let root = std::env::temp_dir().join(format!("holo-segment-{case}-{nanos}"));
    fs::create_dir_all(&root).expect("create root");
    root.join("data.segment")
}

#[test]
fn sync_append_publishes_payload_and_header_consistently() {
    let path = test_segment_path("sync-append");

    append_segment_payload(&path, SegmentKind::Data, 1, b"LOG\n", b"first", true)
        .expect("initial append");
    let header = append_segment_payload(&path, SegmentKind::Data, 1, b"LOG\n", b"second", true)
        .expect("second append");

    let (read_header, payload) = read_segment_file(&path, SegmentKind::Data).expect("read segment");
    assert_eq!(header.payload_len, read_header.payload_len);
    assert_eq!(payload, b"LOG\nfirstsecond");
}

#[test]
fn storage_format_v2_uses_backup_header_when_primary_is_torn() {
    let path = test_segment_path("v2-primary-torn");
    append_segment_payload(&path, SegmentKind::Data, 1, b"LOG\n", b"payload", true)
        .expect("append");

    let mut bytes = fs::read(&path).expect("read segment");
    bytes[44] ^= 0x5A;
    bytes[0] ^= 0xA5;
    fs::write(&path, bytes).expect("write torn primary");

    let (_header, payload) =
        read_segment_file(&path, SegmentKind::Data).expect("backup header should recover segment");
    assert_eq!(payload, b"LOG\npayload");
}

#[test]
fn storage_format_v2_fails_when_both_header_copies_are_invalid() {
    let path = test_segment_path("v2-both-torn");
    append_segment_payload(&path, SegmentKind::Data, 1, b"LOG\n", b"payload", true)
        .expect("append");

    let mut bytes = fs::read(&path).expect("read segment");
    bytes[44] ^= 0x5A;
    bytes[44 + SEGMENT_HEADER_V2_COPY_SIZE] ^= 0xA5;
    fs::write(&path, bytes).expect("write torn headers");

    let err = read_segment_file(&path, SegmentKind::Data)
        .expect_err("invalid v2 header copies must fail closed");
    assert!(format!("{err}").contains("corrupt state"));
}
