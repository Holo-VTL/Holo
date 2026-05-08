use crate::iscsi::cdb_server::{
    changer_drive_count, CHANGER_DT_START, CHANGER_IE_START, CHANGER_MT_COUNT, CHANGER_MT_START,
    CHANGER_ST_START,
};
use crate::scsi_tape::state::TapeState;

pub(crate) fn changer_mode_page_assignment(state: &TapeState) -> Vec<u8> {
    let mut out = Vec::with_capacity(20);
    out.push(0x1D); // ELEMENT ADDRESS ASSIGNMENT PAGE
    out.push(0x12); // page length
    out.extend_from_slice(&CHANGER_MT_START.to_be_bytes());
    out.extend_from_slice(&CHANGER_MT_COUNT.to_be_bytes());
    out.extend_from_slice(&CHANGER_ST_START.to_be_bytes());
    out.extend_from_slice(&state.changer_slot_count().to_be_bytes());
    out.extend_from_slice(&CHANGER_IE_START.to_be_bytes());
    out.extend_from_slice(&state.changer_ie_count().to_be_bytes());
    out.extend_from_slice(&CHANGER_DT_START.to_be_bytes());
    out.extend_from_slice(&changer_drive_count(state).to_be_bytes());
    out.extend_from_slice(&0u16.to_be_bytes()); // reserved
    out
}

pub(crate) fn changer_mode_page_transport_geometry() -> Vec<u8> {
    vec![
        0x1E, // TRANSPORT GEOMETRY DESCRIPTOR PAGE
        0x02, // page length
        0x00, // rotate not possible
        0x00, // member number
    ]
}

pub(crate) fn changer_mode_page_device_capabilities() -> Vec<u8> {
    vec![
        0x1F, // DEVICE CAPABILITIES PAGE
        0x12, // page length
        0x0E, // st/ie/dt capabilities
        0x00, // reserved
        0x0E, // mt move
        0x0E, // st move
        0x0E, // ie move
        0x0E, // dt move
        0x00, 0x00, 0x00, 0x00, // reserved
        0x00, // mt exchange
        0x0E, // st exchange
        0x0E, // ie exchange
        0x0E, // dt exchange
        0x00, 0x00, 0x00, 0x00, // reserved
    ]
}

pub(crate) fn changer_mode_pages(state: &TapeState, page_code: u8) -> Option<Vec<u8>> {
    let mut pages = Vec::new();
    match page_code {
        0x1D => pages.extend_from_slice(&changer_mode_page_assignment(state)),
        0x1E => pages.extend_from_slice(&changer_mode_page_transport_geometry()),
        0x1F => pages.extend_from_slice(&changer_mode_page_device_capabilities()),
        0x3F => {
            pages.extend_from_slice(&changer_mode_page_assignment(state));
            pages.extend_from_slice(&changer_mode_page_transport_geometry());
            pages.extend_from_slice(&changer_mode_page_device_capabilities());
        }
        _ => return None,
    }
    Some(pages)
}
