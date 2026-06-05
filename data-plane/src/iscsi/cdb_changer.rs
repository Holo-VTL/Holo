use std::{
    collections::BTreeMap,
    env,
    sync::OnceLock,
    time::{SystemTime, UNIX_EPOCH},
};

use crate::iscsi::cdb_drive::{
    persistent_reserve_in_drive, persistent_reserve_out_drive_with_context, truncate_reply,
    write_ascii_padded,
};
use crate::iscsi::cdb_modes::changer_mode_pages;
use crate::iscsi::cdb_server::*;
use crate::iscsi::cdb_wire::CdbResponse;
use crate::scsi_tape::commands_core::{execute_with_sense, CoreCommand};
use crate::scsi_tape::state::TapeState;

pub(crate) fn changer_type_range(state: &TapeState, element_type: u8) -> Option<(u16, u16)> {
    match element_type {
        0x01 => Some((CHANGER_MT_START, CHANGER_MT_COUNT)),
        0x02 => Some((CHANGER_ST_START, state.changer_slot_count())),
        0x03 => Some((CHANGER_IE_START, state.changer_ie_count())),
        0x04 => Some((CHANGER_DT_START, changer_drive_count(state))),
        _ => None,
    }
}

fn shared_changer_inventory_enabled(state: &TapeState) -> bool {
    let media_state_key = media_state_key_for_state(state);
    media_state_key.contains(MEDIA_STATE_KEY_SEPARATOR)
        || shared_changer_slots_path(&media_state_key).exists()
        || shared_changer_ie_path(&media_state_key).exists()
        || shared_changer_vault_path(&media_state_key).exists()
}

fn auto_archive_ie_media_to_vault(state: &mut TapeState) -> Result<(), CdbResponse> {
    if !shared_changer_inventory_enabled(state) {
        return Ok(());
    }

    let media_state_key = media_state_key_for_state(state);
    let existing = match read_shared_changer_vault(&media_state_key) {
        Ok(Some(labels)) => labels,
        Ok(None) => Vec::new(),
        Err(err) => {
            eprintln!(
                "[cdb_sync] failed to read shared changer vault drive_id={} error={err}",
                state.drive_id
            );
            Vec::new()
        }
    };

    let mut exported = existing
        .into_iter()
        .flatten()
        .filter(|label| !label.trim().is_empty())
        .collect::<Vec<_>>();
    let mut changed = false;
    for (addr, entry) in state.changer_ie_ports.iter_mut() {
        let Some(label) = entry.take() else {
            continue;
        };
        if !exported.iter().any(|existing| existing == &label) {
            exported.push(label);
        }
        state.changer_ie_impexp.insert(*addr, false);
        changed = true;
    }
    if !changed {
        return Ok(());
    }

    let ie_ports = state
        .changer_ie_ports
        .values()
        .cloned()
        .collect::<Vec<Option<String>>>();
    // Multi-file atomic IE+vault commit is a separate redesign; this path makes
    // the current write failures visible to callers instead of stderr-only.
    if let Err(err) = write_shared_changer_ie(&media_state_key, &ie_ports) {
        eprintln!(
            "[cdb_sync] failed to persist auto-cleared changer IE drive_id={} key={} error={err}",
            state.drive_id, media_state_key
        );
        return Err(internal_target_failure_response());
    }
    let vault_labels = exported.into_iter().map(Some).collect::<Vec<_>>();
    if let Err(err) = write_shared_changer_vault(&media_state_key, &vault_labels) {
        eprintln!(
            "[cdb_sync] failed to persist changer vault drive_id={} key={} error={err}",
            state.drive_id, media_state_key
        );
        return Err(internal_target_failure_response());
    }
    Ok(())
}

pub(crate) fn changer_drive_identifier_designator_for_seed(seed: &str) -> Vec<u8> {
    let profile = crate::scsi_tape::profiles::resolve_drive_profile_from_env();
    let serial = profile
        .serial_for_vpd_seed(seed)
        .unwrap_or_else(|_| "HOLO00000000".to_string());

    let mut vendor = [b' '; 8];
    let mut product = [b' '; 16];
    write_ascii_padded(&mut vendor, profile.vendor.trim_end());
    write_ascii_padded(&mut product, profile.product.trim_end());

    let mut identifier = Vec::with_capacity(8 + 16 + serial.len());
    identifier.extend_from_slice(&vendor);
    identifier.extend_from_slice(&product);
    identifier.extend_from_slice(serial.as_bytes());

    let mut designator = Vec::with_capacity(4 + identifier.len());
    designator.push(0x02); // ASCII
    designator.push(0x01); // T10 vendor identifier
    designator.push(0x00); // reserved
    designator.push(identifier.len().min(u8::MAX as usize) as u8);
    designator.extend_from_slice(&identifier[..usize::min(identifier.len(), u8::MAX as usize)]);
    designator
}

pub(crate) fn changer_serial_as_avoltag_enabled(
    profile: &crate::scsi_tape::identity::DeviceIdentityProfile,
) -> bool {
    let product = profile.product.to_ascii_uppercase();
    (profile.vendor.trim().eq_ignore_ascii_case("HP")
        || profile.vendor.trim().eq_ignore_ascii_case("HPE"))
        && (product.contains("ESL") || product.contains("EML") || product.contains("MSL"))
}

pub(crate) fn changer_volume_tag(
    state: &TapeState,
    element_type: u8,
    element_addr: u16,
    alternate: bool,
) -> [u8; 36] {
    let mut tag = [0u8; 36];
    if alternate && element_type == 0x04 {
        let drive_seed = changer_drive_state_bindings(state)
            .into_iter()
            .find_map(|(addr, seed, _)| {
                if addr == element_addr {
                    Some(seed)
                } else {
                    None
                }
            })
            .unwrap_or_else(|| state.drive_id.clone());
        let profile = crate::scsi_tape::profiles::resolve_drive_profile_from_env();
        let serial = profile
            .serial_for_vpd_seed(&drive_seed)
            .unwrap_or_else(|_| "HOLO00000000".to_string());
        let bytes = serial.as_bytes();
        // Legacy AVolTag layout: keep first 4 bytes zero, place serial after it.
        let start = 4usize;
        let copy_len = usize::min(bytes.len(), tag.len().saturating_sub(start));
        if copy_len > 0 {
            tag[start..start + copy_len].copy_from_slice(&bytes[..copy_len]);
        }
        return tag;
    }
    let label = match element_type {
        0x02 => state
            .changer_slots
            .get(&element_addr)
            .and_then(|value| value.as_ref()),
        0x03 => state
            .changer_ie_ports
            .get(&element_addr)
            .and_then(|value| value.as_ref()),
        0x04 => state
            .changer_drives
            .get(&element_addr)
            .and_then(|value| value.as_ref()),
        _ => None,
    };
    if let Some(label) = label {
        // Legacy Primary Volume Tag layout:
        // - bytes 0..32: label (left aligned, space padded)
        // - bytes 32..36: reserved (zero)
        tag[..32].fill(b' ');
        let bytes = label.trim().as_bytes();
        let copy_len = usize::min(bytes.len(), 32usize);
        if copy_len > 0 {
            tag[..copy_len].copy_from_slice(&bytes[..copy_len]);
        }
    }
    tag
}

#[derive(Debug, Clone)]
pub(crate) struct ChangerMedium {
    label: String,
    source_slot: Option<u16>,
}

pub(crate) fn changer_take_medium(state: &mut TapeState, address: u16) -> Option<ChangerMedium> {
    if let Some(entry) = state.changer_slots.get_mut(&address) {
        let label = entry.take()?;
        return Some(ChangerMedium {
            label,
            source_slot: Some(address),
        });
    }
    if let Some(entry) = state.changer_ie_ports.get_mut(&address) {
        let label = entry.take()?;
        state.changer_ie_impexp.insert(address, false);
        return Some(ChangerMedium {
            label,
            source_slot: Some(address),
        });
    }
    if let Some(entry) = state.changer_drives.get_mut(&address) {
        let label = entry.take()?;
        let source_slot = state.changer_drive_sources.get(&address).copied().flatten();
        state.changer_drive_sources.insert(address, None);
        return Some(ChangerMedium { label, source_slot });
    }
    None
}

pub(crate) fn changer_place_medium(
    state: &mut TapeState,
    address: u16,
    medium: ChangerMedium,
) -> Result<(), ()> {
    if let Some(entry) = state.changer_slots.get_mut(&address) {
        if entry.as_deref() == Some(medium.label.as_str()) {
            *entry = Some(medium.label);
            return Ok(());
        }
        if entry.is_some() {
            return Err(());
        }
        *entry = Some(medium.label);
        return Ok(());
    }
    if let Some(entry) = state.changer_ie_ports.get_mut(&address) {
        if entry.is_some() {
            return Err(());
        }
        *entry = Some(medium.label);
        state.changer_ie_impexp.insert(address, true);
        return Ok(());
    }
    if let Some(entry) = state.changer_drives.get_mut(&address) {
        if entry.is_some() {
            return Err(());
        }
        *entry = Some(medium.label);
        state
            .changer_drive_sources
            .insert(address, medium.source_slot);
        return Ok(());
    }
    Err(())
}

pub(crate) fn changer_has_element(state: &TapeState, address: u16) -> bool {
    state.changer_slots.contains_key(&address)
        || state.changer_ie_ports.contains_key(&address)
        || state.changer_drives.contains_key(&address)
}

pub(crate) fn changer_ie_accessed(state: &TapeState, source: u16, destination: u16) -> bool {
    let end = CHANGER_IE_START.saturating_add(state.changer_ie_count());
    (CHANGER_IE_START..end).contains(&source) || (CHANGER_IE_START..end).contains(&destination)
}

pub(crate) fn push_ie_accessed_unit_attention(state: &mut TapeState) {
    // ASC/ASCQ for IMPORT/EXPORT ELEMENT ACCESSED (0x28/0x01 per SMC-3).
    state.push_unit_attention(0x28, 0x01);
}

pub(crate) fn invalid_element_address_response() -> CdbResponse {
    CdbResponse::check_condition(build_sense_fixed(0x05, 0x21, 0x01))
}

pub(crate) fn source_element_empty_response() -> CdbResponse {
    CdbResponse::check_condition(build_sense_fixed(0x05, 0x3B, 0x0E))
}

pub(crate) fn destination_element_full_response() -> CdbResponse {
    CdbResponse::check_condition(build_sense_fixed(0x05, 0x3B, 0x0D))
}

pub(crate) fn move_medium_changer(state: &mut TapeState, cdb: &[u8]) -> CdbResponse {
    if cdb.len() < 8 {
        return invalid_field_in_cdb_response();
    }
    let source = u16::from_be_bytes([cdb[4], cdb[5]]);
    let destination = u16::from_be_bytes([cdb[6], cdb[7]]);
    if source == destination {
        return CdbResponse::good(vec![]);
    }

    let medium = match changer_take_medium(state, source) {
        Some(medium) => medium,
        None => {
            if changer_has_element(state, source) {
                return source_element_empty_response();
            }
            return invalid_element_address_response();
        }
    };

    if changer_place_medium(state, destination, medium.clone()).is_err() {
        // rollback source on destination failure.
        let _ = changer_place_medium(state, source, medium);
        if changer_has_element(state, destination) {
            return destination_element_full_response();
        }
        return invalid_element_address_response();
    }

    if let Err(response) = auto_archive_ie_media_to_vault(state) {
        return response;
    }

    if changer_ie_accessed(state, source, destination) {
        push_ie_accessed_unit_attention(state);
    } else {
        state.push_unit_attention(0x28, 0x00);
    }
    sync_changer_inventory_to_shared(state);
    sync_changer_mount_to_shared(state);
    CdbResponse::good(vec![])
}

pub(crate) fn exchange_medium_changer(state: &mut TapeState, cdb: &[u8]) -> CdbResponse {
    if cdb.len() < 10 {
        return invalid_field_in_cdb_response();
    }

    let source = u16::from_be_bytes([cdb[4], cdb[5]]);
    let destination_1 = u16::from_be_bytes([cdb[6], cdb[7]]);
    let destination_2 = u16::from_be_bytes([cdb[8], cdb[9]]);
    if source == destination_1 && destination_1 == destination_2 {
        return CdbResponse::good(vec![]);
    }

    let source_medium = match changer_take_medium(state, source) {
        Some(medium) => medium,
        None => {
            if changer_has_element(state, source) {
                return source_element_empty_response();
            }
            return invalid_element_address_response();
        }
    };
    let destination_1_medium = changer_take_medium(state, destination_1);
    let destination_2_medium = changer_take_medium(state, destination_2);

    // destination_1 <= source
    if changer_place_medium(state, destination_1, source_medium.clone()).is_err() {
        let _ = changer_place_medium(state, source, source_medium);
        if let Some(medium) = destination_1_medium {
            let _ = changer_place_medium(state, destination_1, medium);
        }
        if let Some(medium) = destination_2_medium {
            let _ = changer_place_medium(state, destination_2, medium);
        }
        return invalid_element_address_response();
    }

    // destination_2 <= previous destination_1
    if let Some(medium) = destination_1_medium.clone() {
        if changer_place_medium(state, destination_2, medium).is_err() {
            return invalid_element_address_response();
        }
    }

    // source <= previous destination_2
    if let Some(medium) = destination_2_medium {
        if changer_place_medium(state, source, medium).is_err() {
            return invalid_element_address_response();
        }
    }

    if let Err(response) = auto_archive_ie_media_to_vault(state) {
        return response;
    }

    if changer_ie_accessed(state, source, destination_1)
        || changer_ie_accessed(state, source, destination_2)
    {
        push_ie_accessed_unit_attention(state);
    } else {
        state.push_unit_attention(0x28, 0x00);
    }
    sync_changer_inventory_to_shared(state);
    sync_changer_mount_to_shared(state);
    CdbResponse::good(vec![])
}

pub(crate) fn mode_sense_6_changer(state: &TapeState, cdb: &[u8]) -> CdbResponse {
    let page_code = cdb.get(2).copied().unwrap_or(0) & 0x3F;
    let allocation_length = cdb.get(4).copied().unwrap_or(0) as usize;
    let pages = match changer_mode_pages(state, page_code) {
        Some(pages) => pages,
        None => return invalid_field_in_cdb_response(),
    };

    let mut out = vec![0u8; 4];
    out.extend_from_slice(&pages);
    out[0] = out.len().saturating_sub(1) as u8;
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn mode_sense_10_changer(state: &TapeState, cdb: &[u8]) -> CdbResponse {
    let page_code = cdb.get(2).copied().unwrap_or(0) & 0x3F;
    let allocation_length = if cdb.len() >= 9 {
        u16::from_be_bytes([cdb[7], cdb[8]]) as usize
    } else {
        0
    };
    let pages = match changer_mode_pages(state, page_code) {
        Some(pages) => pages,
        None => return invalid_field_in_cdb_response(),
    };

    let mut out = vec![0u8; 8];
    out.extend_from_slice(&pages);
    let mode_data_len = (out.len().saturating_sub(2)) as u16;
    out[0..2].copy_from_slice(&mode_data_len.to_be_bytes());
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn read_element_status_changer(
    state: &mut TapeState,
    cdb: &[u8],
    profile: &crate::scsi_tape::identity::DeviceIdentityProfile,
) -> CdbResponse {
    let Some(allocation_length) = try_read_be24(cdb, 7) else {
        return invalid_field_in_cdb_response();
    };
    if allocation_length == 0 {
        return CdbResponse::good(vec![]);
    }
    if allocation_length < 8 {
        return invalid_field_in_cdb_response();
    }

    let flags = cdb.get(1).copied().unwrap_or(0);
    let element_type = flags & 0x0F;
    let voltag_requested = (flags & 0x10) != 0;
    let avoltag_requested = (flags & 0x20) != 0;
    let dvcid_requested = (cdb.get(6).copied().unwrap_or(0) & 0x01) != 0;
    let start_requested = if cdb.len() >= 4 {
        u16::from_be_bytes([cdb[2], cdb[3]])
    } else {
        CHANGER_DT_START
    };
    let requested_count = if cdb.len() >= 6 {
        u16::from_be_bytes([cdb[4], cdb[5]])
    } else {
        0
    };

    let types: Vec<u8> = if element_type == 0x00 {
        // SMC-3 conventional order for "all element types" query.
        vec![0x01, 0x04, 0x03, 0x02]
    } else if changer_type_range(state, element_type).is_some() {
        vec![element_type]
    } else {
        return invalid_field_in_cdb_response();
    };
    let ie_requested = types.contains(&0x03);

    let drive_bindings = changer_drive_state_bindings(state);
    let mut drive_seed_by_addr = BTreeMap::new();
    for (addr, drive_seed, _) in drive_bindings {
        drive_seed_by_addr.insert(addr, drive_seed);
    }
    let drive_designator = changer_drive_identifier_designator_for_seed(&state.drive_id);
    let mut report_body = Vec::new();
    let mut first_reported: Option<u16> = None;
    let mut remaining = requested_count;
    let mut num_elements_avail = 0u16;

    for ty in types {
        if remaining == 0 {
            break;
        }
        let (base, total) = match changer_type_range(state, ty) {
            Some(v) => v,
            None => continue,
        };
        if total == 0 {
            continue;
        }

        let first_addr = u16::max(base, start_requested);
        if first_addr >= base.saturating_add(total) {
            continue;
        }
        let available = base.saturating_add(total) - first_addr;
        let count = available.min(remaining);
        if count == 0 {
            continue;
        }

        let include_voltag = voltag_requested || avoltag_requested;
        let include_avoltag = ty == 0x04
            && (avoltag_requested
                || (include_voltag && changer_serial_as_avoltag_enabled(profile)));
        let identifier_len = if ty == 0x04 && dvcid_requested {
            drive_designator.len()
        } else {
            4
        };
        let voltag_len = if include_voltag { 36usize } else { 0usize };
        let avoltag_len = if include_avoltag { 36usize } else { 0usize };
        let descriptor_len = 12usize + voltag_len + avoltag_len + identifier_len;
        let descriptor_total = descriptor_len * count as usize;

        report_body.push(ty); // element type code
        report_body.push(
            (if include_voltag { 0x80 } else { 0x00 })
                | (if include_avoltag { 0x40 } else { 0x00 }),
        );
        report_body.extend_from_slice(&(descriptor_len as u16).to_be_bytes());
        report_body.extend_from_slice(&(descriptor_total as u32).to_be_bytes());

        for idx in 0..count {
            let element_addr = first_addr + idx;
            if first_reported.is_none() {
                first_reported = Some(element_addr);
            }
            num_elements_avail = num_elements_avail.saturating_add(1);
            remaining = remaining.saturating_sub(1);

            report_body.extend_from_slice(&element_addr.to_be_bytes());
            let full = match ty {
                0x02 => state
                    .changer_slots
                    .get(&element_addr)
                    .and_then(|value| value.as_ref())
                    .is_some(),
                0x03 => state
                    .changer_ie_ports
                    .get(&element_addr)
                    .and_then(|value| value.as_ref())
                    .is_some(),
                0x04 => state
                    .changer_drives
                    .get(&element_addr)
                    .and_then(|value| value.as_ref())
                    .is_some(),
                _ => false,
            };
            let mut element_flags = if full { 0x09 } else { 0x08 }; // ACCESS + optional FULL
            if ty == 0x03
                && state
                    .changer_ie_impexp
                    .get(&element_addr)
                    .copied()
                    .unwrap_or(false)
            {
                element_flags |= 0x02; // IE IMPEXP flag
            }
            report_body.push(element_flags);
            report_body.push(0x00); // reserved
            report_body.push(0x00); // ASC
            report_body.push(0x00); // ASCQ
            report_body.extend_from_slice(&[0x00, 0x00, 0x00]); // reserved
            let source_addr = state
                .changer_drive_sources
                .get(&element_addr)
                .copied()
                .flatten()
                .unwrap_or(0);
            let has_source = ty == 0x04 && full && source_addr != 0;
            report_body.push(if has_source { 0x80 } else { 0x00 }); // SVALID in invert byte
            report_body.extend_from_slice(&source_addr.to_be_bytes());

            if include_voltag {
                report_body.extend_from_slice(&changer_volume_tag(state, ty, element_addr, false));
            }

            if include_avoltag {
                report_body.extend_from_slice(&changer_volume_tag(state, ty, element_addr, true));
            }

            if ty == 0x04 && dvcid_requested {
                let drive_seed = drive_seed_by_addr
                    .get(&element_addr)
                    .map(String::as_str)
                    .unwrap_or(state.drive_id.as_str());
                let drive_designator = changer_drive_identifier_designator_for_seed(drive_seed);
                report_body.extend_from_slice(&drive_designator);
            } else {
                report_body.extend_from_slice(&[0x00, 0x00, 0x00, 0x00]);
            }
        }
    }

    let mut out = vec![0u8; 8];
    out[0..2].copy_from_slice(&first_reported.unwrap_or(0xFFFF).to_be_bytes());
    out[2..4].copy_from_slice(&num_elements_avail.to_be_bytes());
    out[4..8].copy_from_slice(&(report_body.len() as u32).to_be_bytes());
    out.extend_from_slice(&report_body);
    if ie_requested {
        push_ie_accessed_unit_attention(state);
    }
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn report_luns_single_lun(cdb: &[u8]) -> CdbResponse {
    let allocation_length = if cdb.len() >= 10 {
        u32::from_be_bytes([cdb[6], cdb[7], cdb[8], cdb[9]]) as usize
    } else {
        0
    };
    if allocation_length == 0 {
        return CdbResponse::good(vec![]);
    }

    let mut out = vec![0u8; 16];
    out[0..4].copy_from_slice(&8u32.to_be_bytes()); // one LUN entry
                                                    // bytes [8..16] remain 0x00 for LUN 0
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn report_timestamp(cdb: &[u8]) -> CdbResponse {
    if cdb.len() < 12 || (cdb[1] & 0x1F) != 0x0F {
        return invalid_field_in_cdb_response();
    }
    let allocation_length = u32::from_be_bytes([cdb[6], cdb[7], cdb[8], cdb[9]]) as usize;
    if allocation_length == 0 {
        return CdbResponse::good(vec![]);
    }

    let millis = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_millis().min(0xFFFF_FFFF_FFFF) as u64)
        .unwrap_or(0);
    let millis_bytes = millis.to_be_bytes();
    let mut out = vec![0u8; 12];
    out[0..2].copy_from_slice(&0x000A_u16.to_be_bytes());
    out[3] = 0x03; // Timestamp initialized outside the SCSI SET TIMESTAMP command.
    out[4..10].copy_from_slice(&millis_bytes[2..8]);
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn log_sense_changer(cdb: &[u8]) -> CdbResponse {
    let page_code = cdb.get(2).copied().unwrap_or(0) & 0x3F;
    let allocation_length = if cdb.len() >= 9 {
        u16::from_be_bytes([cdb[7], cdb[8]]) as usize
    } else {
        0
    };
    if page_code != 0x00 {
        return invalid_field_in_cdb_response();
    }

    let out = vec![
        0x00, // page code
        0x00, // reserved
        0x00, 0x01, // page length
        0x00, // supported log page: 0x00
    ];
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn build_sense_fixed(sense_key: u8, asc: u8, ascq: u8) -> Vec<u8> {
    let mut s = vec![0u8; 18];
    s[0] = 0x70;
    s[2] = sense_key & 0x0F;
    s[7] = 10;
    s[12] = asc;
    s[13] = ascq;
    s
}

pub(crate) fn invalid_field_in_cdb_response() -> CdbResponse {
    // ILLEGAL REQUEST / INVALID FIELD IN CDB
    CdbResponse::check_condition(build_sense_fixed(0x05, 0x24, 0x00))
}

pub(crate) fn reserve_release_compat_response() -> CdbResponse {
    static STRICT_RESERVE_RELEASE: OnceLock<bool> = OnceLock::new();
    if *STRICT_RESERVE_RELEASE.get_or_init(|| {
        env::var("HOLO_SCSI_STRICT_RESERVE_RELEASE")
            .ok()
            .map(|raw| {
                matches!(
                    raw.trim().to_ascii_lowercase().as_str(),
                    "1" | "true" | "yes" | "on"
                )
            })
            .unwrap_or(false)
    }) {
        invalid_field_in_cdb_response()
    } else {
        CdbResponse::good(vec![])
    }
}

pub(crate) fn invalid_field_in_parameter_list_response() -> CdbResponse {
    // ILLEGAL REQUEST / INVALID FIELD IN PARAMETER LIST
    CdbResponse::check_condition(build_sense_fixed(0x05, 0x26, 0x00))
}

pub(crate) fn internal_target_failure_response() -> CdbResponse {
    // HARDWARE ERROR / INTERNAL TARGET FAILURE.
    CdbResponse::check_condition(build_sense_fixed(0x04, 0x44, 0x00))
}

pub(crate) fn saving_parameters_not_supported_response() -> CdbResponse {
    // ILLEGAL REQUEST / SAVING PARAMETERS NOT SUPPORTED.
    CdbResponse::check_condition(build_sense_fixed(0x05, 0x39, 0x00))
}

pub(crate) fn volume_overflow_response() -> CdbResponse {
    // VOLUME OVERFLOW sense key with EOM bit set.
    let mut sense = build_sense_fixed(0x0D, 0x00, 0x00);
    sense[2] |= 0x40; // EOM
    CdbResponse::check_condition(sense)
}

pub(crate) fn changer_unsupported_response(opcode: u8) -> CdbResponse {
    // Deterministic unsupported-opcode table for changer role.
    // Current baseline intentionally returns ILLEGAL REQUEST with INVALID COMMAND OPERATION CODE.
    eprintln!("[tcmu_handler] changer unsupported opcode 0x{opcode:02X}");
    let (sense_key, asc, ascq) = match opcode {
        0x01 | // REWIND
        0x08 | // READ(6)
        0x0A | // WRITE(6)
        0x10 | // WRITE FILEMARKS
        0x11 | // SPACE
        0x19 | // ERASE
        0x1B | // LOAD/UNLOAD
        0x34 | // READ POSITION
        0xA5 => // MOVE MEDIUM
            (0x05, 0x20, 0x00),
        _ => (0x05, 0x20, 0x00),
    };
    CdbResponse::check_condition(build_sense_fixed(sense_key, asc, ascq))
}

pub(crate) fn rezero_unit_changer(state: &mut TapeState) -> CdbResponse {
    ensure_changer_drive_topology(state);
    CdbResponse::good(vec![])
}

pub(crate) fn open_close_import_export_changer(state: &mut TapeState, cdb: &[u8]) -> CdbResponse {
    let requested = u16::from_be_bytes([
        cdb.get(2).copied().unwrap_or(0),
        cdb.get(3).copied().unwrap_or(0),
    ]);
    let element_addr = if requested == 0 {
        CHANGER_IE_START
    } else {
        requested
    };
    if !state.changer_ie_ports.contains_key(&element_addr) {
        return invalid_field_in_cdb_response();
    }
    let open = (cdb.get(4).copied().unwrap_or(0) & 0x01) != 0;
    state.changer_ie_impexp.insert(element_addr, open);
    state.push_unit_attention(0x2A, 0x01);
    CdbResponse::good(vec![])
}

#[cfg(test)]
pub(crate) fn dispatch_changer_cdb(
    state: &mut TapeState,
    cdb: &[u8],
    data_out: &[u8],
    profile: crate::scsi_tape::identity::DeviceIdentityProfile,
) -> CdbResponse {
    dispatch_changer_cdb_with_context(
        state,
        cdb,
        data_out,
        profile,
        &CdbDispatchContext::default(),
    )
}

pub(crate) fn dispatch_changer_cdb_with_context(
    state: &mut TapeState,
    cdb: &[u8],
    data_out: &[u8],
    profile: crate::scsi_tape::identity::DeviceIdentityProfile,
    context: &CdbDispatchContext,
) -> CdbResponse {
    ensure_changer_drive_topology(state);
    let opcode = cdb[0];
    if changer_opcode_requires_slot_bootstrap(opcode) {
        // First inventory operation should use shared slot state even when host
        // does not issue INITIALIZE ELEMENT STATUS before MOVE/READ STATUS.
        bootstrap_changer_slots_from_shared_once(state);
    }
    sync_changer_inventory_from_shared(state);
    sync_changer_mount_from_shared(state);
    match opcode {
        0x01 => rezero_unit_changer(state),
        0x00 => CdbResponse::good(vec![]), // TEST UNIT READY
        0x03 => CdbResponse::good(request_sense_response(state)), // REQUEST SENSE
        0x12 => {
            // INQUIRY / EVPD
            let evpd = cdb.get(1).copied().unwrap_or(0) & 0x01;
            let page_code = cdb.get(2).copied().unwrap_or(0);
            let allocation_length = cdb.get(4).copied().unwrap_or(0) as usize;
            let cmd = if evpd == 0 {
                CoreCommand::InquiryStandard {
                    profile,
                    serial_seed: state.drive_id.clone(),
                }
            } else {
                CoreCommand::InquiryVpd {
                    profile,
                    page_code,
                    serial_seed: state.drive_id.clone(),
                }
            };
            match execute_with_sense(state, cmd) {
                Ok(response) => CdbResponse::good(truncate_reply(
                    core_response_to_bytes(state, response, cdb),
                    allocation_length,
                )),
                Err(sense_frame) => {
                    CdbResponse::check_condition(sense_frame_to_bytes(&sense_frame))
                }
            }
        }
        0x07 => {
            state.reset_changer_inventory(); // INITIALIZE ELEMENT STATUS
            ensure_changer_drive_topology(state);
            sync_changer_inventory_from_shared(state);
            state.changer_slots_synced_from_shared = true;
            state.push_unit_attention(0x28, 0x00);
            sync_changer_mount_from_shared(state);
            CdbResponse::good(vec![])
        }
        0x37 => {
            state.reset_changer_inventory(); // INITIALIZE ELEMENT STATUS WITH RANGE
            ensure_changer_drive_topology(state);
            sync_changer_inventory_from_shared(state);
            state.changer_slots_synced_from_shared = true;
            state.push_unit_attention(0x28, 0x00);
            sync_changer_mount_from_shared(state);
            CdbResponse::good(vec![])
        }
        0xE7 => {
            state.reset_changer_inventory(); // INITIALIZE ELEMENT STATUS WITH RANGEP
            ensure_changer_drive_topology(state);
            sync_changer_inventory_from_shared(state);
            state.changer_slots_synced_from_shared = true;
            state.push_unit_attention(0x28, 0x00);
            sync_changer_mount_from_shared(state);
            CdbResponse::good(vec![])
        }
        0x16 => reserve_release_compat_response(), // RESERVE(6)
        0x17 => reserve_release_compat_response(), // RELEASE(6)
        0x1A => mode_sense_6_changer(state, cdb),  // MODE SENSE(6)
        0x1B => open_close_import_export_changer(state, cdb),
        0x1E => CdbResponse::good(vec![]), // PREVENT/ALLOW MEDIUM REMOVAL
        0x2B => CdbResponse::good(vec![]), // POSITION TO ELEMENT
        0x4D => log_sense_changer(cdb),    // LOG SENSE
        0x5E => persistent_reserve_in_drive(state, cdb), // PERSISTENT RESERVE IN
        0x5F => persistent_reserve_out_drive_with_context(state, cdb, data_out, context), // PERSISTENT RESERVE OUT
        0x5A => mode_sense_10_changer(state, cdb), // MODE SENSE(10)
        0xA0 => report_luns_single_lun(cdb),       // REPORT LUNS
        0xA3 => report_timestamp(cdb),             // REPORT TIMESTAMP
        0xA5 => move_medium_changer(state, cdb),   // MOVE MEDIUM
        0xA6 => exchange_medium_changer(state, cdb), // EXCHANGE MEDIUM
        0xB8 => read_element_status_changer(state, cdb, &profile), // READ ELEMENT STATUS
        _ => changer_unsupported_response(opcode),
    }
}
