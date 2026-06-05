use std::{collections::BTreeMap, env, sync::OnceLock};

use crate::scsi_tape::identity::{DeviceIdentityProfile, DeviceType};

const DEFAULT_DRIVE_PROFILE: &str = "ibm-ult3580-td6";
const DEFAULT_CHANGER_PROFILE: &str = "ibm-03584l32";
const PRODUCT_REVISION_FULL: &str = "6.24.0002";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DeviceRole {
    Drive,
    Changer,
}

impl DeviceRole {
    pub fn from_env() -> Self {
        match env::var("HOLO_SCSI_DEVICE_ROLE")
            .unwrap_or_else(|_| "drive".to_string())
            .trim()
            .to_ascii_lowercase()
            .as_str()
        {
            "changer" => Self::Changer,
            _ => Self::Drive,
        }
    }
}

fn drive_profile(
    vendor: &str,
    product: &str,
    revision: &str,
    serial_prefix: &str,
) -> DeviceIdentityProfile {
    DeviceIdentityProfile {
        device_type: DeviceType::Drive,
        vendor: vendor.to_string(),
        product: product.to_string(),
        revision: revision.to_string(),
        inquiry_len: 36,
        ansi_version: 0x05,
        response_data_format: 0x02,
        protect_flags: 0x00,
        mchanger_flags: 0x80,
        linked_flags: 0x00,
        standard_vendor_prefix: String::new(),
        standard_vendor_include_serial: false,
        standard_vendor_suffix: String::new(),
        barcode_scanner_vendor_specific_19: false,
        serial_prefix: serial_prefix.to_string(),
        serial_suffix: String::new(),
        serial_vpd_suffix: String::new(),
        serial_len: 12,
        supported_vpd_pages: vec![0x00, 0x03, 0x80, 0x83, 0x86],
        custom_vpd_pages: BTreeMap::new(),
    }
}

fn changer_profile(
    vendor: &str,
    product: &str,
    revision: &str,
    serial_prefix: &str,
    serial_len: u8,
) -> DeviceIdentityProfile {
    DeviceIdentityProfile {
        device_type: DeviceType::Changer,
        vendor: vendor.to_string(),
        product: product.to_string(),
        revision: revision.to_string(),
        inquiry_len: 56,
        ansi_version: 0x03,
        response_data_format: 0x02,
        protect_flags: 0x00,
        mchanger_flags: 0x00,
        linked_flags: 0x00,
        standard_vendor_prefix: PRODUCT_REVISION_FULL.to_string(),
        standard_vendor_include_serial: false,
        standard_vendor_suffix: String::new(),
        barcode_scanner_vendor_specific_19: true,
        serial_prefix: serial_prefix.to_string(),
        serial_suffix: String::new(),
        serial_vpd_suffix: String::new(),
        serial_len,
        supported_vpd_pages: vec![0x00, 0x80, 0x83],
        custom_vpd_pages: BTreeMap::new(),
    }
}

fn ibm_changer_profile(
    product: &str,
    revision: &str,
    serial_len: u8,
    ansi_version: u8,
    mchanger_flags: u8,
    serial_vpd_suffix: &str,
    barcode_scanner_vendor_specific_19: bool,
) -> DeviceIdentityProfile {
    let mut profile = changer_profile("IBM", product, revision, "IBM", serial_len);
    profile.ansi_version = ansi_version;
    profile.mchanger_flags = mchanger_flags;
    profile.standard_vendor_prefix = "AB".to_string();
    profile.standard_vendor_include_serial = true;
    profile.standard_vendor_suffix = String::new();
    profile.barcode_scanner_vendor_specific_19 = barcode_scanner_vendor_specific_19;
    profile.serial_vpd_suffix = serial_vpd_suffix.to_string();
    profile
}

/// Resolve a tape drive inquiry profile by name.
///
/// Supports both modern canonical names (e.g. `ibm-ult3580-td6`) and
/// legacy-compatible aliases for drop-in migration scenarios.
pub fn resolve_drive_profile(name: &str) -> DeviceIdentityProfile {
    match name.trim().to_ascii_lowercase().as_str() {
        // IBM LTO family (legacy 1:1)
        "drive_type_vibm_3580ult1" | "drive-type-vibm-3580ult1" | "ibm-ult3580-td1" => {
            drive_profile("IBM", "ULT3580-TD1", "D711", "IBM")
        }
        "drive_type_vibm_3580ult2" | "drive-type-vibm-3580ult2" | "ibm-ult3580-td2" => {
            drive_profile("IBM", "ULT3580-TD2", "D711", "IBM")
        }
        "drive_type_vibm_3580ult3" | "drive-type-vibm-3580ult3" | "ibm-ult3580-td3" => {
            drive_profile("IBM", "ULT3580-TD3", "D711", "IBM")
        }
        "drive_type_vibm_3580ult4" | "drive-type-vibm-3580ult4" | "ibm-ult3580-td4" => {
            drive_profile("IBM", "ULT3580-TD4", "D711", "IBM")
        }
        "drive_type_vibm_3580ult5" | "drive-type-vibm-3580ult5" | "ibm-ult3580-td5" => {
            drive_profile("IBM", "ULT3580-TD5", "D711", "IBM")
        }
        "drive_type_vibm_3580ult6" | "drive-type-vibm-3580ult6" | "ibm-ult3580-td6" => {
            drive_profile("IBM", "ULT3580-TD6", "D711", "IBM")
        }
        "ibm-ult3580-td7" => drive_profile("IBM", "ULT3580-TD7", "D711", "IBM"),
        "ibm-ult3580-td8" => drive_profile("IBM", "ULT3580-TD8", "D711", "IBM"),
        "ibm-ult3580-td9" => drive_profile("IBM", "ULT3580-TD9", "D711", "IBM"),
        "ibm-ult3580-tda" => drive_profile("IBM", "ULT3580-TDA", "D711", "IBM"),
        // PRODUCT_ID_IBM_ULT{1..6}
        "ibm-ultrium-td1" => drive_profile("IBM", "ULTRIUM-TD1", "D711", "IBM"),
        "ibm-ultrium-td2" => drive_profile("IBM", "ULTRIUM-TD2", "D711", "IBM"),
        "ibm-ultrium-td3" => drive_profile("IBM", "ULTRIUM-TD3", "D711", "IBM"),
        "ibm-ultrium-td4" => drive_profile("IBM", "ULTRIUM-TD4", "D711", "IBM"),
        "ibm-ultrium-td5" => drive_profile("IBM", "ULTRIUM-TD5", "D711", "IBM"),
        "ibm-ultrium-td6" => drive_profile("IBM", "ULTRIUM-TD6", "D711", "IBM"),

        // HP Ultrium family (legacy 1:1)
        "drive_type_vhp_ult232" | "drive-type-vhp-ult232" | "hp-ultrium-1-scsi" => {
            drive_profile("HP", "Ultrium 1-SCSI", "6.24", "HP")
        }
        "drive_type_vhp_ult448" | "drive-type-vhp-ult448" | "hp-ultrium-2-scsi" => {
            drive_profile("HP", "Ultrium 2-SCSI", "6.24", "HP")
        }
        "drive_type_vhp_ult460" | "drive-type-vhp-ult460" => {
            drive_profile("HP", "Ultrium 2-SCSI", "6.24", "HP")
        }
        "drive_type_vhp_ult960" | "drive-type-vhp-ult960" | "hp-ultrium-3-scsi" => {
            drive_profile("HP", "Ultrium 3-SCSI", "6.24", "HP")
        }
        "drive_type_vhp_ult1840" | "drive-type-vhp-ult1840" | "hp-ultrium-4-scsi" => {
            drive_profile("HP", "Ultrium 4-SCSI", "6.24", "HP")
        }
        "drive_type_vhp_ult3280" | "drive-type-vhp-ult3280" | "hp-ultrium-5-scsi" => {
            drive_profile("HP", "Ultrium 5-SCSI", "6.24", "HP")
        }
        "drive_type_vhp_ult6250" | "drive-type-vhp-ult6250" | "hp-ultrium-6-scsi" => {
            drive_profile("HP", "Ultrium 6-SCSI", "6.24", "HP")
        }
        "hpe-ultrium-7-scsi" => drive_profile("HPE", "Ultrium 7-SCSI", "6.24", "HPE"),
        "hpe-ultrium-8-scsi" => drive_profile("HPE", "Ultrium 8-SCSI", "6.24", "HPE"),
        "hpe-ultrium-9-scsi" => drive_profile("HPE", "Ultrium 9-SCSI", "6.24", "HPE"),
        "quantum-ultrium-td3" => drive_profile("QUANTUM", "ULTRIUM-TD3", "6.24", "QTM"),
        "quantum-ultrium-td4" => drive_profile("QUANTUM", "ULTRIUM-TD4", "6.24", "QTM"),
        "quantum-ultrium-td5" => drive_profile("QUANTUM", "ULTRIUM-TD5", "6.24", "QTM"),
        "quantum-ultrium-td6" => drive_profile("QUANTUM", "ULTRIUM-TD6", "6.24", "QTM"),
        "quantum-ultrium-td7" => drive_profile("QUANTUM", "ULTRIUM-TD7", "6.24", "QTM"),
        "quantum-ultrium-td8" => drive_profile("QUANTUM", "ULTRIUM-TD8", "6.24", "QTM"),
        "quantum-ultrium-td9" => drive_profile("QUANTUM", "ULTRIUM-TD9", "6.24", "QTM"),
        "quantum-ultrium-tda" => drive_profile("QUANTUM", "ULTRIUM-TDA", "6.24", "QTM"),
        "stk-t9840a" => drive_profile("STK", "T9840A", "1.00", "STK"),
        "stk-t9840b" => drive_profile("STK", "T9840B", "1.00", "STK"),
        "stk-t9840c" => drive_profile("STK", "T9840C", "1.00", "STK"),
        "stk-t9840d" => drive_profile("STK", "T9840D", "1.00", "STK"),
        "stk-t9940a" => drive_profile("STK", "T9940A", "1.00", "STK"),
        "stk-t9940b" => drive_profile("STK", "T9940B", "1.00", "STK"),
        "stk-t10000a" => drive_profile("STK", "T10000A", "1.00", "STK"),
        "stk-t10000b" => drive_profile("STK", "T10000B", "1.00", "STK"),
        "stk-t10000c" => drive_profile("STK", "T10000C", "1.00", "STK"),
        "stk-t10000d" => drive_profile("STK", "T10000D", "1.00", "STK"),
        "ibm-03592j1a" => drive_profile("IBM", "03592J1A", "1.00", "IBM"),
        "ibm-03592e05" => drive_profile("IBM", "03592E05", "1.00", "IBM"),
        "ibm-03592e06" => drive_profile("IBM", "03592E06", "1.00", "IBM"),

        // HP/Compaq/Quantum DLT/SDLT family (legacy 1:1)
        "drive_type_vhp_dltvs80" | "drive-type-vhp-dltvs80" | "hp-dlt-vs80" => {
            drive_profile("HP", "DLT VS80", "6.24", "HP")
        }
        "drive_type_vhp_dltvs160" | "drive-type-vhp-dltvs160" | "hp-dlt-vs160" => {
            drive_profile("HP", "DLT VS160", "6.24", "HP")
        }
        "drive_type_vhp_sdlt220" | "drive-type-vhp-sdlt220" | "hp-sdlt220" => {
            drive_profile("COMPAQ", "SDLT1", "6.24", "CPQ")
        }
        "drive_type_vhp_sdlt320" | "drive-type-vhp-sdlt320" | "hp-sdlt320" => {
            drive_profile("COMPAQ", "SDLT320", "6.24", "CPQ")
        }
        "drive_type_vhp_sdlt600" | "drive-type-vhp-sdlt600" | "hp-sdlt600" => {
            drive_profile("HP", "SDLT600", "6.24", "HP")
        }
        "drive_type_vquantum_sdlt220" | "drive-type-vquantum-sdlt220" | "quantum-sdlt220" => {
            drive_profile("QUANTUM", "SuperDLT1", "6.24", "QTM")
        }
        "drive_type_vquantum_sdlt320" | "drive-type-vquantum-sdlt320" | "quantum-sdlt320" => {
            drive_profile("QUANTUM", "SDLT320", "6.24", "QTM")
        }
        "drive_type_vquantum_sdlt600" | "drive-type-vquantum-sdlt600" | "quantum-sdlt600" => {
            drive_profile("QUANTUM", "SDLT600", "6.24", "QTM")
        }

        // Internal default profile.
        "holo-lto9" | "holo" => drive_profile("HOLO", "VIRTUAL TAPE", "0001", "VTX"),

        // Safe fallback.
        _ => drive_profile("IBM", "ULT3580-TD6", "D711", "IBM"),
    }
}

/// Resolve tape drive profile from process environment.
///
/// `HOLO_TAPE_DRIVE_PROFILE` examples:
/// - ibm-ult3580-td6
/// - hp-ultrium-6-scsi
/// - holo-lto9
pub fn resolve_drive_profile_from_env() -> DeviceIdentityProfile {
    static DRIVE_PROFILE: OnceLock<DeviceIdentityProfile> = OnceLock::new();
    DRIVE_PROFILE
        .get_or_init(|| {
            let profile_name = env::var("HOLO_TAPE_DRIVE_PROFILE")
                .unwrap_or_else(|_| DEFAULT_DRIVE_PROFILE.to_string());
            resolve_drive_profile(&profile_name)
        })
        .clone()
}

/// Resolve a medium changer identity profile by name.
///
/// Supports both modern canonical names and legacy-compatible aliases.
pub fn resolve_changer_profile(name: &str) -> DeviceIdentityProfile {
    match name.trim().to_ascii_lowercase().as_str() {
        // ADIC Scalar family (legacy 1:1)
        "library_type_vadic_scalar24" | "library-type-vadic-scalar24" | "adic-scalar-24" => {
            changer_profile("ADIC", "Scalar 24", "6.24", "ADIC", 24)
        }
        "library_type_vadic_scalar100" | "library-type-vadic-scalar100" | "adic-scalar-100" => {
            changer_profile("ADIC", "Scalar 100", "6.24", "ADIC", 24)
        }
        "library_type_vadic_scalari2000"
        | "library-type-vadic-scalari2000"
        | "adic-scalar-i2000" => changer_profile("ADIC", "Scalar i2000", "6.24", "ADIC", 24),
        // Extra compatibility probe profile for Windows built-in matching validation.
        "library_type_vadic_scalari500" | "library-type-vadic-scalari500" | "adic-scalar-i500" => {
            changer_profile("ADIC", "Scalar i500", "100A", "ADIC", 24)
        }

        // HP / Overland family (legacy 1:1)
        "library_type_vhp_esl9000" | "library-type-vhp-esl9000" | "hp-esl9000-series" => {
            changer_profile("HP", "ESL9000 Series", "6.24", "HP", 10)
        }
        "library_type_vhp_eslseries" | "library-type-vhp-eslseries" | "hp-esl-e-series" => {
            changer_profile("HP", "ESL E-Series", "6.24", "HP", 10)
        }
        "library_type_vhp_emlseries" | "library-type-vhp-emlseries" | "hp-eml-e-series" => {
            changer_profile("HP", "EML E-Series", "6.24", "HP", 10)
        }
        "library_type_vhp_mslseries" | "library-type-vhp-mslseries" | "hp-msl-g3-series" => {
            changer_profile("HP", "MSL G3 Series", "6.24", "HP", 10)
        }
        "hp-msl2024" | "hp-hpe-msl2024" | "hphpe-msl2024" | "hphpe-hphpe-msl2024" => {
            changer_profile("HP", "MSL2024", "6.24", "HP", 10)
        }
        "hp-msl4048" | "hp-hpe-msl4048" | "hphpe-msl4048" | "hphpe-hphpe-msl4048" => {
            changer_profile("HP", "MSL4048", "6.24", "HP", 10)
        }
        "hp-msl8096" | "hp-hpe-msl8096" | "hphpe-msl8096" | "hphpe-hphpe-msl8096" => {
            changer_profile("HP", "MSL8096", "6.24", "HP", 10)
        }
        "library_type_vhp_msl6000" | "library-type-vhp-msl6000" | "hp-msl6000-series" => {
            changer_profile("HP", "MSL6000 Series", "6.24", "HP", 10)
        }
        "hpe-msl3040" => changer_profile("HPE", "MSL3040", "6.24", "HPE", 10),
        "hpe-msl6480" => changer_profile("HPE", "MSL6480", "6.24", "HPE", 10),
        "library_type_vovl_neoseries" | "library-type-vovl-neoseries" | "overland-neo-series" => {
            changer_profile("OVERLAND", "NEO Series", "6.24", "OVL", 10)
        }
        "overland-flexstor-ii" => changer_profile("OVERLAND", "FlexStor II", "6.24", "OVL", 10),
        "overland-neoxl-multistak" => changer_profile("OVERLAND", "MULTISTAK", "6.24", "OVL", 10),
        "tandberg-neos-t24" => changer_profile("TANDBERG", "NEOs T24", "6.24", "TAN", 10),
        "tandberg-neoxl-40" => changer_profile("TANDBERG", "NEOxl 40", "6.24", "TAN", 10),
        "tandberg-neoxl-80" => changer_profile("TANDBERG", "NEOxl 80", "6.24", "TAN", 10),

        // IBM family (legacy 1:1)
        "library_type_vibm_3583" | "library-type-vibm-3583" | "ibm-ult3583-tl" => {
            ibm_changer_profile("ULT3583-TL", "6.24", 14, 0x03, 0x00, "", true)
        }
        "library_type_vibm_3584" | "library-type-vibm-3584" | "ibm-03584l32" | "ibm-03584l42" => {
            ibm_changer_profile("03584L32", "6.24", 12, 0x03, 0x20, "0400", false)
        }
        "library_type_vibm_ts3100" | "library-type-vibm-ts3100" | "ibm-3573-tl" => {
            ibm_changer_profile("3573-TL", "6.24", 12, 0x05, 0x00, "_LL3", true)
        }
        "ibm-ts3310" => ibm_changer_profile("TS3310", "6.24", 12, 0x05, 0x00, "_LL3", true),
        "ibm-ts4300" => ibm_changer_profile("TS4300", "6.24", 12, 0x05, 0x00, "_LL3", true),
        "ibm-ts4500" => ibm_changer_profile("TS4500", "6.24", 12, 0x05, 0x00, "_LL3", true),
        "ibm-diamondback" => {
            ibm_changer_profile("Diamondback", "6.24", 12, 0x05, 0x00, "_LL3", true)
        }

        // Windows matching probe variants (non-legacy aliases; keep for diagnostic sweep).
        "probe-ibm-03584l32-rev-0402" => {
            ibm_changer_profile("03584L32", "0402", 12, 0x03, 0x20, "0402", false)
        }
        "probe-ibm-03584l32-ts3500-rev-0402" => {
            ibm_changer_profile("03584L32 TS3500", "0402", 12, 0x03, 0x20, "0402", false)
        }
        "probe-ibm-03584l32-ts3500-rev-624" => {
            ibm_changer_profile("03584L32 TS3500", "6.24", 12, 0x03, 0x20, "0400", false)
        }
        "probe-adic-scalar-i500-rev-200g" => {
            changer_profile("ADIC", "Scalar i500", "200G", "ADIC", 24)
        }
        "probe-adic-scalar-i500-rev-624" => {
            changer_profile("ADIC", "Scalar i500", "6.24", "ADIC", 24)
        }

        // Quantum ATL family (legacy 1:1)
        "library_type_vquantum_m2500" | "library-type-vquantum-m2500" | "quantum-2500" => {
            changer_profile("ATL", "2500", "6.24", "QTM", 10)
        }
        "quantum-scalar-i40" => changer_profile("QUANTUM", "Scalar i40", "6.24", "QTM", 24),
        "quantum-scalar-i80" => changer_profile("QUANTUM", "Scalar i80", "6.24", "QTM", 24),
        "quantum-scalar-i6000" => changer_profile("QUANTUM", "Scalar i6000", "6.24", "QTM", 24),
        "quantum-scalar-i3" => changer_profile("QUANTUM", "Scalar i3", "6.24", "QTM", 24),
        "quantum-scalar-i6" => changer_profile("QUANTUM", "Scalar i6", "6.24", "QTM", 24),
        "quantum-superloader-3" => changer_profile("QUANTUM", "SuperLoader 3", "6.24", "QTM", 10),
        "dell-tl1000" => changer_profile("DELL", "TL1000", "6.24", "DELL", 10),
        "dell-tl2000" => changer_profile("DELL", "TL2000", "6.24", "DELL", 10),
        "dell-tl4000" => changer_profile("DELL", "TL4000", "6.24", "DELL", 10),
        "dell-ml3" => changer_profile("DELL", "ML3", "6.24", "DELL", 10),
        "dell-ml6000" => changer_profile("DELL", "ML6000", "6.24", "DELL", 10),
        "stk-l20" => changer_profile("STK", "L20", "1.00", "STK", 10),
        "stk-l80" => changer_profile("STK", "L80", "1.00", "STK", 10),
        "stk-l700" => changer_profile("STK", "L700", "1.00", "STK", 10),
        "stk-sl150" => changer_profile("STK", "SL150", "1.00", "STK", 10),
        "stk-sl3000" => changer_profile("STK", "SL3000", "1.00", "STK", 10),
        "stk-sl4000" => changer_profile("STK", "SL4000", "1.00", "STK", 10),
        "stk-sl8500" => changer_profile("STK", "SL8500", "1.00", "STK", 10),
        "spectra-t50e" => changer_profile("SPECTRA", "T50e", "1.00", "SPC", 10),
        "spectra-t120" => changer_profile("SPECTRA", "T120", "1.00", "SPC", 10),
        "spectra-t200" => changer_profile("SPECTRA", "T200", "1.00", "SPC", 10),
        "spectra-t380" => changer_profile("SPECTRA", "T380", "1.00", "SPC", 10),
        "spectra-t680" => changer_profile("SPECTRA", "T680", "1.00", "SPC", 10),
        "spectra-t950" => changer_profile("SPECTRA", "T950", "1.00", "SPC", 10),
        "spectra-t950v" => changer_profile("SPECTRA", "T950v", "1.00", "SPC", 10),
        "spectra-tfinity" => changer_profile("SPECTRA", "TFinity", "1.00", "SPC", 10),
        "spectra-tfinity-exascale" => {
            changer_profile("SPECTRA", "TFinity ExaScale", "1.00", "SPC", 10)
        }
        "spectra-stack" => changer_profile("SPECTRA", "Stack", "1.00", "SPC", 10),
        "spectra-python" => changer_profile("SPECTRA", "PYTHON", "1.00", "SPC", 10),

        // Safe fallback.
        _ => ibm_changer_profile("03584L32", "6.24", 12, 0x03, 0x20, "0400", false),
    }
}

pub fn resolve_changer_profile_from_env() -> DeviceIdentityProfile {
    static CHANGER_PROFILE: OnceLock<DeviceIdentityProfile> = OnceLock::new();
    CHANGER_PROFILE
        .get_or_init(|| {
            let profile_name = env::var("HOLO_SCSI_CHANGER_PROFILE")
                .unwrap_or_else(|_| DEFAULT_CHANGER_PROFILE.to_string());
            resolve_changer_profile(&profile_name)
        })
        .clone()
}

pub fn resolve_active_profile_from_env() -> DeviceIdentityProfile {
    match DeviceRole::from_env() {
        DeviceRole::Drive => resolve_drive_profile_from_env(),
        DeviceRole::Changer => resolve_changer_profile_from_env(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn resolves_ibm_profile() {
        let p = resolve_drive_profile("ibm-ult3580-td6");
        assert_eq!(p.vendor.trim_end(), "IBM");
        assert_eq!(p.product.trim_end(), "ULT3580-TD6");
        assert_eq!(p.revision, "D711");
    }

    #[test]
    fn resolves_hp_profile() {
        let p = resolve_drive_profile("hp-ultrium-6-scsi");
        assert_eq!(p.vendor.trim_end(), "HP");
        assert_eq!(p.product.trim_end(), "Ultrium 6-SCSI");
    }

    #[test]
    fn falls_back_to_ibm_td6_for_unknown_profile() {
        let p = resolve_drive_profile("unknown-profile");
        assert_eq!(p.vendor.trim_end(), "IBM");
        assert_eq!(p.product.trim_end(), "ULT3580-TD6");
    }

    #[test]
    fn resolves_ibm_changer_profile() {
        let p = resolve_changer_profile("ibm-03584l32");
        assert_eq!(p.device_type, DeviceType::Changer);
        assert_eq!(p.vendor.trim_end(), "IBM");
        assert_eq!(p.product.trim_end(), "03584L32");
    }

    #[test]
    fn resolves_all_legacy_drive_profiles() {
        // Legacy drive profile aliases (21 items).
        let legacy_drive_expectations = [
            ("drive_type_vhp_dltvs80", "HP", "DLT VS80"),
            ("drive_type_vhp_dltvs160", "HP", "DLT VS160"),
            ("drive_type_vhp_sdlt220", "COMPAQ", "SDLT1"),
            ("drive_type_vhp_sdlt320", "COMPAQ", "SDLT320"),
            ("drive_type_vhp_sdlt600", "HP", "SDLT600"),
            ("drive_type_vquantum_sdlt220", "QUANTUM", "SuperDLT1"),
            ("drive_type_vquantum_sdlt320", "QUANTUM", "SDLT320"),
            ("drive_type_vquantum_sdlt600", "QUANTUM", "SDLT600"),
            ("drive_type_vhp_ult232", "HP", "Ultrium 1-SCSI"),
            ("drive_type_vhp_ult448", "HP", "Ultrium 2-SCSI"),
            ("drive_type_vhp_ult460", "HP", "Ultrium 2-SCSI"),
            ("drive_type_vhp_ult960", "HP", "Ultrium 3-SCSI"),
            ("drive_type_vhp_ult1840", "HP", "Ultrium 4-SCSI"),
            ("drive_type_vhp_ult3280", "HP", "Ultrium 5-SCSI"),
            ("drive_type_vhp_ult6250", "HP", "Ultrium 6-SCSI"),
            ("drive_type_vibm_3580ult1", "IBM", "ULT3580-TD1"),
            ("drive_type_vibm_3580ult2", "IBM", "ULT3580-TD2"),
            ("drive_type_vibm_3580ult3", "IBM", "ULT3580-TD3"),
            ("drive_type_vibm_3580ult4", "IBM", "ULT3580-TD4"),
            ("drive_type_vibm_3580ult5", "IBM", "ULT3580-TD5"),
            ("drive_type_vibm_3580ult6", "IBM", "ULT3580-TD6"),
        ];
        assert_eq!(legacy_drive_expectations.len(), 21);
        for (key, expected_vendor, expected_product) in legacy_drive_expectations {
            let p = resolve_drive_profile(key);
            assert_eq!(p.device_type, DeviceType::Drive, "key={key}");
            assert_eq!(p.vendor.trim_end(), expected_vendor, "key={key}");
            assert_eq!(p.product.trim_end(), expected_product, "key={key}");
        }
    }

    #[test]
    fn resolves_all_legacy_library_profiles() {
        // Legacy library profile aliases (13 items).
        let legacy_library_expectations = [
            ("library_type_vadic_scalar24", "ADIC", "Scalar 24"),
            ("library_type_vadic_scalar100", "ADIC", "Scalar 100"),
            ("library_type_vadic_scalari2000", "ADIC", "Scalar i2000"),
            ("library_type_vhp_esl9000", "HP", "ESL9000 Series"),
            ("library_type_vhp_eslseries", "HP", "ESL E-Series"),
            ("library_type_vhp_emlseries", "HP", "EML E-Series"),
            ("library_type_vibm_3583", "IBM", "ULT3583-TL"),
            ("library_type_vibm_3584", "IBM", "03584L32"),
            ("library_type_vibm_ts3100", "IBM", "3573-TL"),
            ("library_type_vhp_mslseries", "HP", "MSL G3 Series"),
            ("library_type_vhp_msl6000", "HP", "MSL6000 Series"),
            ("library_type_vovl_neoseries", "OVERLAND", "NEO Series"),
            ("library_type_vquantum_m2500", "ATL", "2500"),
        ];
        assert_eq!(legacy_library_expectations.len(), 13);
        for (key, expected_vendor, expected_product) in legacy_library_expectations {
            let p = resolve_changer_profile(key);
            assert_eq!(p.device_type, DeviceType::Changer, "key={key}");
            assert_eq!(p.vendor.trim_end(), expected_vendor, "key={key}");
            assert_eq!(p.product.trim_end(), expected_product, "key={key}");
        }
    }

    #[test]
    fn resolves_adic_scalar_i500_probe_profile() {
        let p = resolve_changer_profile("adic-scalar-i500");
        assert_eq!(p.device_type, DeviceType::Changer);
        assert_eq!(p.vendor.trim_end(), "ADIC");
        assert_eq!(p.product.trim_end(), "Scalar i500");
        assert_eq!(p.revision, "100A");
    }

    #[test]
    fn resolves_windows_probe_variants() {
        let ibm = resolve_changer_profile("probe-ibm-03584l32-ts3500-rev-0402");
        assert_eq!(ibm.vendor.trim_end(), "IBM");
        assert_eq!(ibm.product.trim_end(), "03584L32 TS3500");
        assert_eq!(ibm.revision, "0402");

        let adic = resolve_changer_profile("probe-adic-scalar-i500-rev-200g");
        assert_eq!(adic.vendor.trim_end(), "ADIC");
        assert_eq!(adic.product.trim_end(), "Scalar i500");
        assert_eq!(adic.revision, "200G");
    }

    #[test]
    fn resolves_master_list_drive_profiles() {
        let cases = [
            ("ibm-ult3580-td9", "IBM", "ULT3580-TD9"),
            ("hpe-ultrium-9-scsi", "HPE", "Ultrium 9-SCSI"),
            ("quantum-ultrium-td8", "QUANTUM", "ULTRIUM-TD8"),
            ("stk-t10000d", "STK", "T10000D"),
            ("ibm-03592e06", "IBM", "03592E06"),
        ];
        for (key, expected_vendor, expected_product) in cases {
            let p = resolve_drive_profile(key);
            assert_eq!(p.vendor.trim_end(), expected_vendor, "key={key}");
            assert_eq!(p.product.trim_end(), expected_product, "key={key}");
        }
    }

    #[test]
    fn resolves_master_list_changer_profiles() {
        let cases = [
            ("ibm-ts4300", "IBM", "TS4300"),
            ("hp-msl8096", "HP", "MSL8096"),
            ("hphpe-hphpe-msl8096", "HP", "MSL8096"),
            ("hpe-msl3040", "HPE", "MSL3040"),
            ("quantum-scalar-i6000", "QUANTUM", "Scalar i6000"),
            ("dell-tl4000", "DELL", "TL4000"),
            ("stk-sl8500", "STK", "SL8500"),
            ("spectra-tfinity-exascale", "SPECTRA", "TFinity ExaScale"),
            ("tandberg-neoxl-80", "TANDBERG", "NEOxl 80"),
        ];
        for (key, expected_vendor, expected_product) in cases {
            let p = resolve_changer_profile(key);
            assert_eq!(p.vendor.trim_end(), expected_vendor, "key={key}");
            assert_eq!(p.product.trim_end(), expected_product, "key={key}");
        }
    }

    #[test]
    fn role_from_env_defaults_to_drive() {
        std::env::remove_var("HOLO_SCSI_DEVICE_ROLE");
        assert_eq!(DeviceRole::from_env(), DeviceRole::Drive);
        std::env::set_var("HOLO_SCSI_DEVICE_ROLE", "changer");
        assert_eq!(DeviceRole::from_env(), DeviceRole::Changer);
        std::env::remove_var("HOLO_SCSI_DEVICE_ROLE");
    }
}
