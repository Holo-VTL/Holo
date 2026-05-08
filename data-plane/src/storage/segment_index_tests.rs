use std::fs;
use std::time::{SystemTime, UNIX_EPOCH};

use super::layout::{LayoutPaths, SegmentKind};
use super::segment::read_segment_header;
use super::segment_index::{
    data_segment_path, initialize_segment_index, load_segment_index, load_segment_index_for_append,
    persist_segment_index, prepare_active_segment_for_append, record_blob_append, SegmentState,
};

fn test_paths(name: &str) -> LayoutPaths {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("clock")
        .as_nanos();
    let root = std::env::temp_dir().join(format!("holo-segment-index-{name}-{nanos}"));
    fs::create_dir_all(&root).expect("create root");
    LayoutPaths::for_cartridge(&root, "drive-a", "cart-a")
}

#[test]
fn initializes_segment_index_and_active_segment() {
    let paths = test_paths("init");
    fs::create_dir_all(&paths.root).expect("create layout root");

    let index = initialize_segment_index(&paths).expect("initialize index");

    assert_eq!(index.descriptors.len(), 1);
    assert_eq!(index.descriptors[0].state, SegmentState::Active);
    assert!(paths.segment_index_file.exists());
    assert!(data_segment_path(&paths, 0).exists());
    let header =
        read_segment_header(&data_segment_path(&paths, 0), SegmentKind::Data).expect("data header");
    assert_eq!(header.payload_len, 0);
}

#[test]
fn rotates_active_segment_when_append_would_exceed_limit() {
    let paths = test_paths("rotate");
    fs::create_dir_all(&paths.root).expect("create layout root");
    let mut index = initialize_segment_index(&paths).expect("initialize index");
    index.max_segment_size = 64;
    record_blob_append(&mut index, 0, 60, 1, 32, super::CompressionCodec::Lz4)
        .expect("record blob");
    persist_segment_index(&paths.segment_index_file, &index).expect("persist index");

    let next = prepare_active_segment_for_append(&paths, &mut index, 32, 4).expect("prepare");

    assert_eq!(next, 1);
    assert_eq!(index.descriptors[0].state, SegmentState::Sealed);
    assert_eq!(index.descriptors[1].state, SegmentState::Active);
    assert!(data_segment_path(&paths, 1).exists());
    let reloaded = load_segment_index(&paths.segment_index_file).expect("reload index");
    assert_eq!(reloaded.descriptors.len(), 2);
}

#[test]
fn append_cache_reloads_when_index_file_changes() {
    let paths = test_paths("cache-stamp");
    fs::create_dir_all(&paths.root).expect("create layout root");
    let mut index = initialize_segment_index(&paths).expect("initialize index");
    let cached = load_segment_index_for_append(&paths.segment_index_file).expect("prime cache");
    assert_eq!(cached.descriptors.len(), 1);

    index.max_segment_size = 64;
    record_blob_append(&mut index, 0, 60, 1, 32, super::CompressionCodec::Lz4)
        .expect("record blob");
    let _ = prepare_active_segment_for_append(&paths, &mut index, 32, 4).expect("prepare");
    persist_segment_index(&paths.segment_index_file, &index).expect("persist external update");

    let reloaded =
        load_segment_index_for_append(&paths.segment_index_file).expect("reload changed index");
    assert_eq!(reloaded.descriptors.len(), 2);
}
