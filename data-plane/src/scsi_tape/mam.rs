use crate::scsi_tape::error::TapeError;
use crate::scsi_tape::state::{MountState, TapeState};

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MamBaselineRecord {
    pub cartridge_id: String,
    pub barcode: String,
    pub media_type: String,
    pub capacity_bytes: u64,
    pub remaining_bytes: u64,
}

pub fn build_mam_baseline(
    state: &TapeState,
    barcode: impl Into<String>,
    media_type: impl Into<String>,
    capacity_bytes: u64,
    remaining_bytes: u64,
) -> Result<MamBaselineRecord, TapeError> {
    if state.mount_state == MountState::Empty {
        return Err(TapeError::NotReady("no media loaded".to_string()));
    }
    if remaining_bytes > capacity_bytes {
        return Err(TapeError::InvalidProfile(
            "remaining bytes cannot exceed capacity".to_string(),
        ));
    }

    let cartridge_id = state
        .cartridge_id
        .clone()
        .ok_or_else(|| TapeError::NotReady("missing cartridge id".to_string()))?;

    Ok(MamBaselineRecord {
        cartridge_id,
        barcode: barcode.into(),
        media_type: media_type.into(),
        capacity_bytes,
        remaining_bytes,
    })
}

pub fn encode_mam_baseline(record: &MamBaselineRecord) -> Vec<u8> {
    let mut out = Vec::new();
    write_tlv(&mut out, 0x01, record.cartridge_id.as_bytes());
    write_tlv(&mut out, 0x02, record.barcode.as_bytes());
    write_tlv(&mut out, 0x03, record.media_type.as_bytes());
    write_tlv(&mut out, 0x04, &record.capacity_bytes.to_be_bytes());
    write_tlv(&mut out, 0x05, &record.remaining_bytes.to_be_bytes());
    out
}

fn write_tlv(out: &mut Vec<u8>, tag: u8, value: &[u8]) {
    out.push(tag);
    out.extend_from_slice(&(value.len() as u16).to_be_bytes());
    out.extend_from_slice(value);
}
