use std::{collections::BTreeMap, fs, io, path::PathBuf};

use crate::iscsi::cdb_changer::*;
use crate::iscsi::cdb_server::*;
use crate::iscsi::cdb_wire::CdbResponse;
use crate::scsi_tape::commands_core::{execute_with_sense, CoreCommand, CoreResponse};
use crate::scsi_tape::identity::DeviceIdentityProfile;
use crate::scsi_tape::state::{AllowOverwriteState, TapeState};
use crate::storage::read_logical_block;

const MAX_WRITE_ATTRIBUTE_PARAMETER_LIST_LEN: usize = 64 * 1024;

pub(crate) fn dispatch_drive_discovery_cdb_with_context(
    state: &mut TapeState,
    cdb: &[u8],
    data_out: &[u8],
    profile: &DeviceIdentityProfile,
    context: &CdbDispatchContext,
) -> Option<CdbResponse> {
    let opcode = cdb[0];
    match opcode {
        0x04 => Some(format_medium_drive(state, cdb, profile)),
        0x05 => Some(read_block_limits_drive(cdb)),
        0x0B => Some(CdbResponse::good(vec![])), // SET CAPACITY
        0x13 => Some(verify_6_drive(state, cdb, data_out)),
        0x15 => Some(mode_select_6_drive(state, cdb, data_out, profile)),
        0x16 => Some(reserve_release_compat_response()), // RESERVE(6)
        0x17 => Some(reserve_release_compat_response()), // RELEASE(6)
        0x12 => Some(inquiry_drive(state, cdb, profile.clone())),
        0x1A => Some(mode_sense_6_drive(state, cdb, profile)),
        0x1B => Some(load_unload_drive(state, cdb)),
        0x1C => Some(receive_diagnostic_results_drive(cdb)),
        0x1D => Some(CdbResponse::good(vec![])),
        0x1E => Some(CdbResponse::good(vec![])), // PREVENT/ALLOW MEDIUM REMOVAL
        0x34 => Some(read_position_drive(state, cdb)),
        0x3C => Some(read_buffer_drive(cdb, profile)),
        0x44 => Some(report_density_support_drive(state, cdb, profile)),
        0x4C => Some(log_select_drive(cdb)),
        0x4D => Some(log_sense_drive(state, cdb, profile)),
        0x55 => Some(mode_select_10_drive(state, cdb, data_out, profile)),
        0x56 => Some(reserve_release_compat_response()), // RESERVE(10)
        0x57 => Some(reserve_release_compat_response()), // RELEASE(10)
        0x5E => Some(persistent_reserve_in_drive(state, cdb)),
        0x5F => Some(persistent_reserve_out_drive_with_context(
            state, cdb, data_out, context,
        )),
        0x5A => Some(mode_sense_10_drive(state, cdb, profile)),
        0x82 => Some(allow_overwrite_drive(state, cdb)),
        0xA0 => Some(report_luns_single_lun(cdb)),
        0x8C => Some(read_attribute_drive(state, cdb, profile)),
        0x8D => Some(write_attribute_drive(state, cdb, data_out, profile)),
        0x91 => Some(space_16_drive(state, cdb)),
        0x92 => Some(locate_16_drive(state, cdb)),
        0xAB => Some(read_media_serial_number_drive(state, cdb)),
        _ => None,
    }
}

pub(crate) fn log_select_drive(cdb: &[u8]) -> CdbResponse {
    let sp = (cdb.get(1).copied().unwrap_or(0) & 0x01) != 0;
    let pcr = (cdb.get(1).copied().unwrap_or(0) & 0x02) != 0;
    let Some(parameter_list_length) = try_read_be16(cdb, 7) else {
        return invalid_field_in_cdb_response();
    };
    if sp || (pcr && parameter_list_length != 0) {
        return invalid_field_in_cdb_response();
    }
    CdbResponse::good(vec![])
}

pub(crate) fn load_unload_drive(state: &mut TapeState, cdb: &[u8]) -> CdbResponse {
    let control = cdb.get(4).copied().unwrap_or(0);
    let load = (control & 0x01) != 0;
    let eot = (control & 0x04) != 0;
    let hold = (control & 0x08) != 0;

    if eot {
        return invalid_field_in_cdb_response();
    }

    if load {
        if state.mount_state == crate::scsi_tape::state::MountState::Loaded {
            // Legacy behavior: LOAD on already loaded tape rewinds to BOT.
            state.current_position = 0;
            return CdbResponse::good(vec![]);
        }

        let media_state_key = media_state_key_for_state(state);
        let desired = read_shared_loaded_cartridge(&media_state_key).unwrap_or_default();
        let Some(cartridge) = desired else {
            return CdbResponse::check_condition(build_sense_fixed(0x02, 0x3A, 0x00));
        };
        if let Err(err) = crate::media::mount_bridge::attach_cartridge(state, &cartridge) {
            eprintln!(
                "[cdb_sync] load failed drive_id={} cartridge={} error={err}",
                state.drive_id, cartridge
            );
            return CdbResponse::check_condition(build_sense_fixed(0x02, 0x3A, 0x00));
        }
        apply_shared_cartridge_metadata(state);
        sync_loaded_cartridge_usage_to_shared(state);
        state.push_unit_attention(0x28, 0x00);
        return CdbResponse::good(vec![]);
    }

    if state.mount_state != crate::scsi_tape::state::MountState::Loaded {
        return CdbResponse::good(vec![]);
    }
    if hold {
        // HOLD keeps medium loaded.
        return CdbResponse::good(vec![]);
    }

    sync_loaded_cartridge_usage_to_shared(state);
    crate::media::mount_bridge::detach_cartridge(state);
    state.push_unit_attention(0x28, 0x00);
    CdbResponse::good(vec![])
}

pub(crate) fn inquiry_drive(
    state: &mut TapeState,
    cdb: &[u8],
    profile: DeviceIdentityProfile,
) -> CdbResponse {
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
        Err(sense_frame) => CdbResponse::check_condition(sense_frame_to_bytes(&sense_frame)),
    }
}

pub(crate) fn read_block_limits_drive(cdb: &[u8]) -> CdbResponse {
    let mloi = (cdb.get(1).copied().unwrap_or(0) & 0x01) != 0;
    if mloi {
        let mut out = vec![0u8; 20];
        // Report max logical object ID as 32-bit value in a 64-bit field (per SSC-3 practice).
        out[12..20].copy_from_slice(&0x0000_0000_FFFF_FFFFu64.to_be_bytes());
        return CdbResponse::good(out);
    }

    let mut out = vec![0u8; 6];
    out[0] = 0x00; // granularity
    out[1] = ((DRIVE_REPORTED_MAX_BLOCK_SIZE >> 16) & 0xFF) as u8;
    out[2] = ((DRIVE_REPORTED_MAX_BLOCK_SIZE >> 8) & 0xFF) as u8;
    out[3] = (DRIVE_REPORTED_MAX_BLOCK_SIZE & 0xFF) as u8;
    out[4..6].copy_from_slice(&DRIVE_MIN_BLOCK_SIZE.to_be_bytes());
    CdbResponse::good(out)
}

pub(crate) fn normalized_partition_units(raw_units: u8) -> u8 {
    let units = raw_units & 0x0F;
    if units == 0 {
        return 9;
    }
    if units < 9 {
        return 9;
    }
    units
}

pub(crate) fn partition_unit_bytes(units: u8) -> Option<u64> {
    if units < 9 {
        return None;
    }
    let scale = u32::from(units - 9);
    if scale > 12 {
        return None;
    }
    Some((1024u64 * 1024 * 1024) << scale)
}

pub(crate) fn partition_size_to_units(size_bytes: u64, units: u8) -> u16 {
    let Some(unit_size) = partition_unit_bytes(units) else {
        return 0;
    };
    if size_bytes == 0 {
        return 0;
    }
    let units_rounded = size_bytes.saturating_add(unit_size.saturating_sub(1)) / unit_size;
    units_rounded.min(u16::MAX as u64) as u16
}

pub(crate) fn ensure_partition_runtime_initialized(
    state: &mut TapeState,
    profile: &DeviceIdentityProfile,
) {
    if state.partition_runtime.partition_sizes_bytes[0] != 0 {
        return;
    }
    let capacity = medium_capacity_bytes_for_state(state, profile);
    state.partition_runtime.partition_units = 9;
    state.partition_runtime.addl_partitions_defined = 0;
    state.partition_runtime.fdp = 0x9C;
    state.partition_runtime.medium_fmt_recognition = 0x03;
    state.partition_runtime.partition_sizes_bytes = [0; 4];
    state.partition_runtime.partition_size_units = [0; 4];
    state.partition_runtime.partition_sizes_bytes[0] = capacity;
    state.partition_runtime.partition_size_units[0] =
        partition_size_to_units(capacity, state.partition_runtime.partition_units);
    state.partition_runtime.active_partition = 0;
}

pub(crate) fn medium_capacity_bytes_for_state(
    state: &TapeState,
    profile: &DeviceIdentityProfile,
) -> u64 {
    state
        .cartridge_capacity_bytes
        .filter(|capacity| *capacity > 0)
        .unwrap_or_else(|| native_capacity_bytes_for_profile(profile))
}

pub(crate) fn has_custom_capacity_override(
    state: &TapeState,
    profile: &DeviceIdentityProfile,
) -> bool {
    state
        .cartridge_capacity_bytes
        .filter(|capacity| *capacity > 0)
        .is_some_and(|capacity| capacity != native_capacity_bytes_for_profile(profile))
}

pub(crate) fn apply_medium_capacity_override(state: &mut TapeState, capacity_bytes: u64) {
    if capacity_bytes == 0 {
        return;
    }
    state.cartridge_capacity_bytes = Some(capacity_bytes);
    if state.partition_runtime.addl_partitions_defined == 0 {
        state.partition_runtime.partition_units = 9;
        state.partition_runtime.addl_partitions_defined = 0;
        state.partition_runtime.fdp = 0x9C;
        state.partition_runtime.medium_fmt_recognition = 0x03;
        state.partition_runtime.partition_sizes_bytes = [0; 4];
        state.partition_runtime.partition_size_units = [0; 4];
        state.partition_runtime.partition_sizes_bytes[0] = capacity_bytes;
        state.partition_runtime.partition_size_units[0] =
            partition_size_to_units(capacity_bytes, state.partition_runtime.partition_units);
        state.partition_runtime.active_partition = 0;
    }
}

pub(crate) fn active_partition_capacity_bytes(
    state: &TapeState,
    profile: &DeviceIdentityProfile,
) -> u64 {
    let medium_capacity = medium_capacity_bytes_for_state(state, profile);
    let partition_count = usize::from(state.partition_runtime.addl_partitions_defined)
        .saturating_add(1)
        .clamp(1, 4);
    let idx = usize::from(state.partition_runtime.active_partition).min(partition_count - 1);
    let configured = state.partition_runtime.partition_sizes_bytes[idx];
    if configured == 0 {
        medium_capacity
    } else {
        configured
    }
}

pub(crate) fn parse_partition_sizes(
    page: &[u8],
    addl_partitions_defined: u8,
) -> Result<[u16; 4], ()> {
    let count = usize::from(addl_partitions_defined).saturating_add(1);
    if count == 0 || count > 4 {
        return Err(());
    }
    let needed = 8usize.saturating_add(count * 2);
    if page.len() < needed {
        return Err(());
    }
    let mut sizes = [0u16; 4];
    for (idx, size) in sizes.iter_mut().enumerate().take(count) {
        let offset = 8 + (idx * 2);
        *size = u16::from_be_bytes([page[offset], page[offset + 1]]);
    }
    Ok(sizes)
}

pub(crate) fn validate_partition_definition(
    capacity: u64,
    profile: &DeviceIdentityProfile,
    partition_units: u8,
    addl_partitions_defined: u8,
    partition_sizes: &[u16; 4],
) -> Result<(), ()> {
    let max_addl = max_additional_partitions_for_profile(profile);
    if addl_partitions_defined > max_addl {
        return Err(());
    }
    if partition_units != 0 && partition_units < 9 {
        return Err(());
    }
    if partition_units == 0 && addl_partitions_defined > 1 {
        return Err(());
    }

    if partition_units == 0 {
        return Ok(());
    }

    let unit_size = partition_unit_bytes(partition_units).ok_or(())?;
    let count = usize::from(addl_partitions_defined).saturating_add(1);

    let mut fill_to_max_seen = false;
    let mut total = 0u64;
    for (idx, raw) in partition_sizes.iter().copied().take(count).enumerate() {
        if raw == u16::MAX {
            if fill_to_max_seen || idx > 1 {
                return Err(());
            }
            fill_to_max_seen = true;
            continue;
        }
        total = total.saturating_add(unit_size.saturating_mul(raw as u64));
    }

    if fill_to_max_seen {
        if total >= capacity {
            return Err(());
        }
    } else if total > capacity {
        return Err(());
    }
    Ok(())
}

pub(crate) fn apply_medium_partition_mode_page(
    state: &mut TapeState,
    profile: &DeviceIdentityProfile,
    page: &[u8],
) -> Result<(), ()> {
    if page.len() < 8 {
        return Err(());
    }
    let addl_partitions_defined = page[3];
    let fdp = page[4];
    let medium_fmt_recognition = page[5];
    let partition_units = page[6] & 0x0F;
    let partition_sizes = parse_partition_sizes(page, addl_partitions_defined)?;

    if ((fdp >> 3) & 0x03) != 0x03 {
        return Err(());
    }
    validate_partition_definition(
        medium_capacity_bytes_for_state(state, profile),
        profile,
        partition_units,
        addl_partitions_defined,
        &partition_sizes,
    )?;

    state.partition_runtime.partition_units = if partition_units == 0 {
        0
    } else {
        normalized_partition_units(partition_units)
    };
    state.partition_runtime.addl_partitions_defined = addl_partitions_defined;
    state.partition_runtime.fdp = fdp;
    state.partition_runtime.medium_fmt_recognition = medium_fmt_recognition;
    state.partition_runtime.partition_size_units = partition_sizes;
    Ok(())
}

pub(crate) fn apply_format_medium_layout(
    state: &mut TapeState,
    profile: &DeviceIdentityProfile,
    format: u8,
) -> Result<(), ()> {
    ensure_partition_runtime_initialized(state, profile);
    let capacity = medium_capacity_bytes_for_state(state, profile);
    let max_addl = max_additional_partitions_for_profile(profile);

    if format == 0 {
        state.partition_runtime.partition_units = 9;
        state.partition_runtime.addl_partitions_defined = 0;
        state.partition_runtime.partition_size_units = [0; 4];
        state.partition_runtime.partition_sizes_bytes = [0; 4];
        state.partition_runtime.partition_sizes_bytes[0] = capacity;
        state.partition_runtime.partition_size_units[0] =
            partition_size_to_units(capacity, state.partition_runtime.partition_units);
        return Ok(());
    }

    let partition_units = state.partition_runtime.partition_units & 0x0F;
    let mut addl_partitions_defined = state.partition_runtime.addl_partitions_defined;
    let partition_sizes = state.partition_runtime.partition_size_units;

    validate_partition_definition(
        capacity,
        profile,
        partition_units,
        addl_partitions_defined,
        &partition_sizes,
    )?;
    if addl_partitions_defined > max_addl {
        return Err(());
    }

    let partition_mode = (state.partition_runtime.fdp >> 5) & 0x07;
    if partition_mode == 0 {
        return Err(());
    }

    let mut partition_sizes_bytes = [0u64; 4];
    let fixed_data_partitions = ((state.partition_runtime.fdp >> 7) & 0x01) != 0;
    if fixed_data_partitions {
        addl_partitions_defined = 1;
        partition_sizes_bytes[0] = capacity.saturating_mul(95) / 100;
        partition_sizes_bytes[1] = capacity.saturating_sub(partition_sizes_bytes[0]);
    } else {
        let count = usize::from(addl_partitions_defined).saturating_add(1);
        if partition_units == 0 {
            if count == 1 {
                partition_sizes_bytes[0] = capacity;
            } else {
                let mut p0 = partition_sizes[0] as u64;
                if p0 == 0 || p0 > 100 {
                    p0 = 50;
                }
                partition_sizes_bytes[0] = (capacity.saturating_mul(p0)) / 100;
                partition_sizes_bytes[1] = capacity.saturating_sub(partition_sizes_bytes[0]);
            }
        } else {
            let unit_size = partition_unit_bytes(partition_units).ok_or(())?;
            let mut fill_to_max_idx = None;
            let mut total = 0u64;
            for (idx, raw) in partition_sizes.iter().copied().take(count).enumerate() {
                if raw == u16::MAX {
                    fill_to_max_idx = Some(idx);
                    continue;
                }
                let size = unit_size.saturating_mul(raw as u64);
                partition_sizes_bytes[idx] = size;
                total = total.saturating_add(size);
            }
            if let Some(fill_idx) = fill_to_max_idx {
                if total >= capacity {
                    return Err(());
                }
                partition_sizes_bytes[fill_idx] = capacity - total;
                total = capacity;
            }
            if total > capacity {
                return Err(());
            }
            if total < capacity {
                partition_sizes_bytes[0] =
                    partition_sizes_bytes[0].saturating_add(capacity - total);
            }
        }
    }

    let units = normalized_partition_units(partition_units);
    state.partition_runtime.partition_units = units;
    state.partition_runtime.addl_partitions_defined = addl_partitions_defined;
    state.partition_runtime.partition_sizes_bytes = partition_sizes_bytes;
    state.partition_runtime.partition_size_units = [0; 4];
    let count = usize::from(addl_partitions_defined)
        .saturating_add(1)
        .min(4);
    for (idx, size_bytes) in partition_sizes_bytes.iter().enumerate().take(count) {
        state.partition_runtime.partition_size_units[idx] =
            partition_size_to_units(*size_bytes, units);
    }
    Ok(())
}

pub(crate) fn parameter_value_invalid_response() -> CdbResponse {
    CdbResponse::check_condition(build_sense_fixed(0x05, 0x26, 0x00))
}

pub(crate) fn position_past_beginning_response() -> CdbResponse {
    CdbResponse::check_condition(build_sense_fixed(0x05, 0x3B, 0x0C))
}

pub(crate) fn format_medium_drive(
    state: &mut TapeState,
    cdb: &[u8],
    profile: &DeviceIdentityProfile,
) -> CdbResponse {
    if state.mount_state != crate::scsi_tape::state::MountState::Loaded {
        return CdbResponse::check_condition(build_sense_fixed(0x02, 0x3A, 0x00));
    }
    if state.current_position != 0 {
        return position_past_beginning_response();
    }
    let format = cdb.get(2).copied().unwrap_or(0) & 0x0F;
    if apply_format_medium_layout(state, profile, format).is_err() {
        return parameter_value_invalid_response();
    }

    // FORMAT MEDIUM resets medium contents and runtime position to BOT.
    state.current_position = 0;
    state.eod_position = 0;
    state.filemarks.clear();
    state.block_starts.clear();
    state.block_lengths.clear();
    state.partition_runtime.active_partition = 0;
    state.push_unit_attention(0x2A, 0x01);
    CdbResponse::good(vec![])
}

pub(crate) fn mode_select_6_drive(
    state: &mut TapeState,
    cdb: &[u8],
    data_out: &[u8],
    profile: &DeviceIdentityProfile,
) -> CdbResponse {
    let parameter_list_length = cdb.get(4).copied().unwrap_or(0) as usize;
    if parameter_list_length > 0 && data_out.len() < parameter_list_length {
        return invalid_field_in_cdb_response();
    }
    if parameter_list_length > 0
        && apply_mode_select_payload_drive(
            state,
            &data_out[..parameter_list_length],
            ModeSelectKind::Mode6,
            profile,
        )
        .is_err()
    {
        return invalid_field_in_cdb_response();
    }
    CdbResponse::good(vec![])
}

pub(crate) fn mode_select_10_drive(
    state: &mut TapeState,
    cdb: &[u8],
    data_out: &[u8],
    profile: &DeviceIdentityProfile,
) -> CdbResponse {
    let Some(parameter_list_length) = try_read_be16(cdb, 7) else {
        return invalid_field_in_cdb_response();
    };
    if parameter_list_length > 0 && data_out.len() < parameter_list_length {
        return invalid_field_in_cdb_response();
    }
    if parameter_list_length > 0
        && apply_mode_select_payload_drive(
            state,
            &data_out[..parameter_list_length],
            ModeSelectKind::Mode10,
            profile,
        )
        .is_err()
    {
        return invalid_field_in_cdb_response();
    }
    CdbResponse::good(vec![])
}

pub(crate) enum ModeSelectKind {
    Mode6,
    Mode10,
}

pub(crate) fn apply_mode_select_payload_drive(
    state: &mut TapeState,
    payload: &[u8],
    kind: ModeSelectKind,
    profile: &DeviceIdentityProfile,
) -> Result<(), ()> {
    let (device_specific_parameter, block_descriptor_length, mut offset) = match kind {
        ModeSelectKind::Mode6 => {
            if payload.len() < 4 {
                return Err(());
            }
            (payload[2], payload[3] as usize, 4usize)
        }
        ModeSelectKind::Mode10 => {
            if payload.len() < 8 {
                return Err(());
            }
            (payload[3], try_read_be16(payload, 6).ok_or(())?, 8usize)
        }
    };

    // Honor buffered mode from mode-header device specific parameter.
    state.buffered_mode = ((device_specific_parameter >> 4) & 0x07) != 0;

    if block_descriptor_length > 0 {
        if payload.len() < offset + block_descriptor_length || block_descriptor_length < 8 {
            return Err(());
        }
        let descriptor = &payload[offset..offset + block_descriptor_length];
        let block_length = try_read_be24(descriptor, 5).ok_or(())? as u32;
        if block_length > DRIVE_MAX_BLOCK_SIZE {
            return Err(());
        }
        if block_length == 0 {
            state.block_mode.mode = crate::scsi_tape::state::BlockMode::Variable;
            state.block_mode.fixed_block_size = 0;
        } else {
            state.block_mode.mode = crate::scsi_tape::state::BlockMode::Fixed;
            state.block_mode.fixed_block_size = block_length;
        }
        offset += block_descriptor_length;
    }

    let mut cursor = offset;
    while payload.len() > cursor + 1 {
        let page = payload[cursor];
        let page_code = page & 0x3F;
        let sub_page = page & 0x40 != 0;
        let page_len = if sub_page {
            if payload.len() < cursor + 4 {
                break;
            }
            4 + try_read_be16(payload, cursor + 2).ok_or(())?
        } else {
            2 + payload[cursor + 1] as usize
        };
        if payload.len() < cursor + page_len {
            break;
        }

        // Keep parser behavior aligned with legacy: consume known pages and ignore others.
        if page_code == MODE_PAGE_DEVICE_CONFIGURATION && !sub_page && page_len >= 16 {
            // SEW (Synchronize at Early Warning)
            let sew = (payload[cursor + 10] & 0x08) != 0;
            if sew {
                state.early_warning_window = 8;
            }
            // Byte 14 is inside the device configuration page only when page_len >= 15;
            // keep the guard at 16 to match the legacy page shape we accept here. This
            // field is reflected in MODE SENSE for initiator compatibility and does
            // not select the write-path codec.
            state.select_data_compression_algorithm = payload[cursor + 14] != 0;
        } else if page_code == MODE_PAGE_DATA_COMPRESSION && !sub_page && page_len >= 4 {
            // QuadStor treats only DCE as mutable here and preserves DCC capability.
            state.data_compression_enabled =
                state.data_compression_allowed && (payload[cursor + 2] & 0x80) != 0;
        } else if page_code == MODE_PAGE_MEDIUM_PARTITION && !sub_page && page_len >= 8 {
            apply_medium_partition_mode_page(state, profile, &payload[cursor..cursor + page_len])?;
        }

        cursor += page_len;
    }
    Ok(())
}

pub(crate) fn receive_diagnostic_results_drive(cdb: &[u8]) -> CdbResponse {
    let page_code = cdb.get(2).copied().unwrap_or(0);
    let allocation_length = read_be16(cdb, 3);
    if allocation_length == 0 {
        return CdbResponse::good(vec![]);
    }
    let out = vec![page_code, 0x00, 0x00, 0x00];
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn verify_6_drive(state: &mut TapeState, cdb: &[u8], data_out: &[u8]) -> CdbResponse {
    if state.mount_state != crate::scsi_tape::state::MountState::Loaded {
        return CdbResponse::check_condition(build_sense_fixed(0x02, 0x3A, 0x00));
    }
    if cdb.len() < 6 {
        return invalid_field_in_cdb_response();
    }
    let flags = cdb.get(1).copied().unwrap_or(0);
    const VERIFY_FIXED: u8 = 0x01;
    const VERIFY_VBF: u8 = 0x02;
    const VERIFY_BYTCMP: u8 = 0x04;
    if flags & !(VERIFY_FIXED | VERIFY_VBF | VERIFY_BYTCMP) != 0 {
        return invalid_field_in_cdb_response();
    }
    let fixed = (flags & VERIFY_FIXED) != 0;
    let vbf = (flags & VERIFY_VBF) != 0;
    let byte_compare = (flags & VERIFY_BYTCMP) != 0;
    if vbf && byte_compare {
        return invalid_field_in_cdb_response();
    }

    let transfer_len = read_be24(cdb, 2);
    if transfer_len == 0 {
        return CdbResponse::good(vec![]);
    }

    if vbf {
        return match execute_with_sense(
            state,
            CoreCommand::SpaceFilemarks {
                count: transfer_len as i64,
            },
        ) {
            Ok(_) => CdbResponse::good(vec![]),
            Err(sense_frame) => CdbResponse::check_condition(sense_frame_to_bytes(&sense_frame)),
        };
    }

    let block_count = if fixed { transfer_len } else { 1 };
    match verify_blocks_at_position(state, block_count, data_out, byte_compare) {
        Ok(()) => CdbResponse::good(vec![]),
        Err(response) => response,
    }
}

fn verify_blocks_at_position(
    state: &mut TapeState,
    block_count: usize,
    data_out: &[u8],
    byte_compare: bool,
) -> Result<(), CdbResponse> {
    let mut compare_offset = 0usize;
    for _ in 0..block_count {
        let position = state.current_position;
        let read = if let Some(layout) = state.active_layout.as_ref() {
            read_logical_block(layout, position)
                .map_err(|_| CdbResponse::check_condition(build_sense_fixed(0x03, 0x11, 0x00)))?
                .ok_or_else(|| CdbResponse::check_condition(build_sense_fixed(0x03, 0x11, 0x00)))?
        } else {
            let logical_len =
                state.block_lengths.get(&position).copied().ok_or_else(|| {
                    CdbResponse::check_condition(build_sense_fixed(0x03, 0x11, 0x00))
                })?;
            crate::storage::LogicalReadResult {
                record_id: 0,
                logical_start: position,
                logical_len,
                dedup_entry_id: 0,
                codec_used: crate::storage::CompressionCodec::None,
                payload: Vec::new(),
            }
        };
        if read.logical_start != position {
            return Err(CdbResponse::check_condition(build_sense_fixed(
                0x03, 0x11, 0x00,
            )));
        }
        if byte_compare {
            let len = read.logical_len as usize;
            if data_out.len().saturating_sub(compare_offset) < len {
                return Err(invalid_field_in_cdb_response());
            }
            let expected = &data_out[compare_offset..compare_offset + len];
            if read.payload != expected {
                return Err(CdbResponse::check_condition(build_sense_fixed(
                    0x0E, 0x1D, 0x00,
                )));
            }
            compare_offset += len;
        }
        state.current_position = state
            .current_position
            .saturating_add(read.logical_len as u64);
        state
            .record_read_usage(read.logical_len as u64)
            .map_err(|_| CdbResponse::check_condition(build_sense_fixed(0x03, 0x44, 0x00)))?;
    }
    if byte_compare && compare_offset != data_out.len() {
        return Err(invalid_field_in_cdb_response());
    }
    Ok(())
}

pub(crate) fn allow_overwrite_drive(state: &mut TapeState, cdb: &[u8]) -> CdbResponse {
    if state.mount_state != crate::scsi_tape::state::MountState::Loaded {
        return CdbResponse::check_condition(build_sense_fixed(0x02, 0x3A, 0x00));
    }
    if cdb.len() < 16 {
        return invalid_field_in_cdb_response();
    }
    let allow_overwrite = cdb.get(2).copied().unwrap_or(0) & 0x0F;
    if allow_overwrite > 0x02 {
        return invalid_field_in_cdb_response();
    }
    let partition = cdb.get(3).copied().unwrap_or(0);
    if partition != 0 {
        return invalid_field_in_cdb_response();
    }
    let block_address = u64::from_be_bytes([
        cdb[4], cdb[5], cdb[6], cdb[7], cdb[8], cdb[9], cdb[10], cdb[11],
    ]);
    state.allow_overwrite = if allow_overwrite == 0 {
        None
    } else {
        Some(AllowOverwriteState {
            allow_type: allow_overwrite,
            partition,
            block_address,
        })
    };
    CdbResponse::good(vec![])
}

pub(crate) fn space_16_drive(state: &mut TapeState, cdb: &[u8]) -> CdbResponse {
    if cdb.len() < 16 {
        return invalid_field_in_cdb_response();
    }
    let code = cdb[1] & 0x0F;
    let count = i64::from_be_bytes([
        cdb[4], cdb[5], cdb[6], cdb[7], cdb[8], cdb[9], cdb[10], cdb[11],
    ]);
    let command = match code {
        0x00 => CoreCommand::SpaceBlocks { count },
        0x01 => CoreCommand::SpaceFilemarks { count },
        0x03 => CoreCommand::SpaceEndOfData { count },
        _ => return invalid_field_in_cdb_response(),
    };
    match execute_with_sense(state, command) {
        Ok(_) => CdbResponse::good(vec![]),
        Err(sense_frame) => CdbResponse::check_condition(sense_frame_to_bytes(&sense_frame)),
    }
}

pub(crate) fn read_media_serial_number_drive(state: &TapeState, cdb: &[u8]) -> CdbResponse {
    if state.mount_state != crate::scsi_tape::state::MountState::Loaded {
        return CdbResponse::check_condition(build_sense_fixed(0x02, 0x3A, 0x00));
    }
    let allocation_length = read_be32(cdb, 6);
    if allocation_length == 0 {
        return CdbResponse::good(vec![]);
    }
    let serial = media_serial_for_state(state);
    let mut out = Vec::with_capacity(4 + serial.len());
    out.extend_from_slice(&(serial.len() as u32).to_be_bytes());
    out.extend_from_slice(serial.as_bytes());
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn media_serial_for_state(state: &TapeState) -> String {
    let seed = state
        .cartridge_id
        .as_deref()
        .filter(|value| !value.trim().is_empty())
        .unwrap_or(&state.drive_id);
    seed.chars()
        .filter(|ch| ch.is_ascii_alphanumeric())
        .take(32)
        .collect::<String>()
}

pub(crate) fn read_buffer_drive(cdb: &[u8], profile: &DeviceIdentityProfile) -> CdbResponse {
    // READ BUFFER (6): byte1 mode, byte2 buffer-id
    let mode = cdb.get(1).copied().unwrap_or(0) & 0x1F;
    let allocation_length = read_be24(cdb, 6);
    if allocation_length == 0 {
        return CdbResponse::good(vec![]);
    }
    let buffer_id = cdb.get(2).copied().unwrap_or(0);
    let mut out = vec![0u8; 4];
    match mode {
        0x03 => {
            // Descriptor mode.
            let capacity = if buffer_id == 0x03 { 64u32 } else { 0u32 };
            out[1] = ((capacity >> 16) & 0xFF) as u8;
            out[2] = ((capacity >> 8) & 0xFF) as u8;
            out[3] = (capacity & 0xFF) as u8;
        }
        0x00 | 0x02 if buffer_id == 0x03 => {
            // Header+data or data mode.
            let serial = profile
                .serial_for_vpd_seed("holo")
                .unwrap_or_else(|_| "HOLO00000000".to_string());
            let mut payload = vec![b' '; 4 + serial.len()];
            let serial_bytes = serial.as_bytes();
            let copy_len = usize::min(serial_bytes.len(), payload.len().saturating_sub(4));
            if copy_len > 0 {
                payload[4..4 + copy_len].copy_from_slice(&serial_bytes[..copy_len]);
            }
            let cap = payload.len().saturating_sub(1) as u32;
            out[0..4].copy_from_slice(&cap.to_be_bytes());
            out.extend_from_slice(&payload);
        }
        _ => {}
    }
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn read_position_drive(state: &mut TapeState, cdb: &[u8]) -> CdbResponse {
    let service_action = cdb.get(1).copied().unwrap_or(0) & 0x1F;
    if !matches!(
        service_action,
        READ_POSITION_SERVICE_ACTION_SHORT
            | READ_POSITION_SERVICE_ACTION_LONG
            | READ_POSITION_SERVICE_ACTION_EXTENDED
    ) {
        return invalid_field_in_cdb_response();
    }

    match execute_with_sense(state, CoreCommand::ReadPosition) {
        Ok(CoreResponse::Position(report)) => CdbResponse::good(read_position_response_bytes(
            state,
            cdb,
            &report,
            state.partition_runtime.active_partition,
        )),
        Ok(_) => CdbResponse::good(vec![]),
        Err(sense_frame) => CdbResponse::check_condition(sense_frame_to_bytes(&sense_frame)),
    }
}

pub(crate) fn persistent_reserve_in_drive(state: &TapeState, cdb: &[u8]) -> CdbResponse {
    let service_action = cdb.get(1).copied().unwrap_or(0) & 0x1F;
    let allocation_length = read_be16(cdb, 7);
    if allocation_length == 0 {
        return CdbResponse::good(vec![]);
    }
    let snapshot = crate::scsi_tape::reservation::snapshot(state);
    let out = match service_action {
        0x00 => persistent_reserve_read_keys(&snapshot),
        0x01 => persistent_reserve_read_reservation(&snapshot),
        0x02 => vec![0x00, 0x00, 0x00, 0x08, 0x00, 0x80, 0x00, 0x00], // READ CAPABILITIES baseline
        0x03 => persistent_reserve_read_full_status(&snapshot),
        _ => return invalid_field_in_cdb_response(),
    };
    CdbResponse::good(truncate_reply(out, allocation_length))
}

fn persistent_reserve_read_keys(
    snapshot: &crate::scsi_tape::reservation::ReservationSnapshot,
) -> Vec<u8> {
    let mut out = Vec::with_capacity(8 + snapshot.registrations.len() * 8);
    out.extend_from_slice(&snapshot.generation.to_be_bytes());
    out.extend_from_slice(&((snapshot.registrations.len() * 8) as u32).to_be_bytes());
    for (_initiator, key) in &snapshot.registrations {
        out.extend_from_slice(&key.to_be_bytes());
    }
    out
}

fn persistent_reserve_read_reservation(
    snapshot: &crate::scsi_tape::reservation::ReservationSnapshot,
) -> Vec<u8> {
    let mut out = Vec::with_capacity(24);
    out.extend_from_slice(&snapshot.generation.to_be_bytes());
    let additional_len = if snapshot.active_key.is_some() {
        16u32
    } else {
        0u32
    };
    out.extend_from_slice(&additional_len.to_be_bytes());
    if let Some(key) = snapshot.active_key {
        out.extend_from_slice(&key.to_be_bytes());
        out.extend_from_slice(&[0x00, 0x00, 0x00, 0x00]);
        out.push(0x00); // LU_SCOPE
        out.push(0x03); // Exclusive Access Registrants Only baseline
        out.extend_from_slice(&[0x00, 0x00]);
    }
    out
}

fn persistent_reserve_read_full_status(
    snapshot: &crate::scsi_tape::reservation::ReservationSnapshot,
) -> Vec<u8> {
    let mut out = Vec::with_capacity(8 + snapshot.registrations.len() * 24);
    out.extend_from_slice(&snapshot.generation.to_be_bytes());
    out.extend_from_slice(&((snapshot.registrations.len() * 24) as u32).to_be_bytes());
    for (initiator, key) in &snapshot.registrations {
        out.extend_from_slice(&key.to_be_bytes());
        out.extend_from_slice(&[0x00, 0x00, 0x00, 0x00]);
        out.push(0x00);
        out.push(
            if snapshot.active_owner.as_deref() == Some(initiator.as_str()) {
                0x03
            } else {
                0x00
            },
        );
        out.extend_from_slice(&[0x00, 0x08]);
        let mut nexus = [0u8; 8];
        for (idx, byte) in initiator.as_bytes().iter().take(8).enumerate() {
            nexus[idx] = *byte;
        }
        out.extend_from_slice(&nexus);
    }
    out
}

pub(crate) fn persistent_reserve_out_drive_with_context(
    state: &mut TapeState,
    cdb: &[u8],
    data_out: &[u8],
    context: &CdbDispatchContext,
) -> CdbResponse {
    let service_action = cdb.get(1).copied().unwrap_or(0) & 0x1F;
    let Some(parameter_list_length) = try_read_be16(cdb, 7) else {
        return invalid_field_in_cdb_response();
    };
    if parameter_list_length > 0 && data_out.len() < parameter_list_length {
        return invalid_field_in_cdb_response();
    }

    let params = if parameter_list_length == 0 {
        &[][..]
    } else {
        &data_out[..parameter_list_length]
    };
    let reservation_key = read_be64(params, 0).unwrap_or(0);
    let service_key = read_be64(params, 8).unwrap_or(0);
    let initiator = context.initiator_or_default().to_string();

    let command = match service_action {
        0x00 => {
            let register_key = if service_key != 0 {
                service_key
            } else {
                reservation_key
            };
            CoreCommand::ReservationRegister {
                initiator,
                key: register_key,
            }
        }
        0x01 => CoreCommand::ReservationReserve {
            initiator,
            key: reservation_key,
        },
        0x02 => CoreCommand::ReservationRelease {
            initiator,
            key: reservation_key,
        },
        0x03 => CoreCommand::ReservationClear {
            initiator,
            key: reservation_key,
        },
        0x04 | 0x05 => CoreCommand::ReservationPreempt {
            initiator,
            key: reservation_key,
            service_key,
        },
        0x06 => CoreCommand::ReservationRegisterIgnore {
            initiator,
            service_key,
        },
        0x07 => {
            let Some(target_initiator) = parse_register_and_move_target_initiator(params) else {
                return invalid_field_in_parameter_list_response();
            };
            CoreCommand::ReservationRegisterMove {
                initiator,
                key: reservation_key,
                service_key,
                target_initiator,
                unregister_source: register_and_move_unregister_source(params),
            }
        }
        _ => return invalid_field_in_cdb_response(),
    };

    match execute_with_sense(state, command) {
        Ok(_) => CdbResponse::good(vec![]),
        Err(sense_frame) => CdbResponse::check_condition(sense_frame_to_bytes(&sense_frame)),
    }
}

fn register_and_move_unregister_source(params: &[u8]) -> bool {
    params.get(20).map(|byte| byte & 0x01 != 0).unwrap_or(false)
}

fn parse_register_and_move_target_initiator(params: &[u8]) -> Option<String> {
    let transport_ids = params.get(24..)?;
    for prefix in [b"iqn.".as_slice(), b"eui.".as_slice(), b"naa.".as_slice()] {
        if let Some(offset) = transport_ids
            .windows(prefix.len())
            .position(|window| window.eq_ignore_ascii_case(prefix))
        {
            let candidate = &transport_ids[offset..];
            let end = candidate
                .iter()
                .position(|byte| *byte == 0 || *byte == b' ' || *byte == b'\n' || *byte == b'\r')
                .unwrap_or(candidate.len());
            if end == 0 {
                continue;
            }
            let value = std::str::from_utf8(&candidate[..end]).ok()?.trim();
            if !value.is_empty()
                && value
                    .bytes()
                    .all(|byte| byte.is_ascii_graphic() && byte != b'\\')
            {
                return Some(value.to_string());
            }
        }
    }
    None
}

pub(crate) fn write_attribute_drive(
    state: &mut TapeState,
    cdb: &[u8],
    data_out: &[u8],
    profile: &DeviceIdentityProfile,
) -> CdbResponse {
    if cdb.len() < 14 {
        return invalid_field_in_cdb_response();
    }
    let raw_parameter_list_length = u32::from_be_bytes([cdb[10], cdb[11], cdb[12], cdb[13]]);
    let Ok(parameter_list_length) = usize::try_from(raw_parameter_list_length) else {
        return invalid_field_in_cdb_response();
    };
    if parameter_list_length == 0 {
        return CdbResponse::good(vec![]);
    }
    if parameter_list_length > MAX_WRITE_ATTRIBUTE_PARAMETER_LIST_LEN {
        return invalid_field_in_cdb_response();
    }
    if data_out.len() < parameter_list_length {
        return invalid_field_in_cdb_response();
    }

    ensure_partition_runtime_initialized(state, profile);
    let partition_count = usize::from(state.partition_runtime.addl_partitions_defined)
        .saturating_add(1)
        .clamp(1, 4);
    let partition_id = cdb[7];
    if usize::from(partition_id) >= partition_count {
        return invalid_field_in_cdb_response();
    }
    if ensure_mam_overrides_loaded(state).is_err() {
        return CdbResponse::check_condition(build_sense_fixed(0x03, 0x44, 0x00));
    }

    let writable_map = drive_mam_attribute_map(state, profile);
    let payload = &data_out[..parameter_list_length];
    let mut cursor = 0usize;
    if payload.len() >= 4 {
        let declared_len =
            u32::from_be_bytes([payload[0], payload[1], payload[2], payload[3]]) as usize;
        if declared_len == payload.len().saturating_sub(4) {
            cursor = 4;
        }
    }
    while payload.len().saturating_sub(cursor) >= 5 {
        let id = u16::from_be_bytes([payload[cursor], payload[cursor + 1]]);
        let format = payload[cursor + 2];
        let value_len = u16::from_be_bytes([payload[cursor + 3], payload[cursor + 4]]) as usize;
        cursor += 5;
        if payload.len() < cursor + value_len {
            return invalid_field_in_parameter_list_response();
        }
        let value = &payload[cursor..cursor + value_len];
        cursor += value_len;

        let Some(schema) = writable_map.get(&id) else {
            continue;
        };
        if !schema.writable {
            return invalid_field_in_parameter_list_response();
        }
        if (format & 0x7F) != (schema.format & 0x7F) {
            return invalid_field_in_parameter_list_response();
        }
        if value.len() > schema.max_len {
            return invalid_field_in_parameter_list_response();
        }
        let normalized = normalize_attribute_value(value, schema.max_len, schema.format);
        let key = mam_override_key(partition_id, id);
        state.mam_overrides.insert(
            key,
            crate::scsi_tape::state::MamAttributeOverride {
                format: schema.format,
                value: normalized,
            },
        );
    }

    if cursor != payload.len() {
        return invalid_field_in_parameter_list_response();
    }
    if persist_mam_overrides(state).is_err() {
        return CdbResponse::check_condition(build_sense_fixed(0x03, 0x44, 0x00));
    }
    state.push_unit_attention(0x2A, 0x01);
    CdbResponse::good(vec![])
}

pub(crate) fn locate_16_drive(state: &mut TapeState, cdb: &[u8]) -> CdbResponse {
    if cdb.len() < 16 {
        return invalid_field_in_cdb_response();
    }
    let logical_block = u64::from_be_bytes([
        cdb[4], cdb[5], cdb[6], cdb[7], cdb[8], cdb[9], cdb[10], cdb[11],
    ]);
    let internal_position = transport_logical_to_internal_offset(state, logical_block);
    match execute_with_sense(
        state,
        CoreCommand::Locate {
            logical_block: internal_position,
        },
    ) {
        Ok(_) => CdbResponse::good(vec![]),
        Err(sense_frame) => CdbResponse::check_condition(sense_frame_to_bytes(&sense_frame)),
    }
}

pub(crate) fn mode_sense_6_drive(
    state: &TapeState,
    cdb: &[u8],
    profile: &DeviceIdentityProfile,
) -> CdbResponse {
    let dbd = (cdb.get(1).copied().unwrap_or(0) & 0x08) != 0;
    let page_control = cdb.get(2).copied().unwrap_or(0) >> 6;
    let page_code = cdb.get(2).copied().unwrap_or(0) & 0x3F;
    let sub_page_code = cdb.get(3).copied().unwrap_or(0);
    let allocation_length = cdb.get(4).copied().unwrap_or(0) as usize;
    if page_control == MODE_SENSE_PC_SAVED_VALUES {
        return saving_parameters_not_supported_response();
    }

    let pages = match drive_mode_pages(state, profile, page_code, sub_page_code, page_control) {
        Some(pages) => pages,
        None => return invalid_field_in_cdb_response(),
    };

    let mut out = vec![0u8; 4];
    out[1] = medium_type_for_loaded_media(state, profile);
    out[2] = device_specific_mode_parameter(state);
    if !dbd {
        out[3] = 8;
        out.extend_from_slice(&drive_mode_block_descriptor(state, profile));
    }
    out.extend_from_slice(&pages);
    out[0] = out.len().saturating_sub(1) as u8;
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn mode_sense_10_drive(
    state: &TapeState,
    cdb: &[u8],
    profile: &DeviceIdentityProfile,
) -> CdbResponse {
    let dbd = (cdb.get(1).copied().unwrap_or(0) & 0x08) != 0;
    let page_control = cdb.get(2).copied().unwrap_or(0) >> 6;
    let page_code = cdb.get(2).copied().unwrap_or(0) & 0x3F;
    let sub_page_code = cdb.get(3).copied().unwrap_or(0);
    let allocation_length = read_be16(cdb, 7);
    if page_control == MODE_SENSE_PC_SAVED_VALUES {
        return saving_parameters_not_supported_response();
    }

    let pages = match drive_mode_pages(state, profile, page_code, sub_page_code, page_control) {
        Some(pages) => pages,
        None => return invalid_field_in_cdb_response(),
    };

    let mut out = vec![0u8; 8];
    out[2] = medium_type_for_loaded_media(state, profile);
    out[3] = device_specific_mode_parameter(state);
    if !dbd {
        out[6..8].copy_from_slice(&8u16.to_be_bytes());
        out.extend_from_slice(&drive_mode_block_descriptor(state, profile));
    }
    out.extend_from_slice(&pages);
    let mode_data_len = (out.len().saturating_sub(2)) as u16;
    out[0..2].copy_from_slice(&mode_data_len.to_be_bytes());
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn drive_mode_block_descriptor(
    state: &TapeState,
    profile: &DeviceIdentityProfile,
) -> [u8; 8] {
    let mut descriptor = [0u8; 8];
    descriptor[0] = density_code_for_profile(profile);
    let default_block_len = match state.block_mode.mode {
        crate::scsi_tape::state::BlockMode::Fixed if state.block_mode.fixed_block_size > 0 => {
            state.block_mode.fixed_block_size
        }
        _ => DRIVE_DEFAULT_BLOCK_LENGTH,
    };
    if state.mount_state == crate::scsi_tape::state::MountState::Loaded
        && has_custom_capacity_override(state, profile)
        && default_block_len > 0
    {
        let capacity_bytes = active_partition_capacity_bytes(state, profile);
        let block_len = u64::from(default_block_len);
        let block_count = capacity_bytes
            .saturating_add(block_len.saturating_sub(1))
            .saturating_div(block_len)
            .min(0x00FF_FFFF);
        descriptor[1] = ((block_count >> 16) & 0xFF) as u8;
        descriptor[2] = ((block_count >> 8) & 0xFF) as u8;
        descriptor[3] = (block_count & 0xFF) as u8;
    }
    descriptor[5] = ((default_block_len >> 16) & 0xFF) as u8;
    descriptor[6] = ((default_block_len >> 8) & 0xFF) as u8;
    descriptor[7] = (default_block_len & 0xFF) as u8;
    descriptor
}

pub(crate) fn drive_mode_pages(
    state: &TapeState,
    profile: &DeviceIdentityProfile,
    page_code: u8,
    sub_page_code: u8,
    page_control: u8,
) -> Option<Vec<u8>> {
    if sub_page_code != 0
        && !(page_code == MODE_PAGE_DEVICE_CONFIGURATION
            && sub_page_code == MODE_SUBPAGE_DEVICE_CONFIGURATION_EXTENSION)
        && !(page_code == MODE_PAGE_CONTROL_MODE
            && sub_page_code == MODE_SUBPAGE_CONTROL_DATA_PROTECTION)
    {
        return None;
    }

    let mut pages = Vec::new();
    match (page_code, sub_page_code) {
        (0x00, 0) => pages.extend_from_slice(&[0x00, 0x00]),
        (MODE_PAGE_RW_ERROR_RECOVERY, 0) => {
            pages.extend_from_slice(&drive_mode_page_rw_error_recovery(page_control))
        }
        (MODE_PAGE_DISCONNECT_RECONNECT, 0) => {
            pages.extend_from_slice(&drive_mode_page_disconnect_reconnect(page_control))
        }
        (MODE_PAGE_CACHING, 0) => pages.extend_from_slice(&drive_mode_page_caching(page_control)),
        (MODE_PAGE_CONTROL_MODE, 0) => {
            pages.extend_from_slice(&drive_mode_page_control_mode(page_control))
        }
        (MODE_PAGE_CONTROL_MODE, MODE_SUBPAGE_CONTROL_DATA_PROTECTION) => {
            pages.extend_from_slice(&drive_mode_page_control_data_protection(page_control))
        }
        (MODE_PAGE_DATA_COMPRESSION, 0) => {
            pages.extend_from_slice(&drive_mode_page_data_compression(state, page_control))
        }
        (MODE_PAGE_DEVICE_CONFIGURATION, 0) => {
            pages.extend_from_slice(&drive_mode_page_device_configuration(state, page_control))
        }
        (MODE_PAGE_DEVICE_CONFIGURATION, MODE_SUBPAGE_DEVICE_CONFIGURATION_EXTENSION) => pages
            .extend_from_slice(&drive_mode_page_device_configuration_extension(
                page_control,
            )),
        (MODE_PAGE_MEDIUM_PARTITION, 0) => pages.extend_from_slice(
            &drive_mode_page_medium_partition(state, profile, page_control),
        ),
        (MODE_PAGE_INFORMATION_EXCEPTION, 0) => {
            pages.extend_from_slice(&drive_mode_page_information_exception(page_control))
        }
        (MODE_PAGE_MEDIUM_CONFIGURATION, 0) => {
            pages.extend_from_slice(&drive_mode_page_medium_configuration(state, page_control))
        }
        (MODE_PAGE_ALL, 0) => {
            pages.extend_from_slice(&[0x00, 0x00]);
            pages.extend_from_slice(&drive_mode_page_rw_error_recovery(page_control));
            pages.extend_from_slice(&drive_mode_page_disconnect_reconnect(page_control));
            pages.extend_from_slice(&drive_mode_page_caching(page_control));
            pages.extend_from_slice(&drive_mode_page_control_mode(page_control));
            pages.extend_from_slice(&drive_mode_page_data_compression(state, page_control));
            pages.extend_from_slice(&drive_mode_page_device_configuration(state, page_control));
            pages.extend_from_slice(&drive_mode_page_medium_partition(
                state,
                profile,
                page_control,
            ));
            pages.extend_from_slice(&drive_mode_page_information_exception(page_control));
            pages.extend_from_slice(&drive_mode_page_medium_configuration(state, page_control));
        }
        _ => {
            if sub_page_code == 0 {
                return Some(vec![]);
            }
            return None;
        }
    }
    Some(pages)
}

pub(crate) fn drive_mode_page_information_exception(page_control: u8) -> Vec<u8> {
    let mut page = vec![
        MODE_PAGE_INFORMATION_EXCEPTION,
        0x0A,
        0x08, // PERF: polling this page should not imply degraded performance.
        0x00, // MRIE: no reporting until a real exception source exists.
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
    ];
    if page_control == 0x01 {
        page[2..].fill(0x00);
        page[2] = 0x08;
    }
    page
}

pub(crate) fn drive_mode_page_caching(page_control: u8) -> Vec<u8> {
    let mut page = vec![
        MODE_PAGE_CACHING,
        0x0A,
        0x00, // rcd/mf/wce
        0x00, // demand read retention / write retention
        0x00, // disable prefetch transfer length
        0x00, // minimum prefetch
        0x00,
        0x00, // maximum prefetch
        0x00,
        0x00, // maximum prefetch ceiling
        0x00,
        0x00,
    ];
    if page_control == 0x01 {
        page[2..].fill(0x00);
    }
    page
}

pub(crate) fn drive_mode_page_rw_error_recovery(page_control: u8) -> Vec<u8> {
    let mut page = vec![
        MODE_PAGE_RW_ERROR_RECOVERY,
        0x0A,
        0x80, // keep hardware error recovery capability visible to initiators
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
    ];
    if page_control == 0x01 {
        page[2..].fill(0x00);
    }
    page
}

pub(crate) fn drive_mode_page_disconnect_reconnect(page_control: u8) -> Vec<u8> {
    let mut page = vec![
        MODE_PAGE_DISCONNECT_RECONNECT,
        0x0E,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
        0x00,
    ];
    if page_control == 0x01 {
        page[2..].fill(0x00);
    }
    page
}

pub(crate) fn drive_mode_page_control_mode(page_control: u8) -> Vec<u8> {
    let mut page = vec![
        MODE_PAGE_CONTROL_MODE,
        0x0A,
        0x20, // tst
        0x00, // qalgo
        0x40, // tas
        0x00, // autoload
        0x00,
        0x00, // ready_aer_period
        0x00,
        0x00, // busy_timeout_period
        0x00,
        0x00, // self_test_period
    ];
    if page_control == 0x01 {
        page[2..].fill(0x00);
    }
    page
}

pub(crate) fn drive_mode_page_control_data_protection(page_control: u8) -> Vec<u8> {
    let mut page = vec![
        0x40 | MODE_PAGE_CONTROL_MODE,
        MODE_SUBPAGE_CONTROL_DATA_PROTECTION,
        0x00,
        0x1C,
        0x00, // lbp_method
        0x04, // lbp_info_length
        0x00, // lbp_w
    ];
    page.extend_from_slice(&[0u8; 25]);
    if page_control == 0x01 {
        page[4..].fill(0x00);
    }
    page
}

pub(crate) fn drive_mode_page_data_compression(state: &TapeState, page_control: u8) -> Vec<u8> {
    let dce = if state.data_compression_enabled {
        0x80
    } else {
        0x00
    };
    let dcc = if state.data_compression_allowed {
        0x40
    } else {
        0x00
    };
    let mut page = vec![
        MODE_PAGE_DATA_COMPRESSION,
        0x0E,
        dce | dcc, // DCE + DCC
        0x80,      // decompression enabled
        0x00,
        0x00,
        0x00,
        0xFF, // compression algorithm
        0x00,
        0x00,
        0x00,
        0xFF, // decompression algorithm
        0x00,
        0x00,
        0x00,
        0x00,
    ];
    if page_control == 0x01 {
        page[2..].fill(0x00);
        if state.data_compression_allowed {
            page[2] = 0x80; // DCE bit changeable
        }
    }
    page
}

pub(crate) fn drive_mode_page_device_configuration(state: &TapeState, page_control: u8) -> Vec<u8> {
    let mut page = vec![
        MODE_PAGE_DEVICE_CONFIGURATION,
        0x0E,
        0x00,
        state.partition_runtime.active_partition,
        0x00,
        0x00,
        0x00,
        0x00,
        0x40, // REW: report setmarks/block identifiers capability
        0x00,
        0x08, // SEW (synchronize at early warning)
        0x00,
        0x00,
        0x00,
        u8::from(state.select_data_compression_algorithm),
        0x00,
    ];
    if page_control == 0x01 {
        page[2..].fill(0x00);
        page[10] = 0x08; // SEW bit changeable
        page[14] = 0xFF; // compression algorithm selectable
    }
    page
}

pub(crate) fn drive_mode_page_device_configuration_extension(page_control: u8) -> Vec<u8> {
    let mut page = vec![
        0x40 | MODE_PAGE_DEVICE_CONFIGURATION,
        MODE_SUBPAGE_DEVICE_CONFIGURATION_EXTENSION,
        0x00,
        0x1C,
        0x00,
        0x02, // short erase mode support
        0x00,
        0x00,
        0x00,
    ];
    page.extend_from_slice(&[0u8; 23]);
    if page_control == 0x01 {
        page[4..].fill(0x00);
    }
    page
}

pub(crate) fn drive_mode_page_medium_partition(
    state: &TapeState,
    profile: &DeviceIdentityProfile,
    page_control: u8,
) -> Vec<u8> {
    let runtime = &state.partition_runtime;
    let units = normalized_partition_units(runtime.partition_units);
    let max_addl = max_additional_partitions_for_profile(profile);
    let addl_defined = runtime.addl_partitions_defined.min(max_addl);
    let partition_count = usize::from(addl_defined).saturating_add(1).min(4);
    let mut partition_units = runtime.partition_size_units;
    if partition_count > 0 {
        for (idx, slot) in partition_units.iter_mut().enumerate().take(partition_count) {
            if *slot == 0 && runtime.partition_sizes_bytes[idx] > 0 {
                *slot = partition_size_to_units(runtime.partition_sizes_bytes[idx], units);
            }
        }
    }
    let mut page = vec![
        MODE_PAGE_MEDIUM_PARTITION,
        0x0E,
        max_addl,
        addl_defined,
        runtime.fdp,
        runtime.medium_fmt_recognition,
        units,
        0x00,
        (partition_units[0] >> 8) as u8,
        (partition_units[0] & 0xFF) as u8,
        (partition_units[1] >> 8) as u8,
        (partition_units[1] & 0xFF) as u8,
        (partition_units[2] >> 8) as u8,
        (partition_units[2] & 0xFF) as u8,
        (partition_units[3] >> 8) as u8,
        (partition_units[3] & 0xFF) as u8,
    ];
    if page_control == 0x01 {
        // Changeable-values mask for supported modifiable fields.
        page[2] = 0x00; // max_addl_partitions
        page[3] = 0xFF; // addl_partitions_defined
        page[4] = 0xFC; // fdp
        page[5] = 0x00; // medium_fmt_recognition
        page[6] = 0x0F; // partition_units
        page[7] = 0x00; // reserved
        page[8..].fill(0xFF); // partition_size[]
    }
    page
}

pub(crate) fn drive_mode_page_medium_configuration(state: &TapeState, page_control: u8) -> Vec<u8> {
    let mut page = vec![MODE_PAGE_MEDIUM_CONFIGURATION, 0x1E, 0x00, 0x00, 0x00, 0x00];
    page.extend_from_slice(&[0u8; 26]);
    if state.retention_policy.is_worm_media
        && state.mount_state == crate::scsi_tape::state::MountState::Loaded
    {
        page[2] = 0x01;
        page[4] = 0x01;
        page[5] = 0x02;
    }
    if page_control == 0x01 {
        page[2..].fill(0x00);
    }
    page
}

pub(crate) fn log_sense_drive(
    state: &mut TapeState,
    cdb: &[u8],
    profile: &DeviceIdentityProfile,
) -> CdbResponse {
    let sp = cdb.get(1).copied().unwrap_or(0) & 0x01;
    if sp != 0 {
        return invalid_field_in_cdb_response();
    }

    let page_code = cdb.get(2).copied().unwrap_or(0) & 0x3F;
    let allocation_length = read_be16(cdb, 7);
    state.command_counters.log_sense_ops = state.command_counters.log_sense_ops.saturating_add(1);
    let out = match page_code {
        0x00 => {
            let supported_pages = [0x00u8, 0x02, 0x03, 0x0C, 0x17, 0x2E, 0x31, 0x32, 0x37];
            let mut page = vec![0x00, 0x00, 0x00, supported_pages.len() as u8];
            page.extend_from_slice(&supported_pages);
            page
        }
        0x02 | 0x03 => {
            let mut page = vec![page_code, 0x00, 0x00, 0x00];
            for code in [
                0x0000u16, 0x0001, 0x0002, 0x0003, 0x0004, 0x0005, 0x0006, 0x8000, 0x8001, 0x8002,
                0x8003,
            ] {
                append_log_param_u32(&mut page, code, 0);
            }
            finalize_log_page(&mut page);
            page
        }
        0x0C => {
            let mut page = vec![0x0C, 0x00, 0x00, 0x00];
            append_log_param_u32(
                &mut page,
                0x0001,
                clamp_u64_to_u32(state.usage_counters.last_load_write_ops),
            );
            append_log_param_u32(
                &mut page,
                0x0002,
                clamp_u64_to_u32(state.usage_counters.last_load_filemark_ops),
            );
            append_log_param_u32(
                &mut page,
                0x0003,
                clamp_u64_to_u32(state.usage_counters.last_load_read_ops),
            );
            finalize_log_page(&mut page);
            page
        }
        0x17 => {
            let capacity_bytes = active_partition_capacity_bytes(state, profile);
            let used_bytes = state.eod_position.min(capacity_bytes);
            let remaining_bytes = capacity_bytes.saturating_sub(used_bytes);
            let mut page = vec![0x17, 0x00, 0x00, 0x00];
            append_log_param_u32(
                &mut page,
                0x0001,
                clamp_u64_to_u32(state.usage_counters.load_count.max(1)),
            );
            append_log_param_u64(&mut page, 0x0002, state.usage_counters.lifetime_write_ops);
            append_log_param_u32(&mut page, 0x0003, 0);
            append_log_param_u16(&mut page, 0x0004, 0);
            append_log_param_u64(&mut page, 0x0007, state.usage_counters.lifetime_read_ops);
            append_log_param_u32(&mut page, 0x0008, 0);
            append_log_param_u16(&mut page, 0x0009, 0);
            append_log_param_u16(&mut page, 0x000C, 0);
            append_log_param_u16(&mut page, 0x000D, 0);
            append_log_param_u64(
                &mut page,
                0x0010,
                u64::from(bytes_to_mib_u32(
                    state.usage_counters.lifetime_bytes_written,
                )),
            );
            append_log_param_u64(
                &mut page,
                0x0011,
                u64::from(bytes_to_mib_u32(state.usage_counters.lifetime_bytes_read)),
            );
            append_log_param_u32(&mut page, 0x0101, 0);
            append_log_param_u32(&mut page, 0x0102, 0);
            append_log_partition_param_u32(&mut page, 0x0202, bytes_to_mib_u32(capacity_bytes));
            append_log_partition_param_u32(&mut page, 0x0203, bytes_to_mib_u32(used_bytes));
            append_log_partition_param_u32(&mut page, 0x0204, bytes_to_mib_u32(remaining_bytes));
            finalize_log_page(&mut page);
            page
        }
        0x2E => {
            // TapeAlert page: expose an empty alert set.
            vec![0x2E, 0x00, 0x00, 0x00]
        }
        0x31 => {
            let capacity_bytes = active_partition_capacity_bytes(state, profile);
            let used_bytes = state.eod_position.min(capacity_bytes);
            let remaining_bytes = capacity_bytes.saturating_sub(used_bytes);
            let mut page = vec![0x31, 0x00, 0x00, 0x00];
            append_log_param_u32(&mut page, 0x0001, bytes_to_mib_u32(remaining_bytes));
            append_log_param_u32(&mut page, 0x0002, 0);
            append_log_param_u32(&mut page, 0x0003, bytes_to_mib_u32(capacity_bytes));
            append_log_param_u32(&mut page, 0x0004, 0);
            finalize_log_page(&mut page);
            page
        }
        0x32 => {
            let mut page = vec![0x32, 0x00, 0x00, 0x00];
            append_log_param_u32(
                &mut page,
                0x0001,
                bytes_to_mib_u32(state.usage_counters.lifetime_bytes_written),
            );
            append_log_param_u32(
                &mut page,
                0x0002,
                bytes_to_mib_u32(state.usage_counters.lifetime_bytes_written),
            );
            let page_len = (page.len().saturating_sub(4)) as u16;
            page[2..4].copy_from_slice(&page_len.to_be_bytes());
            page
        }
        0x37 => {
            let native_rate = native_transfer_rate_mib_per_sec_for_profile(profile);
            let mut page = vec![0x37, 0x00, 0x00, 0x00];
            append_log_param_u32(&mut page, 0x0001, native_rate);
            append_log_param_u32(&mut page, 0x0002, native_rate);
            finalize_log_page(&mut page);
            page
        }
        _ => return invalid_field_in_cdb_response(),
    };
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn append_log_param_u32(page: &mut Vec<u8>, code: u16, value: u32) {
    page.extend_from_slice(&code.to_be_bytes());
    page.push(0x00);
    page.push(0x04);
    page.extend_from_slice(&value.to_be_bytes());
}

pub(crate) fn append_log_param_u64(page: &mut Vec<u8>, code: u16, value: u64) {
    page.extend_from_slice(&code.to_be_bytes());
    page.push(0x00);
    page.push(0x08);
    page.extend_from_slice(&value.to_be_bytes());
}

pub(crate) fn append_log_param_u16(page: &mut Vec<u8>, code: u16, value: u16) {
    page.extend_from_slice(&code.to_be_bytes());
    page.push(0x00);
    page.push(0x02);
    page.extend_from_slice(&value.to_be_bytes());
}

pub(crate) fn append_log_partition_param_u32(page: &mut Vec<u8>, code: u16, value: u32) {
    page.extend_from_slice(&code.to_be_bytes());
    page.push(0x03);
    page.push(0x08);
    page.push(0x07);
    page.push(0x00);
    page.extend_from_slice(&0u16.to_be_bytes());
    page.extend_from_slice(&value.to_be_bytes());
}

fn finalize_log_page(page: &mut [u8]) {
    let page_len = (page.len().saturating_sub(4)) as u16;
    page[2..4].copy_from_slice(&page_len.to_be_bytes());
}

pub(crate) fn clamp_u64_to_u32(value: u64) -> u32 {
    value.min(u32::MAX as u64) as u32
}

pub(crate) fn report_density_support_drive(
    state: &TapeState,
    cdb: &[u8],
    profile: &DeviceIdentityProfile,
) -> CdbResponse {
    let medium_type = (cdb.get(1).copied().unwrap_or(0) & 0x02) != 0;
    let Some(allocation_length) = try_read_be16(cdb, 7) else {
        return invalid_field_in_cdb_response();
    };
    if allocation_length < 4 {
        return invalid_field_in_cdb_response();
    }

    let descriptor = if medium_type && is_lto_profile(profile) {
        drive_medium_descriptor(state, profile)
    } else {
        drive_density_descriptor(profile, medium_capacity_bytes_for_state(state, profile)).to_vec()
    };
    let descriptor_len = descriptor.len();
    let avail_len = if descriptor_len > 0 {
        (descriptor_len + 2) as u16
    } else {
        0
    };

    let mut out = vec![0u8; 4];
    out[0..2].copy_from_slice(&avail_len.to_be_bytes());
    out.extend_from_slice(&descriptor);
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) fn read_attribute_drive(
    state: &mut TapeState,
    cdb: &[u8],
    profile: &DeviceIdentityProfile,
) -> CdbResponse {
    if cdb.len() < 14 {
        return invalid_field_in_cdb_response();
    }
    let service_action = cdb[1] & 0x1F;
    let partition_id = cdb[7];
    let first_attribute = u16::from_be_bytes([cdb[8], cdb[9]]);
    let allocation_length = u32::from_be_bytes([cdb[10], cdb[11], cdb[12], cdb[13]]) as usize;
    if allocation_length == 0 {
        return CdbResponse::good(vec![]);
    }
    let partition_count = usize::from(state.partition_runtime.addl_partitions_defined)
        .saturating_add(1)
        .clamp(1, 4);
    if usize::from(partition_id) >= partition_count {
        return invalid_field_in_cdb_response();
    }
    if ensure_mam_overrides_loaded(state).is_err() {
        return CdbResponse::check_condition(build_sense_fixed(0x03, 0x44, 0x00));
    }

    let mut attrs = drive_mam_attributes(state, profile);
    apply_mam_overrides_to_attrs(state, partition_id, &mut attrs);

    let out = match service_action {
        READ_ATTRIBUTE_ACTION_ATTRIBUTES => {
            let mut payload = vec![0u8; 4];
            if first_attribute != 0 && !attrs.iter().any(|entry| entry.id == first_attribute) {
                return invalid_field_in_cdb_response();
            }
            for entry in attrs.iter().filter(|entry| entry.id >= first_attribute) {
                if allocation_length >= 4
                    && payload
                        .len()
                        .saturating_add(read_attribute_entry_len(entry))
                        > allocation_length
                {
                    break;
                }
                append_read_attribute_entry(&mut payload, entry);
            }
            finalize_read_attribute_payload(&mut payload);
            payload
        }
        READ_ATTRIBUTE_ACTION_LIST => {
            let mut payload = vec![0u8; 4];
            for entry in &attrs {
                if allocation_length >= 4 && payload.len().saturating_add(2) > allocation_length {
                    break;
                }
                payload.extend_from_slice(&entry.id.to_be_bytes());
            }
            finalize_read_attribute_payload(&mut payload);
            payload
        }
        _ => return invalid_field_in_cdb_response(),
    };
    CdbResponse::good(truncate_reply(out, allocation_length))
}

pub(crate) struct ReadAttributeEntry {
    id: u16,
    format: u8,
    value: Vec<u8>,
}

#[derive(Debug, Clone)]
pub(crate) struct MamAttributeSchema {
    format: u8,
    max_len: usize,
    writable: bool,
}

pub(crate) fn drive_mam_attribute_map(
    state: &TapeState,
    profile: &DeviceIdentityProfile,
) -> BTreeMap<u16, MamAttributeSchema> {
    let mut map = BTreeMap::new();
    for entry in drive_mam_attributes(state, profile) {
        map.insert(
            entry.id,
            MamAttributeSchema {
                format: entry.format,
                max_len: entry.value.len(),
                writable: entry.format < 0x80,
            },
        );
    }
    map
}

pub(crate) fn mam_override_key(partition_id: u8, attr_id: u16) -> u32 {
    ((partition_id as u32) << 16) | u32::from(attr_id)
}

pub(crate) fn mam_override_file_path(state: &TapeState) -> Option<PathBuf> {
    state
        .active_layout
        .as_ref()
        .map(|layout| layout.root.join("mam.overrides"))
}

pub(crate) fn encode_hex(bytes: &[u8]) -> String {
    let mut out = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        out.push_str(&format!("{b:02X}"));
    }
    out
}

pub(crate) fn decode_hex(raw: &str) -> Option<Vec<u8>> {
    if !raw.len().is_multiple_of(2) {
        return None;
    }
    let mut out = Vec::with_capacity(raw.len() / 2);
    for idx in (0..raw.len()).step_by(2) {
        let byte = u8::from_str_radix(&raw[idx..idx + 2], 16).ok()?;
        out.push(byte);
    }
    Some(out)
}

pub(crate) fn ensure_mam_overrides_loaded(state: &mut TapeState) -> io::Result<()> {
    let Some(cartridge_id) = state.cartridge_id.as_deref() else {
        state.mam_overrides.clear();
        state.mam_loaded_for = None;
        return Ok(());
    };
    if state.mam_loaded_for.as_deref() == Some(cartridge_id) {
        return Ok(());
    }

    state.mam_overrides.clear();
    if let Some(path) = mam_override_file_path(state) {
        let raw = match fs::read_to_string(path) {
            Ok(content) => content,
            Err(err) if err.kind() == io::ErrorKind::NotFound => String::new(),
            Err(err) => return Err(err),
        };
        for line in raw.lines() {
            let trimmed = line.trim();
            if trimmed.is_empty() {
                continue;
            }
            let mut parts = trimmed.split('|');
            let Some(key_raw) = parts.next() else {
                continue;
            };
            let Some(format_raw) = parts.next() else {
                continue;
            };
            let Some(value_raw) = parts.next() else {
                continue;
            };
            if parts.next().is_some() {
                continue;
            }
            let Some(key) = u32::from_str_radix(key_raw, 16).ok() else {
                continue;
            };
            let Some(format) = u8::from_str_radix(format_raw, 16).ok() else {
                continue;
            };
            let Some(value) = decode_hex(value_raw) else {
                continue;
            };
            state.mam_overrides.insert(
                key,
                crate::scsi_tape::state::MamAttributeOverride { format, value },
            );
        }
    }
    state.mam_loaded_for = Some(cartridge_id.to_string());
    Ok(())
}

pub(crate) fn persist_mam_overrides(state: &TapeState) -> io::Result<()> {
    let Some(path) = mam_override_file_path(state) else {
        return Ok(());
    };
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let mut body = String::new();
    for (key, value) in &state.mam_overrides {
        body.push_str(&format!(
            "{key:08X}|{:02X}|{}\n",
            value.format,
            encode_hex(&value.value)
        ));
    }
    let tmp = path.with_extension("tmp");
    fs::write(&tmp, body)?;
    fs::rename(tmp, path)?;
    Ok(())
}

pub(crate) fn normalize_attribute_value(raw: &[u8], max_len: usize, format: u8) -> Vec<u8> {
    let mut out = if matches!(format & 0x7F, 0x01 | 0x02) {
        vec![b' '; max_len]
    } else {
        vec![0u8; max_len]
    };
    let copy_len = usize::min(max_len, raw.len());
    if copy_len > 0 {
        out[..copy_len].copy_from_slice(&raw[..copy_len]);
    }
    out
}

pub(crate) fn apply_mam_overrides_to_attrs(
    state: &TapeState,
    partition_id: u8,
    attrs: &mut [ReadAttributeEntry],
) {
    for entry in attrs {
        let key = mam_override_key(partition_id, entry.id);
        if let Some(override_entry) = state.mam_overrides.get(&key) {
            entry.format = override_entry.format;
            entry.value = override_entry.value.clone();
        }
    }
}

pub(crate) fn append_read_attribute_entry(out: &mut Vec<u8>, entry: &ReadAttributeEntry) {
    out.extend_from_slice(&entry.id.to_be_bytes());
    out.push(entry.format);
    out.extend_from_slice(&(entry.value.len() as u16).to_be_bytes());
    out.extend_from_slice(&entry.value);
}

pub(crate) fn read_attribute_entry_len(entry: &ReadAttributeEntry) -> usize {
    5usize.saturating_add(entry.value.len())
}

pub(crate) fn finalize_read_attribute_payload(payload: &mut [u8]) {
    let length = (payload.len().saturating_sub(4)) as u32;
    payload[0..4].copy_from_slice(&length.to_be_bytes());
}

pub(crate) fn drive_mam_attributes(
    state: &TapeState,
    profile: &DeviceIdentityProfile,
) -> Vec<ReadAttributeEntry> {
    let capacity_bytes = active_partition_capacity_bytes(state, profile);
    let remaining_bytes = capacity_bytes.saturating_sub(state.eod_position);
    let medium_type = medium_type_for_loaded_media(state, profile);
    let density_code = density_code_for_profile(profile);
    let fallback_serial = profile
        .serial_for_vpd_seed(&state.drive_id)
        .unwrap_or_else(|_| state.drive_id.clone());
    let medium_serial = state
        .cartridge_id
        .as_deref()
        .filter(|id| !id.trim().is_empty())
        .unwrap_or(&fallback_serial)
        .trim();
    let host_signature = "";
    let capacity_mib = bytes_to_mib_u32(capacity_bytes) as u64;
    let remaining_mib = bytes_to_mib_u32(remaining_bytes) as u64;
    let active_partition = state.partition_runtime.active_partition;
    let device_make_serial = format!("{} {}", profile.vendor.trim(), fallback_serial.trim());

    vec![
        // Cartridge attributes
        // Remaining/max capacity stored in MiB as 8-byte binary value.
        ReadAttributeEntry {
            id: 0x0000,
            format: 0x80,
            value: remaining_mib.to_be_bytes().to_vec(),
        },
        ReadAttributeEntry {
            id: 0x0001,
            format: 0x80,
            value: capacity_mib.to_be_bytes().to_vec(),
        },
        ReadAttributeEntry {
            id: 0x0002,
            format: 0x80,
            value: 0u64.to_be_bytes().to_vec(),
        }, // tapealert
        ReadAttributeEntry {
            id: 0x0003,
            format: 0x80,
            value: state.usage_counters.load_count.to_be_bytes().to_vec(),
        }, // load count
        ReadAttributeEntry {
            id: 0x0004,
            format: 0x80,
            value: 0u64.to_be_bytes().to_vec(),
        }, // MAM space remaining
        ReadAttributeEntry {
            id: 0x0005,
            format: 0x81,
            value: fixed_ascii_value("HOLO", 8),
        },
        ReadAttributeEntry {
            id: 0x0006,
            format: 0x80,
            value: vec![density_code],
        },
        ReadAttributeEntry {
            id: 0x0007,
            format: 0x80,
            value: vec![1],
        }, // initialization count baseline
        ReadAttributeEntry {
            id: 0x0008,
            format: 0x81,
            value: fixed_ascii_value("", 32),
        },
        ReadAttributeEntry {
            id: 0x0009,
            format: 0x80,
            value: clamp_u64_to_u32(state.usage_counters.volume_change_reference)
                .to_be_bytes()
                .to_vec(),
        }, // volume change reference
        // 0x020A-0x020D are device make/serial history slots, not host
        // application metadata. Host-writable application fields start at 0x0800.
        ReadAttributeEntry {
            id: 0x020A,
            format: 0x81,
            value: fixed_ascii_value(&device_make_serial, 40),
        },
        ReadAttributeEntry {
            id: 0x020B,
            format: 0x81,
            value: fixed_ascii_value(host_signature, 40),
        },
        ReadAttributeEntry {
            id: 0x020C,
            format: 0x81,
            value: fixed_ascii_value(host_signature, 40),
        },
        ReadAttributeEntry {
            id: 0x020D,
            format: 0x81,
            value: fixed_ascii_value(host_signature, 40),
        },
        ReadAttributeEntry {
            id: 0x0220,
            format: 0x80,
            value: (bytes_to_mib_u32(state.usage_counters.lifetime_bytes_written) as u64)
                .to_be_bytes()
                .to_vec(),
        },
        ReadAttributeEntry {
            id: 0x0221,
            format: 0x80,
            value: (bytes_to_mib_u32(state.usage_counters.lifetime_bytes_read) as u64)
                .to_be_bytes()
                .to_vec(),
        },
        ReadAttributeEntry {
            id: 0x0222,
            format: 0x80,
            value: (bytes_to_mib_u32(state.usage_counters.last_load_bytes_written) as u64)
                .to_be_bytes()
                .to_vec(),
        },
        ReadAttributeEntry {
            id: 0x0223,
            format: 0x80,
            value: (bytes_to_mib_u32(state.usage_counters.last_load_bytes_read) as u64)
                .to_be_bytes()
                .to_vec(),
        },
        ReadAttributeEntry {
            id: 0x0224,
            format: 0x80,
            value: 0u64.to_be_bytes().to_vec(),
        },
        ReadAttributeEntry {
            id: 0x0225,
            format: 0x80,
            value: 0u64.to_be_bytes().to_vec(),
        },
        // Medium attributes
        ReadAttributeEntry {
            id: 0x0400,
            format: 0x01,
            value: fixed_ascii_value(profile.vendor.trim(), 8),
        },
        ReadAttributeEntry {
            id: 0x0401,
            format: 0x01,
            value: fixed_ascii_value(medium_serial, 32),
        },
        ReadAttributeEntry {
            id: 0x0402,
            format: 0x80,
            value: medium_length_meters_for_state(state, profile)
                .to_be_bytes()
                .to_vec(),
        }, // tape length (meters)
        ReadAttributeEntry {
            id: 0x0403,
            format: 0x80,
            value: 127u32.to_be_bytes().to_vec(),
        }, // tape width baseline
        ReadAttributeEntry {
            id: 0x0404,
            format: 0x81,
            value: fixed_ascii_value("LTO-CVE", 8),
        },
        ReadAttributeEntry {
            id: 0x0405,
            format: 0x80,
            value: vec![density_code],
        },
        ReadAttributeEntry {
            id: 0x0406,
            format: 0x81,
            value: fixed_ascii_value("20260417", 8),
        },
        ReadAttributeEntry {
            id: 0x0407,
            format: 0x80,
            value: 4096u64.to_be_bytes().to_vec(),
        }, // MAM capacity, not tape data capacity.
        ReadAttributeEntry {
            id: 0x0408,
            format: 0x80,
            value: vec![medium_type],
        },
        ReadAttributeEntry {
            id: 0x0409,
            format: 0x80,
            value: 0u16.to_be_bytes().to_vec(),
        },
        // Host attributes
        ReadAttributeEntry {
            id: 0x0800,
            format: 0x01,
            value: fixed_ascii_value("", 8),
        },
        ReadAttributeEntry {
            id: 0x0801,
            format: 0x01,
            value: fixed_ascii_value("", 32),
        },
        ReadAttributeEntry {
            id: 0x0802,
            format: 0x01,
            value: fixed_ascii_value("", 8),
        },
        ReadAttributeEntry {
            id: 0x0803,
            format: 0x02,
            value: fixed_ascii_value("", 160),
        },
        ReadAttributeEntry {
            id: 0x0804,
            format: 0x01,
            value: fixed_ascii_value("", 12),
        },
        ReadAttributeEntry {
            id: 0x0805,
            format: 0x00,
            value: vec![0x00],
        },
        ReadAttributeEntry {
            id: 0x0806,
            format: 0x01,
            value: fixed_ascii_value("", 32),
        },
        ReadAttributeEntry {
            id: 0x0807,
            format: 0x02,
            value: fixed_ascii_value("", 80),
        },
        ReadAttributeEntry {
            id: 0x0808,
            format: 0x02,
            value: fixed_ascii_value("", 160),
        },
        ReadAttributeEntry {
            id: 0x0809,
            format: 0x01,
            value: fixed_ascii_value("", 16),
        },
        ReadAttributeEntry {
            id: 0x080A,
            format: 0x00,
            value: vec![active_partition],
        },
        ReadAttributeEntry {
            id: 0x080B,
            format: 0x01,
            value: fixed_ascii_value("", 16),
        },
        ReadAttributeEntry {
            id: 0x080C,
            format: 0x00,
            value: vec![0u8; 256],
        },
        ReadAttributeEntry {
            id: 0x1500,
            format: 0x00,
            value: 0u32.to_be_bytes().to_vec(),
        },
    ]
}

pub(crate) fn fixed_ascii_value(value: &str, len: usize) -> Vec<u8> {
    let mut out = vec![b' '; len];
    let filtered = value
        .chars()
        .filter(|ch| ch.is_ascii_graphic() || *ch == ' ')
        .collect::<String>();
    let bytes = filtered.as_bytes();
    let copy_len = usize::min(len, bytes.len());
    if copy_len > 0 {
        out[..copy_len].copy_from_slice(&bytes[..copy_len]);
    }
    out
}

pub(crate) fn drive_density_descriptor(
    profile: &DeviceIdentityProfile,
    capacity_bytes: u64,
) -> [u8; 52] {
    let mut descriptor = [0u8; 52];
    let density_code = density_code_for_profile(profile);
    descriptor[0] = density_code;
    descriptor[1] = density_code;
    descriptor[2] = 0x01; // writable media

    // Conservative non-zero defaults so host probes don't treat capabilities as unknown.
    descriptor[8..10].copy_from_slice(&127u16.to_be_bytes()); // media width
    descriptor[10..12].copy_from_slice(&64u16.to_be_bytes()); // logical tracks
    descriptor[12..16].copy_from_slice(&bytes_to_mib_u32(capacity_bytes).to_be_bytes());

    write_ascii_padded(&mut descriptor[16..24], &profile.vendor);
    write_ascii_padded(&mut descriptor[24..32], &density_name_for_profile(profile));
    write_ascii_padded(
        &mut descriptor[32..52],
        &density_description_for_profile(profile),
    );
    descriptor
}

pub(crate) fn density_code_for_profile(profile: &DeviceIdentityProfile) -> u8 {
    let product = profile.product.to_ascii_uppercase();
    if product.contains("TD1") || product.contains("ULTRIUM 1") {
        return 0x40;
    }
    if product.contains("TD2") || product.contains("ULTRIUM 2") {
        return 0x42;
    }
    if product.contains("TD3") || product.contains("ULTRIUM 3") {
        return 0x44;
    }
    if product.contains("TD4") || product.contains("ULTRIUM 4") {
        return 0x46;
    }
    if product.contains("TD5") || product.contains("ULTRIUM 5") {
        return 0x58;
    }
    if product.contains("TD6") || product.contains("ULTRIUM 6") {
        return 0x5A;
    }
    if product.contains("TD7") || product.contains("ULTRIUM 7") {
        return 0x5D;
    }
    if product.contains("TD8") || product.contains("ULTRIUM 8") {
        return 0x5E;
    }
    if product.contains("TD9") || product.contains("ULTRIUM 9") {
        return 0x5F;
    }
    0x5A
}

pub(crate) fn native_capacity_bytes_for_profile(profile: &DeviceIdentityProfile) -> u64 {
    let product = profile.product.to_ascii_uppercase();
    if product.contains("TD1") || product.contains("ULTRIUM 1") {
        return 100 * 1024 * 1024 * 1024;
    }
    if product.contains("TD2") || product.contains("ULTRIUM 2") {
        return 200 * 1024 * 1024 * 1024;
    }
    if product.contains("TD3") || product.contains("ULTRIUM 3") {
        return 400 * 1024 * 1024 * 1024;
    }
    if product.contains("TD4") || product.contains("ULTRIUM 4") {
        return 800 * 1024 * 1024 * 1024;
    }
    if product.contains("TD5") || product.contains("ULTRIUM 5") {
        return 1500 * 1024 * 1024 * 1024;
    }
    if product.contains("TD6") || product.contains("ULTRIUM 6") {
        return 2500 * 1024 * 1024 * 1024;
    }
    if product.contains("TD7") || product.contains("ULTRIUM 7") {
        return 6000 * 1024 * 1024 * 1024;
    }
    if product.contains("TD8") || product.contains("ULTRIUM 8") {
        return 12000 * 1024 * 1024 * 1024;
    }
    if product.contains("TD9") || product.contains("ULTRIUM 9") {
        return 18000 * 1024 * 1024 * 1024;
    }
    2500 * 1024 * 1024 * 1024
}

pub(crate) fn native_transfer_rate_mib_per_sec_for_profile(profile: &DeviceIdentityProfile) -> u32 {
    let generation = lto_generation_for_profile(profile);
    if is_lto_profile(profile) {
        return match generation {
            1 => 20,
            2 => 40,
            3 => 80,
            4 => 120,
            5 => 140,
            6 => 160,
            7 => 300,
            8 => 360,
            9 => 400,
            _ => 160,
        };
    }
    let product = profile.product.to_ascii_uppercase();
    if product.contains("VS80") {
        return 3;
    }
    if product.contains("VS160") {
        return 8;
    }
    if product.contains("SDLT600") {
        return 36;
    }
    if product.contains("SDLT320") {
        return 16;
    }
    if product.contains("SDLT1") || product.contains("SDLT220") || product.contains("SUPERDLT1") {
        return 11;
    }
    160
}

pub(crate) fn max_additional_partitions_for_profile(profile: &DeviceIdentityProfile) -> u8 {
    let product = profile.product.to_ascii_uppercase();
    if product.contains("TD6") || product.contains("ULTRIUM 6") {
        return 3;
    }
    if product.contains("TD5") || product.contains("ULTRIUM 5") {
        return 1;
    }
    0
}

pub(crate) fn density_name_for_profile(profile: &DeviceIdentityProfile) -> String {
    let density_code = density_code_for_profile(profile);
    format!("DENS{density_code:02X}")
}

pub(crate) fn density_description_for_profile(profile: &DeviceIdentityProfile) -> String {
    let product = profile.product.trim();
    if product.is_empty() {
        return "Virtual Tape Media".to_string();
    }
    format!("{product} media")
}

pub(crate) fn is_lto_profile(profile: &DeviceIdentityProfile) -> bool {
    let product = profile.product.to_ascii_uppercase();
    product.contains("TD") || product.contains("ULTRIUM")
}

pub(crate) fn lto_generation_for_profile(profile: &DeviceIdentityProfile) -> u8 {
    let product = profile.product.to_ascii_uppercase();
    if product.contains("TD1") || product.contains("ULTRIUM 1") {
        return 1;
    }
    if product.contains("TD2") || product.contains("ULTRIUM 2") {
        return 2;
    }
    if product.contains("TD3") || product.contains("ULTRIUM 3") {
        return 3;
    }
    if product.contains("TD4") || product.contains("ULTRIUM 4") {
        return 4;
    }
    if product.contains("TD5") || product.contains("ULTRIUM 5") {
        return 5;
    }
    if product.contains("TD6") || product.contains("ULTRIUM 6") {
        return 6;
    }
    if product.contains("TD7") || product.contains("ULTRIUM 7") {
        return 7;
    }
    if product.contains("TD8") || product.contains("ULTRIUM 8") {
        return 8;
    }
    if product.contains("TD9") || product.contains("ULTRIUM 9") {
        return 9;
    }
    6
}

pub(crate) fn lto_media_length_for_generation(generation: u8) -> u16 {
    match generation {
        1 | 2 => 609,
        3 => 680,
        4 => 820,
        5 | 6 => 846,
        7 => 960,
        8 => 1024,
        9 => 1100,
        _ => 846,
    }
}

pub(crate) fn medium_length_meters_for_state(
    state: &TapeState,
    profile: &DeviceIdentityProfile,
) -> u16 {
    if !has_custom_capacity_override(state, profile) {
        return lto_media_length_for_generation(lto_generation_for_profile(profile));
    }
    let native_capacity = native_capacity_bytes_for_profile(profile);
    if native_capacity == 0 {
        return 1;
    }
    let native_length = u64::from(lto_media_length_for_generation(lto_generation_for_profile(
        profile,
    )));
    let custom_capacity = medium_capacity_bytes_for_state(state, profile);
    let scaled = native_length
        .saturating_mul(custom_capacity)
        .saturating_add(native_capacity - 1)
        / native_capacity;
    scaled.clamp(1, u16::MAX as u64) as u16
}

pub(crate) fn drive_medium_descriptor(
    state: &TapeState,
    profile: &DeviceIdentityProfile,
) -> Vec<u8> {
    // SCSI SSC REPORT DENSITY SUPPORT medium descriptor (56 bytes).
    let mut descriptor = vec![0u8; 56];
    let density_code = density_code_for_profile(profile);
    let generation = lto_generation_for_profile(profile);

    descriptor[0] = if state.retention_policy.is_worm_media {
        0x01
    } else {
        0x00
    };
    descriptor[2..4].copy_from_slice(&0x0034u16.to_be_bytes());
    descriptor[4] = density_code;
    descriptor[5] = density_code;
    descriptor[14..16].copy_from_slice(&127u16.to_be_bytes());
    descriptor[16..18]
        .copy_from_slice(&medium_length_meters_for_state(state, profile).to_be_bytes());

    if has_custom_capacity_override(state, profile) {
        write_ascii_padded(&mut descriptor[20..28], "HOLO");
        write_ascii_padded(&mut descriptor[28..36], "Data");
        write_ascii_padded(&mut descriptor[36..56], "HOLO Custom Tape");
    } else {
        write_ascii_padded(&mut descriptor[20..28], profile.vendor.trim());
        if state.retention_policy.is_worm_media {
            write_ascii_padded(&mut descriptor[28..36], "WORM");
        } else {
            write_ascii_padded(&mut descriptor[28..36], "Data");
        }

        let description = if state.retention_policy.is_worm_media {
            format!("Ultrium {generation} WORM Tape")
        } else {
            format!("Ultrium {generation} Data Tape")
        };
        write_ascii_padded(&mut descriptor[36..56], &description);
    }
    descriptor
}

pub(crate) fn medium_type_for_loaded_media(
    state: &TapeState,
    profile: &DeviceIdentityProfile,
) -> u8 {
    if state.mount_state != crate::scsi_tape::state::MountState::Loaded {
        return 0;
    }
    medium_type_for_profile(state, profile)
}

pub(crate) fn medium_type_for_profile(
    state: &TapeState,
    profile: &DeviceIdentityProfile,
) -> u8 {
    let generation = lto_generation_for_profile(profile);
    let base = match generation {
        1..=9 => generation.saturating_mul(0x10).saturating_add(0x08),
        _ => 0x68,
    };
    if state.retention_policy.is_worm_media {
        base.saturating_add(0x04)
    } else {
        base
    }
}

pub(crate) fn device_specific_mode_parameter(state: &TapeState) -> u8 {
    let mut value = 0x00;
    if state.buffered_mode {
        value |= 0x10;
    }
    if state.retention_policy.is_worm_media && state.retention_policy.retention_locked {
        value |= 0x80;
    }
    value
}

pub(crate) fn bytes_to_mib_u32(value: u64) -> u32 {
    let mib = value / (1024 * 1024);
    if mib > u32::MAX as u64 {
        return u32::MAX;
    }
    mib as u32
}

pub(crate) fn write_ascii_padded(buf: &mut [u8], value: &str) {
    buf.fill(b' ');
    let filtered = value
        .chars()
        .filter(|ch| ch.is_ascii_graphic() || *ch == ' ')
        .collect::<String>();
    let bytes = filtered.as_bytes();
    let copy_len = usize::min(buf.len(), bytes.len());
    if copy_len > 0 {
        buf[..copy_len].copy_from_slice(&bytes[..copy_len]);
    }
}

pub(crate) fn truncate_reply(mut reply: Vec<u8>, allocation_len: usize) -> Vec<u8> {
    if allocation_len == 0 {
        return vec![];
    }
    if reply.len() > allocation_len {
        reply.truncate(allocation_len);
    }
    reply
}
