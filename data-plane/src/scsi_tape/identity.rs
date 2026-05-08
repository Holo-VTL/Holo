use std::collections::BTreeMap;

use crate::scsi_tape::error::TapeError;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DeviceType {
    Changer,
    Drive,
}

impl DeviceType {
    pub fn peripheral_device_type(self) -> u8 {
        match self {
            Self::Changer => 0x08,
            Self::Drive => 0x01,
        }
    }
}

#[derive(Debug, Clone)]
pub struct DeviceIdentityProfile {
    pub device_type: DeviceType,
    pub vendor: String,
    pub product: String,
    pub revision: String,
    pub inquiry_len: u8,
    pub ansi_version: u8,
    pub response_data_format: u8,
    pub protect_flags: u8,
    pub mchanger_flags: u8,
    pub linked_flags: u8,
    pub standard_vendor_prefix: String,
    pub standard_vendor_include_serial: bool,
    pub standard_vendor_suffix: String,
    pub barcode_scanner_vendor_specific_19: bool,
    pub serial_prefix: String,
    pub serial_suffix: String,
    pub serial_vpd_suffix: String,
    pub serial_len: u8,
    pub supported_vpd_pages: Vec<u8>,
    pub custom_vpd_pages: BTreeMap<u8, Vec<u8>>,
}

impl DeviceIdentityProfile {
    pub fn validate(&self) -> Result<(), TapeError> {
        if self.inquiry_len < 36 {
            return Err(TapeError::InvalidProfile(
                "inquiry_len must be >= 36".to_string(),
            ));
        }
        if self.serial_len == 0 {
            return Err(TapeError::InvalidProfile(
                "serial_len must be > 0".to_string(),
            ));
        }

        for page in &self.supported_vpd_pages {
            if *page >= 0xC0 && !self.custom_vpd_pages.contains_key(page) {
                return Err(TapeError::InvalidProfile(format!(
                    "custom VPD page 0x{page:02X} missing payload"
                )));
            }
        }

        Ok(())
    }

    pub fn serial_for_seed(&self, seed: &str) -> Result<String, TapeError> {
        self.validate()?;
        let mut raw = format!("{}{}{}", self.serial_prefix, seed, self.serial_suffix)
            .replace(|ch: char| !(ch.is_ascii_alphanumeric() || ch == '_'), "");

        let target_len = self.serial_len as usize;
        if raw.len() > target_len {
            if target_len <= 4 {
                raw.truncate(target_len);
            } else {
                // Preserve uniqueness under truncation by keeping a short stable hash suffix.
                let hash = stable_seed_hash16(raw.as_bytes());
                let suffix = format!("{hash:04X}");
                let prefix_len = target_len.saturating_sub(suffix.len());
                raw.truncate(prefix_len);
                raw.push_str(&suffix);
            }
        }
        if raw.len() < target_len {
            raw.push_str(&"0".repeat(target_len - raw.len()));
        }

        Ok(raw)
    }

    pub fn serial_for_vpd_seed(&self, seed: &str) -> Result<String, TapeError> {
        let mut base = self.serial_for_seed(seed)?;
        if !self.serial_vpd_suffix.is_empty() {
            let suffix = self
                .serial_vpd_suffix
                .chars()
                .filter(|ch| ch.is_ascii_alphanumeric() || *ch == '_')
                .collect::<String>();
            base.push_str(&suffix);
        }
        Ok(base)
    }

    fn standard_vendor_specific_bytes(&self, serial_seed: &str) -> Result<Vec<u8>, TapeError> {
        let mut raw = self.standard_vendor_prefix.clone();
        if self.standard_vendor_include_serial {
            raw.push_str(&self.serial_for_seed(serial_seed)?);
        }
        raw.push_str(&self.standard_vendor_suffix);
        let normalized = raw
            .chars()
            .filter(|ch| ch.is_ascii_graphic() || *ch == ' ')
            .collect::<String>();
        Ok(normalized.into_bytes())
    }

    fn normalized_pages(&self) -> Vec<u8> {
        let mut pages = self.supported_vpd_pages.clone();
        if !pages.contains(&0x00) {
            pages.push(0x00);
        }
        pages.sort_unstable();
        pages.dedup();
        pages
    }
}

fn stable_seed_hash16(input: &[u8]) -> u16 {
    // FNV-1a 32-bit folded to 16 bits for compact suffix generation.
    let mut acc: u32 = 0x811C9DC5;
    for b in input {
        acc ^= u32::from(*b);
        acc = acc.wrapping_mul(16777619);
    }
    ((acc >> 16) as u16) ^ (acc as u16)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ElementClass {
    Drive,
    Slot,
    ImportExport,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ElementAddressProfile {
    pub drive_start: u16,
    pub drive_count: u16,
    pub slot_start: u16,
    pub slot_count: u16,
    pub ie_start: u16,
    pub ie_count: u16,
    pub avoltag_enabled: bool,
}

impl ElementAddressProfile {
    pub fn validate(&self) -> Result<(), TapeError> {
        ensure_non_overlap(
            (self.drive_start, self.drive_count),
            (self.slot_start, self.slot_count),
            "drive",
            "slot",
        )?;
        ensure_non_overlap(
            (self.drive_start, self.drive_count),
            (self.ie_start, self.ie_count),
            "drive",
            "ie",
        )?;
        ensure_non_overlap(
            (self.slot_start, self.slot_count),
            (self.ie_start, self.ie_count),
            "slot",
            "ie",
        )?;
        Ok(())
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ElementStatusEntry {
    pub class: ElementClass,
    pub start_addr: u16,
    pub count: u16,
    pub serial_tags: Vec<String>,
}

pub fn standard_inquiry_bytes(
    profile: &DeviceIdentityProfile,
    serial_seed: &str,
) -> Result<Vec<u8>, TapeError> {
    profile.validate()?;

    let mut out = vec![0u8; profile.inquiry_len as usize];
    out[0] = profile.device_type.peripheral_device_type();
    out[1] = 0x80;
    out[2] = profile.ansi_version;
    out[3] = profile.response_data_format;
    out[4] = profile.inquiry_len.saturating_sub(5);
    out[5] = profile.protect_flags;
    out[6] = profile.mchanger_flags;
    out[7] = profile.linked_flags;

    write_padded_ascii(&mut out, 8, 8, &profile.vendor);
    write_padded_ascii(&mut out, 16, 16, &profile.product);
    write_padded_ascii(&mut out, 32, 4, &profile.revision);

    if out.len() > 36 {
        let vendor_end = usize::min(56, out.len());
        out[36..vendor_end].fill(b' ');

        let vendor_specific = profile.standard_vendor_specific_bytes(serial_seed)?;
        let vendor_copy_len = usize::min(vendor_specific.len(), vendor_end - 36);
        if vendor_copy_len > 0 {
            out[36..36 + vendor_copy_len].copy_from_slice(&vendor_specific[..vendor_copy_len]);
        }

        if profile.barcode_scanner_vendor_specific_19 && vendor_end - 36 >= 20 {
            out[36 + 19] = 0x01;
        }
    }

    Ok(out)
}

pub fn vpd_page_bytes(
    profile: &DeviceIdentityProfile,
    page_code: u8,
    serial_seed: &str,
) -> Result<Vec<u8>, TapeError> {
    profile.validate()?;

    let payload = match page_code {
        0x00 => profile.normalized_pages(),
        0x80 => profile.serial_for_vpd_seed(serial_seed)?.into_bytes(),
        0x83 => {
            let serial = profile.serial_for_vpd_seed(serial_seed)?;
            let mut identifier = Vec::with_capacity(8 + 16 + serial.len());
            let mut vendor = [b' '; 8];
            let mut product = [b' '; 16];
            write_padded_ascii(&mut vendor, 0, 8, &profile.vendor);
            write_padded_ascii(&mut product, 0, 16, &profile.product);
            identifier.extend_from_slice(&vendor);
            identifier.extend_from_slice(&product);
            identifier.extend_from_slice(serial.as_bytes());

            let mut payload = Vec::with_capacity(4 + identifier.len());
            payload.push(0x02); // ASCII
            payload.push(0x01); // T10 vendor identifier
            payload.push(0x00);
            payload.push(identifier.len() as u8);
            payload.extend_from_slice(&identifier);
            payload
        }
        0x03 => {
            // Firmware designation page — compatible with standard initiator
            // probes; revision in bytes 8..11.
            let mut payload = vec![0u8; 0x21];
            write_padded_ascii(&mut payload, 8, 4, &profile.revision);
            payload
        }
        0x86 => {
            // Extended inquiry VPD baseline.
            let mut payload = vec![0u8; 0x3C];
            payload[0] = 0x07; // simple/ordered/head-of-queue command support
            payload
        }
        _ => profile
            .custom_vpd_pages
            .get(&page_code)
            .cloned()
            .ok_or(TapeError::UnsupportedVpdPage(page_code))?,
    };

    let mut out = Vec::with_capacity(payload.len() + 4);
    out.push(profile.device_type.peripheral_device_type());
    out.push(page_code);
    out.extend_from_slice(&(payload.len() as u16).to_be_bytes());
    out.extend_from_slice(&payload);
    Ok(out)
}

pub fn element_status_entries(
    profile: &ElementAddressProfile,
    drive_serials: &[String],
) -> Result<Vec<ElementStatusEntry>, TapeError> {
    profile.validate()?;

    let drive_tags = if profile.avoltag_enabled {
        if drive_serials.is_empty() {
            (0..profile.drive_count)
                .map(|idx| format!("DRV{:04}", idx + 1))
                .collect()
        } else {
            drive_serials
                .iter()
                .take(profile.drive_count as usize)
                .cloned()
                .collect()
        }
    } else {
        Vec::new()
    };

    Ok(vec![
        ElementStatusEntry {
            class: ElementClass::Drive,
            start_addr: profile.drive_start,
            count: profile.drive_count,
            serial_tags: drive_tags,
        },
        ElementStatusEntry {
            class: ElementClass::Slot,
            start_addr: profile.slot_start,
            count: profile.slot_count,
            serial_tags: Vec::new(),
        },
        ElementStatusEntry {
            class: ElementClass::ImportExport,
            start_addr: profile.ie_start,
            count: profile.ie_count,
            serial_tags: Vec::new(),
        },
    ])
}

fn write_padded_ascii(buf: &mut [u8], start: usize, width: usize, value: &str) {
    if start + width > buf.len() {
        return;
    }

    let mut normalized = value
        .chars()
        .filter(|ch| ch.is_ascii_graphic() || *ch == ' ')
        .collect::<String>();

    if normalized.len() > width {
        normalized.truncate(width);
    }

    let mut out = normalized.into_bytes();
    if out.len() < width {
        out.extend_from_slice(&vec![b' '; width - out.len()]);
    }

    buf[start..start + width].copy_from_slice(&out);
}

fn ensure_non_overlap(
    lhs: (u16, u16),
    rhs: (u16, u16),
    lhs_name: &str,
    rhs_name: &str,
) -> Result<(), TapeError> {
    let (l_start, l_count) = lhs;
    let (r_start, r_count) = rhs;

    if l_count == 0 || r_count == 0 {
        return Ok(());
    }

    let l_end = l_start as u32 + l_count as u32 - 1;
    let r_end = r_start as u32 + r_count as u32 - 1;

    let overlap = l_start as u32 <= r_end && r_start as u32 <= l_end;
    if overlap {
        return Err(TapeError::InvalidProfile(format!(
            "{lhs_name} and {rhs_name} element ranges overlap"
        )));
    }

    Ok(())
}
