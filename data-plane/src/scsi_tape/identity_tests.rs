use super::commands_core::{execute, CoreCommand, CoreResponse};
use super::identity::{
    element_status_entries, standard_inquiry_bytes, vpd_page_bytes, ElementAddressProfile,
};
use super::profiles::{resolve_changer_profile, resolve_drive_profile};
use super::state::TapeState;
use super::test_utils::drive_profile_with_custom_vpd;

#[test]
fn builds_standard_inquiry_bytes_from_profile() {
    let profile = drive_profile_with_custom_vpd();
    let inquiry = standard_inquiry_bytes(&profile, "0001").expect("inquiry should pass");

    assert_eq!(inquiry.len(), 96);
    assert_eq!(inquiry[0], 0x01);
    assert_eq!(String::from_utf8_lossy(&inquiry[8..16]).trim(), "HOLO");
    assert_eq!(
        String::from_utf8_lossy(&inquiry[16..32]).trim(),
        "LTO-9 VDRIVE"
    );
    assert_eq!(String::from_utf8_lossy(&inquiry[32..36]).trim(), "0100");
}

#[test]
fn builds_required_and_custom_vpd_pages() {
    let profile = drive_profile_with_custom_vpd();

    let page00 = vpd_page_bytes(&profile, 0x00, "0001").expect("page 00 should pass");
    assert_eq!(page00[1], 0x00);
    assert!(page00[4..].contains(&0x80));
    assert!(page00[4..].contains(&0x83));
    assert!(page00[4..].contains(&0xC0));

    let page80 = vpd_page_bytes(&profile, 0x80, "1234").expect("page 80 should pass");
    assert_eq!(page80[1], 0x80);
    assert_eq!(page80.len(), 4 + 12);

    let pagec0 = vpd_page_bytes(&profile, 0xC0, "0001").expect("page c0 should pass");
    assert_eq!(pagec0[1], 0xC0);
    assert_eq!(&pagec0[4..], &[0x11, 0x22, 0x33]);
}

#[test]
fn projects_element_status_with_avoltag() {
    let profile = ElementAddressProfile {
        drive_start: 256,
        drive_count: 2,
        slot_start: 1024,
        slot_count: 8,
        ie_start: 2048,
        ie_count: 2,
        avoltag_enabled: true,
    };

    let entries = element_status_entries(&profile, &["DRV-A".to_string(), "DRV-B".to_string()])
        .expect("element status should pass");

    assert_eq!(entries[0].start_addr, 256);
    assert_eq!(entries[0].count, 2);
    assert_eq!(entries[0].serial_tags, vec!["DRV-A", "DRV-B"]);
}

#[test]
fn rejects_overlapping_element_ranges() {
    let profile = ElementAddressProfile {
        drive_start: 10,
        drive_count: 5,
        slot_start: 12,
        slot_count: 8,
        ie_start: 30,
        ie_count: 1,
        avoltag_enabled: false,
    };

    let err = element_status_entries(&profile, &[]).expect_err("overlap should fail");
    assert!(format!("{err}").contains("overlap"));
}

#[test]
fn command_core_routes_standard_inquiry() {
    let profile = drive_profile_with_custom_vpd();
    let mut state = TapeState::new("drive-1");

    let response = execute(
        &mut state,
        CoreCommand::InquiryStandard {
            profile,
            serial_seed: "0001".to_string(),
        },
    )
    .expect("command execution should pass");

    match response {
        CoreResponse::Bytes(payload) => assert_eq!(payload[0], 0x01),
        _ => panic!("unexpected response type"),
    }
}

#[test]
fn ibm_3584_changer_inquiry_matches_legacy_markers() {
    let profile = resolve_changer_profile("ibm-03584l32");
    let inquiry = standard_inquiry_bytes(&profile, "seed3584").expect("inquiry should pass");
    let page80 = vpd_page_bytes(&profile, 0x80, "seed3584").expect("page 80 should pass");

    assert_eq!(inquiry[0], 0x08); // medium changer PDT
    assert_eq!(inquiry[2], 0x03); // ANSI SCSI-3
    assert_eq!(inquiry[6], 0x20); // legacy mchanger barcode bit
    assert_eq!(&inquiry[36..38], b"AB"); // IBM vendor-specific prefix
    assert!(page80.ends_with(b"0400")); // legacy 3584 serial suffix
}

#[test]
fn ibm_ts3100_changer_inquiry_matches_legacy_markers() {
    let profile = resolve_changer_profile("ibm-3573-tl");
    let inquiry = standard_inquiry_bytes(&profile, "seed3100").expect("inquiry should pass");
    let page80 = vpd_page_bytes(&profile, 0x80, "seed3100").expect("page 80 should pass");

    assert_eq!(inquiry[0], 0x08); // medium changer PDT
    assert_eq!(inquiry[2], 0x05); // SPC-3 for TS3100 path
    assert_eq!(inquiry[36 + 19], 0x01); // legacy barcode scanner marker
    assert!(page80.ends_with(b"_LL3")); // legacy TS3100 serial suffix
}

#[test]
fn serial_truncation_keeps_drive_identity_unique() {
    let profile = resolve_drive_profile("ibm-ult3580-td6");
    let serial_a = profile
        .serial_for_seed("uat-drive-01")
        .expect("serial a should build");
    let serial_b = profile
        .serial_for_seed("uat-drive-02")
        .expect("serial b should build");
    assert_eq!(serial_a.len(), profile.serial_len as usize);
    assert_eq!(serial_b.len(), profile.serial_len as usize);
    assert_ne!(serial_a, serial_b, "drive serials must stay unique");
}
