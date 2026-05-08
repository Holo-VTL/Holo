use super::mam::{build_mam_baseline, encode_mam_baseline};
use super::state::TapeState;
use crate::storage::{initialize_layout, LayoutPaths};

#[test]
fn builds_mam_baseline_when_media_loaded() {
    let mut state = TapeState::new("drive-1");
    let root = std::env::temp_dir().join("holo-scsi-mam-tests-loaded");
    let _ = std::fs::remove_dir_all(&root);
    let paths = LayoutPaths::for_cartridge(&root, "drive-1", "cart-001");
    let snapshot = initialize_layout(&paths).expect("layout init should pass");
    state.mount("cart-001", snapshot.paths);

    let record = build_mam_baseline(&state, "VOL001", "LTO9", 18_000_000, 17_500_000)
        .expect("mam baseline should pass");

    assert_eq!(record.cartridge_id, "cart-001");
    assert_eq!(record.barcode, "VOL001");

    let encoded = encode_mam_baseline(&record);
    assert!(!encoded.is_empty());
}

#[test]
fn rejects_mam_request_without_loaded_media() {
    let state = TapeState::new("drive-2");

    let err = build_mam_baseline(&state, "VOL002", "LTO9", 100, 50)
        .expect_err("mam baseline should fail without media");

    assert!(format!("{err}").contains("not ready"));
}
