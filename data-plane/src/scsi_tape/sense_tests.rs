use super::commands_core::{execute_with_sense, CoreCommand};
use super::error::TapeError;
use super::sense::{resolve_sense_for_error, sense_for_context, SenseContext};
use super::state::TapeState;
use super::test_utils::drive_profile_without_custom_vpd;

#[test]
fn maps_baseline_sense_contexts() {
    let not_ready = sense_for_context(SenseContext::NotReady);
    assert_eq!(not_ready.status, 0x02);
    assert_eq!(not_ready.sense_key, 0x02);

    let illegal = sense_for_context(SenseContext::IllegalRequest);
    assert_eq!(illegal.sense_key, 0x05);

    let blank_check = sense_for_context(SenseContext::BlankCheckEod);
    assert_eq!(blank_check.sense_key, 0x08);
    assert_eq!(blank_check.asc, 0x00);
    assert_eq!(blank_check.ascq, 0x05);
    assert!(blank_check.information_valid);
    assert_eq!(blank_check.information, 1);

    let denied = sense_for_context(SenseContext::AccessDenied);
    assert_eq!(denied.sense_key, 0x07);

    let data_protect = sense_for_context(SenseContext::DataProtect);
    assert_eq!(data_protect.sense_key, 0x07);
    assert_eq!(data_protect.asc, 0x30);
    assert_eq!(data_protect.ascq, 0x0C);

    let medium = sense_for_context(SenseContext::MediumError);
    assert_eq!(medium.sense_key, 0x03);
    assert_eq!(medium.asc, 0x11);
    assert_eq!(medium.ascq, 0x00);

    let reservation_conflict = sense_for_context(SenseContext::ReservationConflict);
    assert_eq!(reservation_conflict.status, 0x18);

    let invalid_release = sense_for_context(SenseContext::InvalidPersistentRelease);
    assert_eq!(invalid_release.sense_key, 0x05);
    assert_eq!(invalid_release.asc, 0x26);
    assert_eq!(invalid_release.ascq, 0x04);
}

#[test]
fn resolves_sense_for_errors() {
    let frame = resolve_sense_for_error(&TapeError::UnsupportedVpdPage(0xDE));
    assert_eq!(frame.sense_key, 0x05);

    let unsupported_mode = resolve_sense_for_error(&TapeError::UnsupportedModePage(0x99));
    assert_eq!(unsupported_mode.sense_key, 0x05);

    let unsupported_log = resolve_sense_for_error(&TapeError::UnsupportedLogPage(0x77));
    assert_eq!(unsupported_log.sense_key, 0x05);

    let fixed_mismatch = resolve_sense_for_error(&TapeError::FixedBlockSizeMismatch {
        expected: 1024,
        actual: 512,
    });
    assert_eq!(fixed_mismatch.sense_key, 0x05);

    let not_found = resolve_sense_for_error(&TapeError::NotFound("eod".to_string()));
    assert_eq!(not_found.sense_key, 0x08);
    assert_eq!(not_found.asc, 0x00);
    assert_eq!(not_found.ascq, 0x05);
    assert!(not_found.information_valid);
    assert_eq!(not_found.information, 1);

    let denied = resolve_sense_for_error(&TapeError::AccessDenied("acl".to_string()));
    assert_eq!(denied.sense_key, 0x07);

    let worm = resolve_sense_for_error(&TapeError::WormWriteProtected);
    assert_eq!(worm.sense_key, 0x07);
    assert_eq!(worm.asc, 0x30);

    let overflow = resolve_sense_for_error(&TapeError::VolumeOverflow);
    assert_eq!(overflow.sense_key, 0x0D);
    assert!(overflow.end_of_medium);

    let pr_conflict = resolve_sense_for_error(&TapeError::ReservationConflict("owner".to_string()));
    assert_eq!(pr_conflict.status, 0x18);

    let invalid_release = resolve_sense_for_error(&TapeError::InvalidReservationRelease(
        "not owner".to_string(),
    ));
    assert_eq!(invalid_release.sense_key, 0x05);
    assert_eq!(invalid_release.asc, 0x26);
    assert_eq!(invalid_release.ascq, 0x04);

    let storage = resolve_sense_for_error(&TapeError::Storage(
        crate::storage::StorageError::Corrupt("bad payload".to_string()),
    ));
    assert_eq!(storage.sense_key, 0x03);
    assert_eq!(storage.asc, 0x11);
    assert_eq!(storage.ascq, 0x00);
}

#[test]
fn execute_with_sense_returns_illegal_request_for_unsupported_vpd() {
    let profile = drive_profile_without_custom_vpd();
    let mut state = TapeState::new("drive-1");

    let sense = execute_with_sense(
        &mut state,
        CoreCommand::InquiryVpd {
            profile,
            page_code: 0xC1,
            serial_seed: "1001".to_string(),
        },
    )
    .expect_err("unsupported page should map to sense");

    assert_eq!(sense.sense_key, 0x05);
    assert_eq!(sense.asc, 0x24);
}

#[test]
fn execute_with_sense_returns_not_ready_for_mam_without_media() {
    let mut state = TapeState::new("drive-2");

    let sense = execute_with_sense(
        &mut state,
        CoreCommand::MamBaseline {
            barcode: "VOL003".to_string(),
            media_type: "LTO9".to_string(),
            capacity_bytes: 100,
            remaining_bytes: 50,
        },
    )
    .expect_err("mam query without media should fail");

    assert_eq!(sense.sense_key, 0x02);
    assert_eq!(sense.asc, 0x3A);
}
