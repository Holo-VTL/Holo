use std::collections::BTreeMap;

use super::identity::{DeviceIdentityProfile, DeviceType};

pub(super) fn drive_profile_without_custom_vpd() -> DeviceIdentityProfile {
    DeviceIdentityProfile {
        device_type: DeviceType::Drive,
        vendor: "HOLO".to_string(),
        product: "LTO-9 VDRIVE".to_string(),
        revision: "0100".to_string(),
        inquiry_len: 96,
        ansi_version: 0x05,
        response_data_format: 0x02,
        protect_flags: 0x00,
        mchanger_flags: 0x80,
        linked_flags: 0x00,
        standard_vendor_prefix: String::new(),
        standard_vendor_include_serial: false,
        standard_vendor_suffix: String::new(),
        barcode_scanner_vendor_specific_19: false,
        serial_prefix: "VT".to_string(),
        serial_suffix: "D".to_string(),
        serial_vpd_suffix: String::new(),
        serial_len: 12,
        supported_vpd_pages: vec![0x00, 0x80, 0x83],
        custom_vpd_pages: BTreeMap::new(),
    }
}

pub(super) fn drive_profile_with_custom_vpd() -> DeviceIdentityProfile {
    let mut profile = drive_profile_without_custom_vpd();
    profile.supported_vpd_pages.push(0xC0);
    profile
        .custom_vpd_pages
        .insert(0xC0, vec![0x11, 0x22, 0x33]);
    profile
}
