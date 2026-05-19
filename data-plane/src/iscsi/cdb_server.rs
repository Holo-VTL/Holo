use std::{
    collections::{BTreeMap, HashMap},
    env, fs, io,
    path::PathBuf,
    sync::{Mutex, OnceLock},
    time::{Duration, Instant},
};

use crate::iscsi::cdb_changer::{
    build_sense_fixed, dispatch_changer_cdb_with_context, volume_overflow_response,
};
use crate::iscsi::cdb_drive::{
    active_partition_capacity_bytes, apply_medium_capacity_override,
    dispatch_drive_discovery_cdb_with_context, ensure_partition_runtime_initialized,
};
pub use crate::iscsi::cdb_wire::{
    CdbPacket, CdbResponse, MAX_DATA_LEN, MAX_SENSE_LEN, SCSI_STATUS_BUSY,
    SCSI_STATUS_CHECK_CONDITION, SCSI_STATUS_GOOD,
};
use crate::scsi_tape::command_chain::{
    configured_initiator, read_fixed_blocks, write_fixed_blocks, EraseMode,
};
use crate::scsi_tape::commands_core::{
    execute_with_sense, tape_error_to_sense, CoreCommand, CoreResponse,
};
use crate::scsi_tape::identity::DeviceType;
use crate::scsi_tape::sense::SenseFrame;
use crate::scsi_tape::state::TapeState;
use crate::storage::layout::{checksum32, sanitize_id};

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct CdbDispatchContext {
    pub initiator: Option<String>,
}

impl CdbDispatchContext {
    pub fn with_initiator(initiator: impl Into<String>) -> Self {
        let value = initiator.into();
        let trimmed = value.trim();
        if trimmed.is_empty() {
            Self::default()
        } else {
            Self {
                initiator: Some(trimmed.to_string()),
            }
        }
    }

    pub(crate) fn initiator_or_default(&self) -> &str {
        match self
            .initiator
            .as_deref()
            .filter(|value| !value.trim().is_empty())
        {
            Some(value) => value,
            None => configured_initiator(),
        }
    }
}

/// Convert a `SenseFrame` into a fixed-format sense byte vector (18 bytes).
pub fn sense_frame_to_bytes(frame: &SenseFrame) -> Vec<u8> {
    let mut s = vec![0u8; 18];
    s[0] = if frame.information_valid { 0xF0 } else { 0x70 }; // Current errors, fixed format (+ VALID info when present)
    s[2] = frame.sense_key & 0x0F;
    if frame.end_of_medium {
        s[2] |= 0x40;
    }
    if frame.information_valid {
        s[3..7].copy_from_slice(&frame.information.to_be_bytes());
    }
    s[7] = 10; // Additional sense length = 18 - 8
    s[12] = frame.asc;
    s[13] = frame.ascq;
    s
}

/// Dispatch a raw CDB to the tape state machine and return an IPC response.
/// `state` is the per-session tape state; it is mutated by write/control commands.
pub fn dispatch_raw_cdb(state: &mut TapeState, cdb: &[u8], data_out: &[u8]) -> CdbResponse {
    dispatch_raw_cdb_with_context(state, cdb, data_out, CdbDispatchContext::default())
}

/// Dispatch a raw CDB with transport-supplied initiator context.
pub fn dispatch_raw_cdb_with_context(
    state: &mut TapeState,
    cdb: &[u8],
    data_out: &[u8],
    context: CdbDispatchContext,
) -> CdbResponse {
    let started_at = Instant::now();
    let opcode = cdb.first().copied().unwrap_or(0);
    let data_out_len = data_out.len();
    if let Some(initiator) = context
        .initiator
        .as_deref()
        .map(str::trim)
        .filter(|value| !value.is_empty())
    {
        let default_queue = state.take_nexus_unit_attention(initiator);
        let response = dispatch_raw_cdb_with_context_inner(state, cdb, data_out, &context);
        state.restore_nexus_unit_attention(initiator, default_queue);
        log_slow_cdb_dispatch(started_at, opcode, data_out_len, &response);
        return response;
    }
    let response = dispatch_raw_cdb_with_context_inner(state, cdb, data_out, &context);
    log_slow_cdb_dispatch(started_at, opcode, data_out_len, &response);
    response
}

fn log_slow_cdb_dispatch(
    started_at: Instant,
    opcode: u8,
    data_out_len: usize,
    response: &CdbResponse,
) {
    let elapsed = started_at.elapsed();
    if elapsed >= slow_cdb_dispatch_threshold() {
        eprintln!(
            "[slow_cdb_dispatch] opcode=0x{opcode:02X} status=0x{status:02X} elapsed_ms={} data_out_len={} data_in_len={} sense_len={}",
            elapsed.as_millis(),
            data_out_len,
            response.reply.len(),
            response.sense.len(),
            status = response.status,
        );
    }
}

fn slow_cdb_dispatch_threshold() -> Duration {
    static THRESHOLD: OnceLock<Duration> = OnceLock::new();
    *THRESHOLD.get_or_init(|| {
        let millis = env::var("HOLO_SLOW_CDB_DISPATCH_MS")
            .ok()
            .and_then(|raw| raw.parse::<u64>().ok())
            .filter(|value| *value > 0)
            .unwrap_or(1000);
        Duration::from_millis(millis)
    })
}

fn dispatch_raw_cdb_with_context_inner(
    state: &mut TapeState,
    cdb: &[u8],
    data_out: &[u8],
    context: &CdbDispatchContext,
) -> CdbResponse {
    if cdb.is_empty() {
        let sense = sense_frame_to_bytes(&crate::scsi_tape::sense::sense_for_context(
            crate::scsi_tape::sense::SenseContext::IllegalRequest,
        ));
        return CdbResponse::check_condition(sense);
    }
    if !validate_cdb_structure(cdb) {
        return CdbResponse::check_condition(build_sense_fixed(0x05, 0x24, 0x00));
    }

    let opcode = cdb[0];
    let active_profile = crate::scsi_tape::profiles::resolve_active_profile_from_env();
    if active_profile.device_type == DeviceType::Changer {
        let response =
            dispatch_changer_cdb_with_context(state, cdb, data_out, active_profile, context);
        trace_cdb_if_enabled("changer", cdb, data_out, &response);
        return response;
    }
    if drive_opcode_requires_media_sync(opcode, state) {
        sync_drive_mount_from_shared(state);
    }
    if opcode == 0x00 {
        // TEST UNIT READY
        let response = if let Some((asc, ascq)) = state.take_unit_attention() {
            CdbResponse::check_condition(build_sense_fixed(0x06, asc, ascq))
        } else if state.mount_state == crate::scsi_tape::state::MountState::Loaded {
            CdbResponse::good(vec![])
        } else {
            CdbResponse::check_condition(build_sense_fixed(0x02, 0x3A, 0x00))
        };
        trace_cdb_if_enabled("drive", cdb, data_out, &response);
        return response;
    }
    if opcode == 0x03 {
        // REQUEST SENSE
        let response = CdbResponse::good(request_sense_response(state));
        trace_cdb_if_enabled("drive", cdb, data_out, &response);
        return response;
    }
    if let Some((asc, ascq)) = take_pending_unit_attention_for_opcode(state, opcode) {
        let response = CdbResponse::check_condition(build_sense_fixed(0x06, asc, ascq));
        trace_cdb_if_enabled("drive", cdb, data_out, &response);
        return response;
    }
    let drive_profile = crate::scsi_tape::profiles::resolve_drive_profile_from_env();
    ensure_partition_runtime_initialized(state, &drive_profile);
    if let Some(response) =
        dispatch_drive_discovery_cdb_with_context(state, cdb, data_out, &drive_profile, context)
    {
        sync_loaded_cartridge_usage_after_mutation(state, opcode, &response);
        trace_cdb_if_enabled("drive", cdb, data_out, &response);
        return response;
    }
    if is_legacy_empty_write_probe(state, cdb, data_out)
        || is_legacy_single_byte_write_probe(state, cdb, data_out)
    {
        // Some backup software issues WRITE(6) fixed=1, transfer-length=1
        // with no DATA OUT (or a single zero probe byte) at BOT.
        // Treat these probes as no-op GOOD so the medium is not polluted.
        let response = CdbResponse::good(vec![]);
        trace_cdb_if_enabled("drive", cdb, data_out, &response);
        return response;
    }
    if is_data_write_opcode(opcode)
        && state.mount_state == crate::scsi_tape::state::MountState::Loaded
    {
        let capacity = active_partition_capacity_bytes(state, &drive_profile);
        let write_bytes = projected_write_bytes(state, cdb, data_out);
        let projected_position = state.current_position.saturating_add(write_bytes);
        if projected_position > capacity {
            let response = volume_overflow_response();
            trace_cdb_if_enabled("drive", cdb, data_out, &response);
            return response;
        }
    }
    if let Some(response) = dispatch_fixed_block_io_with_context(state, cdb, data_out, context) {
        sync_loaded_cartridge_usage_after_mutation(state, opcode, &response);
        trace_cdb_if_enabled("drive", cdb, data_out, &response);
        return response;
    }

    let cmd = decode_cdb_to_command(state, opcode, cdb, data_out);
    let response = match cmd {
        None => {
            // Unsupported opcode → ILLEGAL REQUEST
            let sense = sense_frame_to_bytes(&crate::scsi_tape::sense::sense_for_context(
                crate::scsi_tape::sense::SenseContext::IllegalRequest,
            ));
            CdbResponse::check_condition(sense)
        }
        Some(core_cmd) => match execute_with_sense(state, core_cmd) {
            Ok(response) => {
                let data_in = core_response_to_bytes(state, response, cdb);
                CdbResponse::good(data_in)
            }
            Err(sense_frame) => {
                let sense_bytes = sense_frame_to_bytes(&sense_frame);
                CdbResponse::check_condition(sense_bytes)
            }
        },
    };
    sync_loaded_cartridge_usage_after_mutation(state, opcode, &response);
    trace_cdb_if_enabled("drive", cdb, data_out, &response);
    response
}

pub(crate) fn request_sense_response(state: &mut TapeState) -> Vec<u8> {
    if let Some((asc, ascq)) = state.take_unit_attention() {
        // UNIT ATTENTION / ASC/ASCQ from queued lifecycle event.
        return build_sense_fixed(0x06, asc, ascq);
    }
    let mut s = vec![0u8; 18];
    s[0] = 0x70; // Current errors, fixed format
    s[7] = 10; // Additional sense length = 18 - 8
    s
}

pub(crate) fn take_pending_unit_attention_for_opcode(
    state: &mut TapeState,
    opcode: u8,
) -> Option<(u8, u8)> {
    if opcode == 0x12 {
        return None;
    }
    // Keep the startup power-on/reset UA on the legacy TUR/REQUEST SENSE path
    // while allowing media and parameter-change UAs to interrupt real work.
    let index = state
        .pending_unit_attention
        .iter()
        .position(|&(asc, ascq)| (asc, ascq) != (0x29, 0x00))?;
    state.pending_unit_attention.remove(index)
}

pub(crate) fn scsi_trace_enabled() -> bool {
    let path = env::var("HOLO_SCSI_TRACE_CONFIG").unwrap_or_default();
    if !path.trim().is_empty() {
        if let Ok(value) = fs::read_to_string(path.trim()) {
            return trace_flag_enabled(&value);
        }
    }
    trace_flag_enabled(&env::var("HOLO_SCSI_TRACE").unwrap_or_default())
}

fn trace_flag_enabled(raw: &str) -> bool {
    matches!(
        raw.trim().to_ascii_lowercase().as_str(),
        "1" | "true" | "yes" | "on"
    )
}

pub(crate) fn validate_cdb_structure(cdb: &[u8]) -> bool {
    let len = cdb.len();
    if !matches!(len, 6 | 10 | 12 | 16) {
        return false;
    }
    match expected_cdb_length(cdb[0]) {
        Some(expected) => len == expected,
        None => true,
    }
}

pub(crate) fn expected_cdb_length(opcode: u8) -> Option<usize> {
    match opcode >> 5 {
        0 => Some(6),
        1 | 2 => Some(10),
        4 => Some(16),
        5 => Some(12),
        _ => None,
    }
}

pub(crate) fn bytes_to_hex(bytes: &[u8], max_len: usize) -> String {
    let shown = &bytes[..usize::min(bytes.len(), max_len)];
    let mut out = String::new();
    for (idx, b) in shown.iter().enumerate() {
        if idx > 0 {
            out.push(' ');
        }
        out.push_str(&format!("{b:02X}"));
    }
    if bytes.len() > shown.len() {
        out.push_str(" ...");
    }
    out
}

pub(crate) fn is_data_write_opcode(opcode: u8) -> bool {
    matches!(opcode, 0x0A | 0x8A)
}

pub(crate) fn is_media_usage_mutating_opcode(opcode: u8) -> bool {
    matches!(opcode, 0x04 | 0x0A | 0x8A | 0x10 | 0x19)
}

pub(crate) fn sync_loaded_cartridge_usage_after_mutation(
    state: &TapeState,
    opcode: u8,
    response: &CdbResponse,
) {
    if response.status == SCSI_STATUS_GOOD && is_media_usage_mutating_opcode(opcode) {
        sync_loaded_cartridge_usage_to_shared_throttled(state, !is_data_write_opcode(opcode));
    }
}

pub(crate) fn is_data_transfer_opcode(opcode: u8) -> bool {
    matches!(opcode, 0x08 | 0x88 | 0x0A | 0x8A)
}

pub(crate) fn drive_opcode_requires_media_sync(opcode: u8, state: &TapeState) -> bool {
    state.mount_state != crate::scsi_tape::state::MountState::Loaded
        || !is_data_transfer_opcode(opcode)
}

pub(crate) fn is_fixed_block_transfer(cdb: &[u8]) -> bool {
    cdb.get(1).copied().unwrap_or(0) & 0x01 != 0
}

pub(crate) fn write_transfer_blocks(opcode: u8, cdb: &[u8]) -> u32 {
    match opcode {
        // WRITE(6): byte 4 (0 means 256 blocks)
        0x0A => {
            let raw = cdb.get(4).copied().unwrap_or(0);
            if raw == 0 {
                256
            } else {
                raw as u32
            }
        }
        // WRITE(16): bytes 10..13
        0x8A => read_be32(cdb, 10) as u32,
        _ => 0,
    }
}

pub(crate) fn read_transfer_blocks(opcode: u8, cdb: &[u8]) -> u32 {
    match opcode {
        // READ(6): bytes 2..4 (0 means 256 blocks)
        0x08 => {
            let raw = read_be24(cdb, 2) as u32;
            if raw == 0 {
                256
            } else {
                raw
            }
        }
        // READ(16): bytes 10..13
        0x88 => read_be32(cdb, 10) as u32,
        _ => 0,
    }
}

pub(crate) fn fixed_block_size(state: &TapeState) -> Option<usize> {
    if state.block_mode.mode != crate::scsi_tape::state::BlockMode::Fixed {
        return None;
    }
    let size = state.block_mode.fixed_block_size as usize;
    if size == 0 {
        return None;
    }
    Some(size)
}

#[cfg(test)]
pub(crate) fn dispatch_fixed_block_io(
    state: &mut TapeState,
    cdb: &[u8],
    data_out: &[u8],
) -> Option<CdbResponse> {
    dispatch_fixed_block_io_with_context(state, cdb, data_out, &CdbDispatchContext::default())
}

pub(crate) fn dispatch_fixed_block_io_with_context(
    state: &mut TapeState,
    cdb: &[u8],
    data_out: &[u8],
    context: &CdbDispatchContext,
) -> Option<CdbResponse> {
    let opcode = cdb.first().copied().unwrap_or(0);
    if !is_fixed_block_transfer(cdb) {
        return None;
    }
    let block_size = fixed_block_size(state)?;
    match opcode {
        0x0A | 0x8A => {
            let transfer_blocks = write_transfer_blocks(opcode, cdb) as usize;
            if transfer_blocks == 0 {
                return Some(CdbResponse::check_condition(build_sense_fixed(
                    0x05, 0x24, 0x00,
                )));
            }
            let expected_len = transfer_blocks.saturating_mul(block_size);
            if expected_len != data_out.len() {
                return Some(CdbResponse::check_condition(build_sense_fixed(
                    0x05, 0x24, 0x00,
                )));
            }
            if let Err(error) = write_fixed_blocks(
                state,
                data_out,
                block_size,
                Some(context.initiator_or_default()),
            ) {
                let sense_frame = tape_error_to_sense(&error);
                return Some(CdbResponse::check_condition(sense_frame_to_bytes(
                    &sense_frame,
                )));
            }
            Some(CdbResponse::good(vec![]))
        }
        0x08 | 0x88 => {
            let transfer_blocks = read_transfer_blocks(opcode, cdb) as usize;
            if transfer_blocks == 0 {
                return Some(CdbResponse::check_condition(build_sense_fixed(
                    0x05, 0x24, 0x00,
                )));
            }
            let out = match read_fixed_blocks(state, block_size, transfer_blocks) {
                Ok(out) => out,
                Err(error) => {
                    let sense_frame = tape_error_to_sense(&error);
                    return Some(CdbResponse::check_condition(sense_frame_to_bytes(
                        &sense_frame,
                    )));
                }
            };
            Some(CdbResponse::good(out))
        }
        _ => None,
    }
}

pub(crate) fn is_legacy_empty_write_probe(state: &TapeState, cdb: &[u8], data_out: &[u8]) -> bool {
    if data_out.is_empty()
        && state.mount_state == crate::scsi_tape::state::MountState::Loaded
        && state.current_position == 0
        && state.eod_position == 0
        && cdb.get(1).copied().unwrap_or(0) & 0x01 != 0
    {
        return match cdb.first().copied().unwrap_or(0) {
            // WRITE(6): transfer length at byte 4.
            0x0A => cdb.get(4).copied().unwrap_or(0) == 1,
            // WRITE(16): transfer length at bytes 10..13.
            0x8A => read_be32(cdb, 10) == 1,
            _ => false,
        };
    }
    false
}

pub(crate) fn is_legacy_single_byte_write_probe(
    state: &TapeState,
    cdb: &[u8],
    data_out: &[u8],
) -> bool {
    if data_out.len() != 1
        || state.mount_state != crate::scsi_tape::state::MountState::Loaded
        || state.current_position != 0
        || state.eod_position != 0
        || cdb.get(1).copied().unwrap_or(0) & 0x01 == 0
        || state.block_mode.mode != crate::scsi_tape::state::BlockMode::Fixed
        || state.block_mode.fixed_block_size <= 1
    {
        return false;
    }

    match cdb.first().copied().unwrap_or(0) {
        // WRITE(6): transfer length at byte 4.
        0x0A => cdb.get(4).copied().unwrap_or(0) == 1,
        // WRITE(16): transfer length at bytes 10..13.
        0x8A => read_be32(cdb, 10) == 1,
        _ => false,
    }
}

pub(crate) fn shared_media_state_dir() -> PathBuf {
    if let Ok(raw) = env::var("HOLO_MEDIA_STATE_DIR") {
        let trimmed = raw.trim();
        if !trimmed.is_empty() {
            return PathBuf::from(trimmed);
        }
    }
    if cfg!(test) {
        return PathBuf::from("/tmp/holo-media-state-tests");
    }
    PathBuf::from("/run/holo/media-state")
}

pub(crate) fn media_state_key_for_state(state: &TapeState) -> String {
    if let Ok(raw) = env::var("HOLO_MEDIA_STATE_KEY") {
        let trimmed = raw.trim();
        if !trimmed.is_empty() {
            return trimmed.to_string();
        }
    }
    state.drive_id.clone()
}

pub(crate) const MEDIA_STATE_KEY_SEPARATOR: &str = "__";

pub(crate) fn split_media_state_key(media_state_key: &str) -> (String, Option<String>) {
    let trimmed = media_state_key.trim();
    if trimmed.is_empty() {
        return ("unknown".to_string(), None);
    }
    if let Some((library, drive)) = trimmed.split_once(MEDIA_STATE_KEY_SEPARATOR) {
        let library = if library.trim().is_empty() {
            "unknown".to_string()
        } else {
            library.trim().to_string()
        };
        let drive = if drive.trim().is_empty() {
            None
        } else {
            Some(drive.trim().to_string())
        };
        return (library, drive);
    }
    (trimmed.to_string(), None)
}

pub(crate) fn configured_changer_drive_ids() -> Vec<String> {
    let raw = match env::var("HOLO_CHANGER_DRIVE_IDS") {
        Ok(v) => v,
        Err(_) => return Vec::new(),
    };
    let mut ids = raw
        .split(',')
        .map(str::trim)
        .filter(|id| !id.is_empty())
        .map(str::to_string)
        .collect::<Vec<_>>();
    ids.sort_unstable();
    ids.dedup();
    ids
}

pub(crate) fn discover_changer_drive_ids_from_slots(library_id: &str) -> Vec<String> {
    let mut ids = Vec::new();
    let dir = shared_media_state_dir();
    let entries = match fs::read_dir(&dir) {
        Ok(v) => v,
        Err(_) => return ids,
    };
    let library_prefix = format!("{}{}", sanitize_id(library_id), MEDIA_STATE_KEY_SEPARATOR);
    for entry in entries.flatten() {
        let name = entry.file_name();
        let name = name.to_string_lossy();
        if !name.ends_with(".slots") || !name.starts_with(&library_prefix) {
            continue;
        }
        let stem = name.trim_end_matches(".slots");
        let Some((_, drive_id)) = stem.split_once(MEDIA_STATE_KEY_SEPARATOR) else {
            continue;
        };
        if !drive_id.is_empty() {
            ids.push(drive_id.to_string());
        }
    }
    ids.sort_unstable();
    ids.dedup();
    ids
}

pub(crate) fn changer_drive_state_bindings(state: &TapeState) -> Vec<(u16, String, String)> {
    let media_state_key = media_state_key_for_state(state);
    let (library_id, drive_hint) = split_media_state_key(&media_state_key);
    let mut drive_ids = configured_changer_drive_ids();
    if drive_ids.is_empty() {
        drive_ids = discover_changer_drive_ids_from_slots(&library_id);
    }
    if drive_ids.is_empty() {
        if let Some(drive_id) = drive_hint {
            drive_ids.push(drive_id);
        } else {
            drive_ids.push(state.drive_id.clone());
        }
    }
    drive_ids.sort_unstable();
    drive_ids.dedup();

    let mut bindings = Vec::with_capacity(drive_ids.len());
    for (idx, drive_id) in drive_ids.into_iter().enumerate() {
        let Ok(offset) = u16::try_from(idx) else {
            break;
        };
        let addr = CHANGER_DT_START.saturating_add(offset);
        let state_key = if media_state_key.contains(MEDIA_STATE_KEY_SEPARATOR) {
            format!("{library_id}{MEDIA_STATE_KEY_SEPARATOR}{drive_id}")
        } else {
            media_state_key.clone()
        };
        bindings.push((addr, drive_id, state_key));
    }
    bindings
}

pub(crate) fn ensure_changer_drive_topology(state: &mut TapeState) {
    let bindings = changer_drive_state_bindings(state);
    if bindings.is_empty() {
        return;
    }
    let mut next_drives = BTreeMap::new();
    let mut next_sources = BTreeMap::new();
    for (addr, _, _) in &bindings {
        next_drives.insert(
            *addr,
            state.changer_drives.get(addr).cloned().unwrap_or(None),
        );
        next_sources.insert(
            *addr,
            state
                .changer_drive_sources
                .get(addr)
                .copied()
                .unwrap_or(None),
        );
    }
    state.changer_drives = next_drives;
    state.changer_drive_sources = next_sources;
}

pub(crate) fn changer_drive_count(state: &TapeState) -> u16 {
    let count = state.changer_drives.len();
    if count == 0 {
        return 1;
    }
    u16::try_from(count).unwrap_or(u16::MAX)
}

pub(crate) fn shared_media_state_path(serial_seed: &str) -> PathBuf {
    let key = sanitize_id(serial_seed);
    shared_media_state_dir().join(format!("{key}.state"))
}

pub(crate) fn shared_cartridge_metadata_path(cartridge_id: &str) -> PathBuf {
    let key = sanitize_id(cartridge_id);
    shared_media_state_dir().join(format!("cartridge_{key}.meta"))
}

#[derive(Debug, Clone, Copy, Default)]
pub(crate) struct SharedCartridgeMetadata {
    pub capacity_bytes: Option<u64>,
    pub used_bytes: Option<u64>,
}

pub(crate) fn read_shared_cartridge_metadata(
    cartridge_id: &str,
) -> io::Result<Option<SharedCartridgeMetadata>> {
    let path = shared_cartridge_metadata_path(cartridge_id);
    let raw = match fs::read_to_string(&path) {
        Ok(v) => v,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(err) => return Err(err),
    };
    let mut metadata = SharedCartridgeMetadata::default();
    for line in raw.lines() {
        let Some((key, value)) = line.split_once('=') else {
            continue;
        };
        let Ok(parsed) = value.trim().parse::<u64>() else {
            continue;
        };
        match key.trim() {
            "capacity_bytes" => metadata.capacity_bytes = Some(parsed),
            "used_bytes" => metadata.used_bytes = Some(parsed),
            _ => {}
        }
    }
    Ok(Some(metadata))
}

pub(crate) fn write_shared_cartridge_metadata(
    cartridge_id: &str,
    capacity_bytes: Option<u64>,
    used_bytes: u64,
) -> io::Result<()> {
    let path = shared_cartridge_metadata_path(cartridge_id);
    let dir = path
        .parent()
        .map(PathBuf::from)
        .unwrap_or_else(shared_media_state_dir);
    fs::create_dir_all(&dir)?;
    let tmp = path.with_extension("meta.tmp");
    let capacity = capacity_bytes.unwrap_or(0);
    let payload = format!(
        "cartridge_id={}\ncapacity_bytes={capacity}\nused_bytes={used_bytes}\n",
        cartridge_id.trim()
    );
    fs::write(&tmp, payload)?;
    fs::rename(tmp, path)?;
    Ok(())
}

pub(crate) fn apply_shared_cartridge_metadata(state: &mut TapeState) {
    let Some(cartridge_id) = state.cartridge_id.clone() else {
        return;
    };
    let metadata = match read_shared_cartridge_metadata(&cartridge_id) {
        Ok(v) => v,
        Err(err) => {
            eprintln!(
                "[cdb_sync] failed to read cartridge metadata drive_id={} cartridge={} error={err}",
                state.drive_id, cartridge_id
            );
            None
        }
    };
    if let Some(capacity_bytes) = metadata.and_then(|value| value.capacity_bytes) {
        apply_medium_capacity_override(state, capacity_bytes);
    }
}

pub(crate) fn sync_loaded_cartridge_usage_to_shared(state: &TapeState) {
    sync_loaded_cartridge_usage_to_shared_throttled(state, true);
}

#[derive(Debug, Clone)]
struct SyncedCartridgeUsage {
    cartridge_id: String,
    capacity_bytes: Option<u64>,
    used_bytes: u64,
}

fn usage_sync_cache() -> &'static Mutex<HashMap<String, SyncedCartridgeUsage>> {
    static CACHE: OnceLock<Mutex<HashMap<String, SyncedCartridgeUsage>>> = OnceLock::new();
    CACHE.get_or_init(|| Mutex::new(HashMap::new()))
}

fn lock_io_mutex<'a, T>(
    mutex: &'a Mutex<T>,
    name: &str,
) -> io::Result<std::sync::MutexGuard<'a, T>> {
    mutex
        .lock()
        .map_err(|_| io::Error::other(format!("{name} cache poisoned")))
}

fn usage_sync_interval_bytes() -> u64 {
    static INTERVAL: OnceLock<u64> = OnceLock::new();
    *INTERVAL.get_or_init(|| {
        env::var("HOLO_CARTRIDGE_USAGE_SYNC_EVERY_BYTES")
            .ok()
            .and_then(|raw| raw.trim().parse::<u64>().ok())
            .unwrap_or(64 * 1024 * 1024)
    })
}

fn sync_loaded_cartridge_usage_to_shared_throttled(state: &TapeState, force: bool) {
    if state.mount_state != crate::scsi_tape::state::MountState::Loaded {
        return;
    }
    let Some(cartridge_id) = state.cartridge_id.as_deref() else {
        return;
    };
    let mut guard = match lock_io_mutex(usage_sync_cache(), "cartridge usage sync") {
        Ok(guard) => guard,
        Err(err) => {
            eprintln!(
                "[cdb_sync] failed to lock cartridge usage cache drive_id={} cartridge={} error={err}",
                state.drive_id, cartridge_id
            );
            return;
        }
    };
    let existing = guard
        .get(&state.drive_id)
        .filter(|cached| cached.cartridge_id == cartridge_id)
        .cloned();
    if !force {
        if let Some(existing) = existing.as_ref() {
            let delta = state.eod_position.abs_diff(existing.used_bytes);
            if delta < usage_sync_interval_bytes() {
                return;
            }
        }
    }
    let existing_capacity = if let Some(existing) = existing {
        existing.capacity_bytes
    } else {
        match read_shared_cartridge_metadata(cartridge_id) {
            Ok(Some(metadata)) => metadata.capacity_bytes,
            Ok(None) => None,
            Err(err) => {
                eprintln!(
                    "[cdb_sync] failed to read cartridge metadata before usage sync drive_id={} cartridge={} error={err}",
                    state.drive_id, cartridge_id
                );
                None
            }
        }
    };
    let capacity = state.cartridge_capacity_bytes.or(existing_capacity);
    if let Err(err) = write_shared_cartridge_metadata(cartridge_id, capacity, state.eod_position) {
        eprintln!(
            "[cdb_sync] failed to persist cartridge usage drive_id={} cartridge={} error={err}",
            state.drive_id, cartridge_id
        );
        return;
    }
    guard.insert(
        state.drive_id.clone(),
        SyncedCartridgeUsage {
            cartridge_id: cartridge_id.to_string(),
            capacity_bytes: capacity,
            used_bytes: state.eod_position,
        },
    );
}

#[derive(Debug, Clone)]
struct CachedLoadedState {
    value: Option<String>,
    loaded_at: Instant,
}

fn shared_loaded_state_cache() -> &'static Mutex<HashMap<PathBuf, CachedLoadedState>> {
    static CACHE: OnceLock<Mutex<HashMap<PathBuf, CachedLoadedState>>> = OnceLock::new();
    CACHE.get_or_init(|| Mutex::new(HashMap::new()))
}

fn shared_loaded_state_ttl() -> Duration {
    let ms = env::var("HOLO_MEDIA_STATE_CACHE_MS")
        .ok()
        .and_then(|raw| raw.trim().parse::<u64>().ok())
        .unwrap_or(200);
    Duration::from_millis(ms)
}

pub(crate) fn shared_changer_slots_path(serial_seed: &str) -> PathBuf {
    let key = sanitize_id(serial_seed);
    shared_media_state_dir().join(format!("{key}.slots"))
}

pub(crate) fn read_shared_changer_slots(
    serial_seed: &str,
) -> io::Result<Option<Vec<Option<String>>>> {
    let path = shared_changer_slots_path(serial_seed);
    let raw = match fs::read_to_string(&path) {
        Ok(v) => v,
        Err(err) if err.kind() == io::ErrorKind::NotFound => {
            lock_io_mutex(shared_loaded_state_cache(), "media state")?.insert(
                path,
                CachedLoadedState {
                    value: None,
                    loaded_at: Instant::now(),
                },
            );
            return Ok(None);
        }
        Err(err) => return Err(err),
    };
    let mut slots = Vec::new();
    for line in raw.lines() {
        let trimmed = line.trim();
        if trimmed.is_empty() || trimmed == "-" {
            slots.push(None);
        } else {
            slots.push(Some(trimmed.to_string()));
        }
    }
    Ok(Some(slots))
}

pub(crate) fn write_shared_changer_slots(
    serial_seed: &str,
    slots: &[Option<String>],
) -> io::Result<()> {
    let path = shared_changer_slots_path(serial_seed);
    let dir = path
        .parent()
        .map(PathBuf::from)
        .unwrap_or_else(shared_media_state_dir);
    fs::create_dir_all(&dir)?;
    let tmp = path.with_extension("slots.tmp");
    let mut payload = String::new();
    for slot in slots {
        match slot {
            Some(label) if !label.trim().is_empty() => {
                payload.push_str(label.trim());
                payload.push('\n');
            }
            _ => payload.push_str("-\n"),
        }
    }
    fs::write(&tmp, payload)?;
    fs::rename(tmp, path)?;
    Ok(())
}

pub(crate) fn shared_changer_ie_path(serial_seed: &str) -> PathBuf {
    let key = sanitize_id(serial_seed);
    shared_media_state_dir().join(format!("{key}.ie"))
}

pub(crate) fn shared_changer_vault_path(serial_seed: &str) -> PathBuf {
    let key = sanitize_id(serial_seed);
    shared_media_state_dir().join(format!("{key}.vault"))
}

pub(crate) fn read_shared_changer_ie(serial_seed: &str) -> io::Result<Option<Vec<Option<String>>>> {
    let path = shared_changer_ie_path(serial_seed);
    let raw = match fs::read_to_string(&path) {
        Ok(v) => v,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(err) => return Err(err),
    };
    let mut ports = Vec::new();
    for line in raw.lines() {
        let trimmed = line.trim();
        if trimmed.is_empty() || trimmed == "-" {
            ports.push(None);
        } else {
            ports.push(Some(trimmed.to_string()));
        }
    }
    Ok(Some(ports))
}

pub(crate) fn read_shared_changer_vault(
    serial_seed: &str,
) -> io::Result<Option<Vec<Option<String>>>> {
    let path = shared_changer_vault_path(serial_seed);
    let raw = match fs::read_to_string(&path) {
        Ok(v) => v,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(err) => return Err(err),
    };
    let mut labels = Vec::new();
    for line in raw.lines() {
        let trimmed = line.trim();
        if trimmed.is_empty() || trimmed == "-" {
            continue;
        }
        labels.push(Some(trimmed.to_string()));
    }
    Ok(Some(labels))
}

pub(crate) fn write_shared_changer_ie(
    serial_seed: &str,
    ports: &[Option<String>],
) -> io::Result<()> {
    let path = shared_changer_ie_path(serial_seed);
    let dir = path
        .parent()
        .map(PathBuf::from)
        .unwrap_or_else(shared_media_state_dir);
    fs::create_dir_all(&dir)?;
    let tmp = path.with_extension("ie.tmp");
    let mut payload = String::new();
    for port in ports {
        match port {
            Some(label) if !label.trim().is_empty() => {
                payload.push_str(label.trim());
                payload.push('\n');
            }
            _ => payload.push_str("-\n"),
        }
    }
    fs::write(&tmp, payload)?;
    fs::rename(tmp, path)?;
    Ok(())
}

pub(crate) fn write_shared_changer_vault(
    serial_seed: &str,
    labels: &[Option<String>],
) -> io::Result<()> {
    let path = shared_changer_vault_path(serial_seed);
    let dir = path
        .parent()
        .map(PathBuf::from)
        .unwrap_or_else(shared_media_state_dir);
    fs::create_dir_all(&dir)?;
    let tmp = path.with_extension("vault.tmp");
    let mut payload = String::new();
    for label in labels.iter().flatten() {
        let trimmed = label.trim();
        if !trimmed.is_empty() {
            payload.push_str(trimmed);
            payload.push('\n');
        }
    }
    fs::write(&tmp, payload)?;
    fs::rename(tmp, path)?;
    Ok(())
}

pub(crate) fn sync_changer_slots_from_shared(state: &mut TapeState) {
    let media_state_key = media_state_key_for_state(state);
    let requested = match read_shared_changer_slots(&media_state_key) {
        Ok(v) => v,
        Err(err) => {
            eprintln!(
                "[cdb_sync] failed to read shared changer slots drive_id={} error={err}",
                state.drive_id
            );
            None
        }
    };
    let Some(slots) = requested else {
        return;
    };
    let requested_count = usize::max(slots.len(), 1);
    let mut next_slots = BTreeMap::new();
    for idx in 0..requested_count {
        let Ok(offset) = u16::try_from(idx) else {
            break;
        };
        let addr = CHANGER_ST_START + offset;
        let next = slots.get(idx).and_then(|value| value.as_ref()).cloned();
        next_slots.insert(addr, next);
    }
    if state.changer_slots != next_slots {
        state.changer_slots = next_slots;
        state.push_unit_attention(0x28, 0x00);
    }
}

pub(crate) fn sync_changer_ie_from_shared(state: &mut TapeState) {
    let media_state_key = media_state_key_for_state(state);
    let requested = match read_shared_changer_ie(&media_state_key) {
        Ok(v) => v,
        Err(err) => {
            eprintln!(
                "[cdb_sync] failed to read shared changer IE drive_id={} error={err}",
                state.drive_id
            );
            None
        }
    };
    let Some(ports) = requested else {
        return;
    };

    let requested_count = usize::max(ports.len(), 1);
    let mut next_ports = BTreeMap::new();
    let mut next_flags = BTreeMap::new();
    for idx in 0..requested_count {
        let Ok(offset) = u16::try_from(idx) else {
            break;
        };
        let addr = CHANGER_IE_START + offset;
        let next = ports.get(idx).and_then(|value| value.as_ref()).cloned();
        next_flags.insert(addr, next.is_some());
        next_ports.insert(addr, next);
    }
    if state.changer_ie_ports != next_ports || state.changer_ie_impexp != next_flags {
        state.changer_ie_ports = next_ports;
        state.changer_ie_impexp = next_flags;
        state.push_unit_attention(0x28, 0x01);
    }
}

pub(crate) fn sync_changer_inventory_from_shared(state: &mut TapeState) {
    sync_changer_slots_from_shared(state);
    sync_changer_ie_from_shared(state);
}

pub(crate) fn sync_changer_inventory_to_shared(state: &TapeState) {
    let media_state_key = media_state_key_for_state(state);
    if !media_state_key.contains(MEDIA_STATE_KEY_SEPARATOR)
        && !shared_changer_slots_path(&media_state_key).exists()
        && !shared_changer_ie_path(&media_state_key).exists()
    {
        return;
    }
    let slots = state
        .changer_slots
        .values()
        .cloned()
        .collect::<Vec<Option<String>>>();
    if let Err(err) = write_shared_changer_slots(&media_state_key, &slots) {
        eprintln!(
            "[cdb_sync] failed to persist changer slots drive_id={} key={} error={err}",
            state.drive_id, media_state_key
        );
    }
    let ie_ports = state
        .changer_ie_ports
        .values()
        .cloned()
        .collect::<Vec<Option<String>>>();
    if let Err(err) = write_shared_changer_ie(&media_state_key, &ie_ports) {
        eprintln!(
            "[cdb_sync] failed to persist changer IE drive_id={} key={} error={err}",
            state.drive_id, media_state_key
        );
    }
}

pub(crate) fn bootstrap_changer_slots_from_shared_once(state: &mut TapeState) {
    if state.changer_slots_synced_from_shared {
        return;
    }
    sync_changer_slots_from_shared(state);
    state.changer_slots_synced_from_shared = true;
}

pub(crate) fn changer_opcode_requires_slot_bootstrap(opcode: u8) -> bool {
    matches!(opcode, 0x1A | 0x5A | 0xA5 | 0xA6 | 0xB8)
}

pub(crate) fn read_shared_loaded_cartridge(serial_seed: &str) -> io::Result<Option<String>> {
    let path = shared_media_state_path(serial_seed);
    let ttl = shared_loaded_state_ttl();
    if !ttl.is_zero() {
        let guard = lock_io_mutex(shared_loaded_state_cache(), "media state")?;
        if let Some(cached) = guard.get(&path) {
            if cached.loaded_at.elapsed() <= ttl {
                return Ok(cached.value.clone());
            }
        }
    }
    read_shared_loaded_cartridge_from_path(path)
}

pub(crate) fn read_shared_loaded_cartridge_fresh(serial_seed: &str) -> io::Result<Option<String>> {
    read_shared_loaded_cartridge_from_path(shared_media_state_path(serial_seed))
}

fn read_shared_loaded_cartridge_from_path(path: PathBuf) -> io::Result<Option<String>> {
    let raw = match fs::read_to_string(&path) {
        Ok(v) => v,
        Err(err) if err.kind() == io::ErrorKind::NotFound => {
            lock_io_mutex(shared_loaded_state_cache(), "media state")?.insert(
                path,
                CachedLoadedState {
                    value: None,
                    loaded_at: Instant::now(),
                },
            );
            return Ok(None);
        }
        Err(err) => return Err(err),
    };
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        lock_io_mutex(shared_loaded_state_cache(), "media state")?.insert(
            path,
            CachedLoadedState {
                value: None,
                loaded_at: Instant::now(),
            },
        );
        return Ok(None);
    }
    if let Some(value) = trimmed.strip_prefix("cartridge=") {
        let value = value.trim();
        if value.is_empty() {
            return Ok(None);
        }
        let next = Some(value.to_string());
        lock_io_mutex(shared_loaded_state_cache(), "media state")?.insert(
            path,
            CachedLoadedState {
                value: next.clone(),
                loaded_at: Instant::now(),
            },
        );
        return Ok(next);
    }
    let next = Some(trimmed.to_string());
    lock_io_mutex(shared_loaded_state_cache(), "media state")?.insert(
        path,
        CachedLoadedState {
            value: next.clone(),
            loaded_at: Instant::now(),
        },
    );
    Ok(next)
}

pub(crate) fn write_shared_loaded_cartridge(
    serial_seed: &str,
    cartridge: Option<&str>,
) -> io::Result<()> {
    let path = shared_media_state_path(serial_seed);
    let dir = path
        .parent()
        .map(PathBuf::from)
        .unwrap_or_else(shared_media_state_dir);
    fs::create_dir_all(&dir)?;
    let tmp = path.with_extension("state.tmp");
    let payload = match cartridge {
        Some(value) if !value.trim().is_empty() => format!("cartridge={}\n", value.trim()),
        _ => "cartridge=\n".to_string(),
    };
    fs::write(&tmp, payload)?;
    fs::rename(tmp, &path)?;
    let cached = cartridge
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(ToOwned::to_owned);
    lock_io_mutex(shared_loaded_state_cache(), "media state")?.insert(
        path,
        CachedLoadedState {
            value: cached,
            loaded_at: Instant::now(),
        },
    );
    Ok(())
}

pub(crate) fn sync_changer_mount_to_shared(state: &TapeState) {
    let bindings = changer_drive_state_bindings(state);
    if bindings.is_empty() {
        return;
    }
    for (addr, _, state_key) in bindings {
        let cartridge = state
            .changer_drives
            .get(&addr)
            .and_then(|value| value.as_deref());
        if let Err(err) = write_shared_loaded_cartridge(&state_key, cartridge) {
            eprintln!(
                "[cdb_sync] failed to persist changer media state drive_id={} element=0x{addr:04X} key={} error={err}",
                state.drive_id, state_key
            );
        }
    }
}

pub(crate) fn sync_changer_mount_from_shared(state: &mut TapeState) {
    let media_state_key = media_state_key_for_state(state);
    if !media_state_key.contains(MEDIA_STATE_KEY_SEPARATOR) {
        return;
    }
    let bindings = changer_drive_state_bindings(state);
    if bindings.is_empty() {
        return;
    }
    let mut changed = false;
    for (addr, _, state_key) in bindings {
        let desired = match read_shared_loaded_cartridge_fresh(&state_key) {
            Ok(v) => v,
            Err(err) => {
                eprintln!(
                    "[cdb_sync] failed to read changer media state drive_id={} element=0x{addr:04X} key={} error={err}",
                    state.drive_id, state_key
                );
                continue;
            }
        };
        let current = state
            .changer_drives
            .get(&addr)
            .and_then(|value| value.as_ref())
            .cloned();
        if current == desired {
            continue;
        }

        match desired {
            Some(label) => {
                if let Some(current_label) = current.filter(|value| value != &label) {
                    let source_slot = state.changer_drive_sources.get(&addr).copied().flatten();
                    remove_changer_medium_label(state, &current_label, addr);
                    restore_changer_medium_label(state, current_label, source_slot);
                }
                let source_slot = remove_changer_medium_label(state, &label, addr);
                state.changer_drives.insert(addr, Some(label));
                state.changer_drive_sources.insert(addr, source_slot);
            }
            None => {
                if let Some(label) = current {
                    let source_slot = state.changer_drive_sources.get(&addr).copied().flatten();
                    restore_changer_medium_label(state, label, source_slot);
                }
                state.changer_drives.insert(addr, None);
                state.changer_drive_sources.insert(addr, None);
            }
        }
        changed = true;
    }
    if changed {
        state.push_unit_attention(0x28, 0x00);
    }
}

fn remove_changer_medium_label(
    state: &mut TapeState,
    label: &str,
    target_drive: u16,
) -> Option<u16> {
    for (slot, entry) in state.changer_slots.iter_mut() {
        if entry.as_deref() == Some(label) {
            *entry = None;
            return Some(*slot);
        }
    }
    for (slot, entry) in state.changer_ie_ports.iter_mut() {
        if entry.as_deref() == Some(label) {
            *entry = None;
            state.changer_ie_impexp.insert(*slot, false);
            return Some(*slot);
        }
    }
    let duplicate_drive = state.changer_drives.iter().find_map(|(addr, entry)| {
        if *addr != target_drive && entry.as_deref() == Some(label) {
            Some(*addr)
        } else {
            None
        }
    });
    if let Some(addr) = duplicate_drive {
        state.changer_drives.insert(addr, None);
        let source_slot = state.changer_drive_sources.get(&addr).copied().flatten();
        state.changer_drive_sources.insert(addr, None);
        return source_slot;
    }
    None
}

fn restore_changer_medium_label(state: &mut TapeState, label: String, source_slot: Option<u16>) {
    if let Some(slot) = source_slot {
        if let Some(entry) = state.changer_slots.get_mut(&slot) {
            if entry.is_none() {
                *entry = Some(label);
                return;
            }
        }
    }
    if let Some((_, entry)) = state
        .changer_slots
        .iter_mut()
        .find(|(_, entry)| entry.is_none())
    {
        *entry = Some(label);
    }
}

pub(crate) fn sync_drive_mount_from_shared(state: &mut TapeState) {
    let media_state_key = media_state_key_for_state(state);
    let desired = match read_shared_loaded_cartridge_fresh(&media_state_key) {
        Ok(v) => v,
        Err(err) => {
            eprintln!(
                "[cdb_sync] failed to read shared media state drive_id={} error={err}",
                state.drive_id
            );
            return;
        }
    };
    let current = state.cartridge_id.clone();
    match (current.as_deref(), desired.as_deref()) {
        (Some(cur), Some(next)) if cur == next => {}
        (Some(_), Some(next)) => {
            sync_loaded_cartridge_usage_to_shared(state);
            crate::media::mount_bridge::detach_cartridge(state);
            if let Err(err) = crate::media::mount_bridge::attach_cartridge(state, next) {
                eprintln!(
                    "[cdb_sync] failed to switch mounted cartridge drive_id={} cartridge={} error={err}",
                    state.drive_id, next
                );
            } else {
                apply_shared_cartridge_metadata(state);
                sync_loaded_cartridge_usage_to_shared(state);
                state.push_unit_attention(0x28, 0x00);
            }
        }
        (None, Some(next)) => {
            if let Err(err) = crate::media::mount_bridge::attach_cartridge(state, next) {
                eprintln!(
                    "[cdb_sync] failed to mount cartridge from shared state drive_id={} cartridge={} error={err}",
                    state.drive_id, next
                );
            } else {
                apply_shared_cartridge_metadata(state);
                sync_loaded_cartridge_usage_to_shared(state);
                state.push_unit_attention(0x28, 0x00);
            }
        }
        (Some(_), None) => {
            sync_loaded_cartridge_usage_to_shared(state);
            crate::media::mount_bridge::detach_cartridge(state);
            state.push_unit_attention(0x28, 0x00);
        }
        (None, None) => {}
    }
}

pub(crate) fn trace_cdb_if_enabled(
    role: &str,
    cdb: &[u8],
    data_out: &[u8],
    response: &CdbResponse,
) {
    if !scsi_trace_enabled() || cdb.is_empty() {
        return;
    }
    let data_out_checksum = checksum32(data_out);
    let data_in_checksum = checksum32(&response.reply);
    let read_attribute_trace = read_attribute_trace_suffix(cdb, &response.reply);
    eprintln!(
        "[cdb_trace] role={role} opcode=0x{opcode:02X} status=0x{status:02X} data_out_len={data_out_len} data_in_len={data_in_len} data_out_checksum=0x{data_out_checksum:08X} data_in_checksum=0x{data_in_checksum:08X} cdb=[{cdb_hex}] data_out_preview=[{data_out_hex}] data_preview=[{data_hex}] sense=[{sense_hex}]{read_attribute_trace}",
        opcode = cdb[0],
        status = response.status,
        data_out_len = data_out.len(),
        data_in_len = response.reply.len(),
        data_out_checksum = data_out_checksum,
        data_in_checksum = data_in_checksum,
        cdb_hex = bytes_to_hex(cdb, 32),
        data_out_hex = bytes_to_hex(data_out, 8),
        data_hex = bytes_to_hex(&response.reply, 8),
        sense_hex = bytes_to_hex(&response.sense, 18),
        read_attribute_trace = read_attribute_trace,
    );
}

pub(crate) fn read_attribute_trace_suffix(cdb: &[u8], payload: &[u8]) -> String {
    if cdb.first().copied() != Some(0x8C) || payload.len() < 4 {
        return String::new();
    }
    let declared_len = u32::from_be_bytes([payload[0], payload[1], payload[2], payload[3]]);
    let mut offset = 4usize;
    let mut entries = Vec::new();
    while offset < payload.len() {
        let remaining = payload.len() - offset;
        if remaining < 5 {
            entries.push(format!("truncated_header_at={offset} remaining={remaining}"));
            break;
        }
        let id = u16::from_be_bytes([payload[offset], payload[offset + 1]]);
        let format = payload[offset + 2];
        let value_len = u16::from_be_bytes([payload[offset + 3], payload[offset + 4]]) as usize;
        offset += 5;
        if payload.len().saturating_sub(offset) < value_len {
            entries.push(format!(
                "id=0x{id:04X} fmt=0x{format:02X} len={value_len} truncated_value_remaining={}",
                payload.len().saturating_sub(offset)
            ));
            break;
        }
        let value = &payload[offset..offset + value_len];
        entries.push(format!(
            "id=0x{id:04X} fmt=0x{format:02X} len={value_len} hex=[{}] ascii=\"{}\"",
            bytes_to_hex(value, value.len()),
            trace_ascii(value)
        ));
        offset += value_len;
    }
    format!(" read_attr_declared_len={declared_len} read_attr=[{}]", entries.join("; "))
}

pub(crate) fn trace_ascii(bytes: &[u8]) -> String {
    bytes
        .iter()
        .map(|&b| match b {
            b' '..=b'~' => b as char,
            _ => '.',
        })
        .collect()
}

pub(crate) fn projected_write_bytes(state: &TapeState, cdb: &[u8], data_out: &[u8]) -> u64 {
    if state.block_mode.mode != crate::scsi_tape::state::BlockMode::Fixed {
        return data_out.len() as u64;
    }
    let block_size = state.block_mode.fixed_block_size as u64;
    if block_size == 0 {
        return data_out.len() as u64;
    }
    let transfer_blocks = match cdb.first().copied().unwrap_or(0) {
        0x0A => {
            let raw = cdb.get(4).copied().unwrap_or(0) as u64;
            if raw == 0 {
                256
            } else {
                raw
            }
        }
        0x2A => read_be16(cdb, 7) as u64,
        0xAA => read_be32(cdb, 6) as u64,
        0x8A => read_be32(cdb, 10) as u64,
        _ => 0,
    };
    if transfer_blocks == 0 {
        data_out.len() as u64
    } else {
        transfer_blocks.saturating_mul(block_size)
    }
}

pub(crate) const CHANGER_MT_START: u16 = 0x0000;
pub(crate) const CHANGER_MT_COUNT: u16 = 1;
pub(crate) const CHANGER_DT_START: u16 = 0x0100;
pub(crate) const CHANGER_IE_START: u16 = 0x0300;
pub(crate) const CHANGER_ST_START: u16 = 0x0400;

pub(crate) fn read_be24(input: &[u8], offset: usize) -> usize {
    try_read_be24(input, offset).unwrap_or(0)
}

pub(crate) fn try_read_be24(input: &[u8], offset: usize) -> Option<usize> {
    if input.len() < offset + 3 {
        return None;
    }
    Some(
        ((input[offset] as usize) << 16)
            | ((input[offset + 1] as usize) << 8)
            | (input[offset + 2] as usize),
    )
}

pub(crate) fn read_be16(input: &[u8], offset: usize) -> usize {
    try_read_be16(input, offset).unwrap_or(0)
}

pub(crate) fn try_read_be16(input: &[u8], offset: usize) -> Option<usize> {
    if input.len() < offset + 2 {
        return None;
    }
    Some(u16::from_be_bytes([input[offset], input[offset + 1]]) as usize)
}

pub(crate) fn read_be32(input: &[u8], offset: usize) -> usize {
    try_read_be32(input, offset).unwrap_or(0)
}

pub(crate) fn try_read_be32(input: &[u8], offset: usize) -> Option<usize> {
    if input.len() < offset + 4 {
        return None;
    }
    Some(u32::from_be_bytes([
        input[offset],
        input[offset + 1],
        input[offset + 2],
        input[offset + 3],
    ]) as usize)
}

pub(crate) fn read_be64(input: &[u8], offset: usize) -> Option<u64> {
    if input.len() < offset + 8 {
        return None;
    }
    Some(u64::from_be_bytes([
        input[offset],
        input[offset + 1],
        input[offset + 2],
        input[offset + 3],
        input[offset + 4],
        input[offset + 5],
        input[offset + 6],
        input[offset + 7],
    ]))
}

pub(crate) const MODE_PAGE_RW_ERROR_RECOVERY: u8 = 0x01;
pub(crate) const MODE_PAGE_DISCONNECT_RECONNECT: u8 = 0x02;
pub(crate) const MODE_PAGE_CACHING: u8 = 0x08;
pub(crate) const MODE_PAGE_CONTROL_MODE: u8 = 0x0A;
pub(crate) const MODE_PAGE_DATA_COMPRESSION: u8 = 0x0F;
pub(crate) const MODE_PAGE_DEVICE_CONFIGURATION: u8 = 0x10;
pub(crate) const MODE_PAGE_MEDIUM_PARTITION: u8 = 0x11;
pub(crate) const MODE_PAGE_INFORMATION_EXCEPTION: u8 = 0x1C;
pub(crate) const MODE_PAGE_MEDIUM_CONFIGURATION: u8 = 0x1D;
pub(crate) const MODE_PAGE_ALL: u8 = 0x3F;
pub(crate) const MODE_SUBPAGE_CONTROL_DATA_PROTECTION: u8 = 0xF0;
pub(crate) const MODE_SUBPAGE_DEVICE_CONFIGURATION_EXTENSION: u8 = 0x01;
pub(crate) const MODE_SENSE_PC_SAVED_VALUES: u8 = 0x03;

pub(crate) const DRIVE_DEFAULT_BLOCK_LENGTH: u32 = 256 * 1024;
pub(crate) const DRIVE_MIN_BLOCK_SIZE: u16 = 1;
pub(crate) const DRIVE_REPORTED_MAX_BLOCK_SIZE: u32 = 0x00FF_FFFF;
pub(crate) const DRIVE_MAX_BLOCK_SIZE: u32 = 8 * 1024 * 1024;
pub(crate) const READ_ATTRIBUTE_ACTION_ATTRIBUTES: u8 = 0x00;
pub(crate) const READ_ATTRIBUTE_ACTION_LIST: u8 = 0x01;
pub(crate) const READ_POSITION_SERVICE_ACTION_SHORT: u8 = 0x00;
pub(crate) const READ_POSITION_SERVICE_ACTION_LONG: u8 = 0x06;
pub(crate) const READ_POSITION_SERVICE_ACTION_EXTENDED: u8 = 0x08;

/// Decode standard tape CDB opcodes to CoreCommand variants.
/// Returns `None` for unrecognised opcodes.
pub(crate) fn decode_cdb_to_command(
    state: &TapeState,
    opcode: u8,
    cdb: &[u8],
    data_out: &[u8],
) -> Option<CoreCommand> {
    let profile = crate::scsi_tape::profiles::resolve_drive_profile_from_env();
    if opcode == 0x12 {
        // INQUIRY
        let evpd = cdb.get(1).copied().unwrap_or(0) & 0x01;
        let page_code = cdb.get(2).copied().unwrap_or(0);
        if evpd == 0 {
            return Some(CoreCommand::InquiryStandard {
                profile,
                serial_seed: state.drive_id.clone(),
            });
        }
        return Some(CoreCommand::InquiryVpd {
            profile,
            page_code,
            // Keep serial stable per exported drive/publication so host
            // device cache and pathing logic do not collapse identities.
            serial_seed: state.drive_id.clone(),
        });
    }

    match opcode {
        // REWIND (0x01)
        0x01 => Some(CoreCommand::Rewind),
        // READ (0x08) — variable block, transfer length from bytes 2-4
        0x08 => Some(CoreCommand::ReadData),
        // WRITE (0x0A) — transfer data-out as payload
        0x0A => Some(CoreCommand::WriteData {
            payload: data_out.to_vec(),
        }),
        // READ (16)
        0x88 => Some(CoreCommand::ReadData),
        // WRITE (16)
        0x8A => Some(CoreCommand::WriteData {
            payload: data_out.to_vec(),
        }),
        // WRITE FILEMARKS (0x10)
        0x10 => {
            let count = if cdb.len() >= 5 {
                u32::from_be_bytes([0, cdb[2], cdb[3], cdb[4]])
            } else {
                1
            };
            Some(CoreCommand::WriteFilemarks { count })
        }
        // SPACE (0x11)
        0x11 => {
            if cdb.len() < 6 {
                return None;
            }
            let code = cdb[1] & 0x07;
            let count = sign_extend_be24([cdb[2], cdb[3], cdb[4]]);
            match code {
                0x00 => Some(CoreCommand::SpaceBlocks { count }),
                0x01 => Some(CoreCommand::SpaceFilemarks { count }),
                0x03 => Some(CoreCommand::SpaceEndOfData { count }),
                _ => None,
            }
        }
        // ERASE (0x19)
        0x19 => {
            let mode = if cdb.get(1).copied().unwrap_or(0) & 0x01 != 0 {
                EraseMode::Long
            } else {
                EraseMode::Short
            };
            Some(CoreCommand::Erase { mode })
        }
        // MODE SENSE (6) (0x1A)
        0x1A => {
            let page_code = cdb.get(2).copied().unwrap_or(0) & 0x3F;
            Some(CoreCommand::ModeSense { page_code })
        }
        // LOCATE (0x2B)
        0x2B => {
            if cdb.len() < 9 {
                return None;
            }
            let logical_block = u64::from(u32::from_be_bytes([cdb[3], cdb[4], cdb[5], cdb[6]]));
            Some(CoreCommand::Locate {
                logical_block: transport_logical_to_internal_offset(state, logical_block),
            })
        }
        // READ POSITION (0x34)
        0x34 => Some(CoreCommand::ReadPosition),
        // LOG SENSE (0x4D)
        0x4D => {
            let page_code = cdb.get(2).copied().unwrap_or(0) & 0x3F;
            Some(CoreCommand::LogSense { page_code })
        }
        _ => None,
    }
}

/// Convert a `CoreResponse` to a flat byte vector for the IPC reply payload.
pub(crate) fn core_response_to_bytes(
    state: &TapeState,
    response: CoreResponse,
    cdb: &[u8],
) -> Vec<u8> {
    match response {
        CoreResponse::None => vec![],
        CoreResponse::Bytes(b) => b,
        CoreResponse::Data(d) => d,
        CoreResponse::ElementStatus(entries) => {
            // Flatten element status entries: [class_byte, start_addr_be(2), count_be(2)]
            let mut out = Vec::with_capacity(entries.len() * 5);
            for entry in &entries {
                let class_byte = match entry.class {
                    crate::scsi_tape::identity::ElementClass::Drive => 0x01u8,
                    crate::scsi_tape::identity::ElementClass::Slot => 0x02u8,
                    crate::scsi_tape::identity::ElementClass::ImportExport => 0x03u8,
                };
                out.push(class_byte);
                out.extend_from_slice(&entry.start_addr.to_be_bytes());
                out.extend_from_slice(&entry.count.to_be_bytes());
            }
            out
        }
        CoreResponse::Mam(record) => {
            use crate::scsi_tape::commands_core::encode_mam_response;
            encode_mam_response(&record)
        }
        CoreResponse::Position(report) => read_position_response_bytes(
            state,
            cdb,
            &report,
            state.partition_runtime.active_partition,
        ),
        CoreResponse::Reservation(_) => vec![],
    }
}

pub(crate) fn read_position_response_bytes(
    state: &TapeState,
    cdb: &[u8],
    report: &crate::scsi_tape::command_chain::PositionReport,
    active_partition: u8,
) -> Vec<u8> {
    let reported_position = read_position_reported_position(transport_internal_offset_to_logical(
        state,
        report.current_position,
    ));
    let service_action = cdb.get(1).copied().unwrap_or(0) & 0x1F;
    let mut out = match service_action {
        READ_POSITION_SERVICE_ACTION_LONG => {
            read_position_long_response(report, active_partition, reported_position)
        }
        READ_POSITION_SERVICE_ACTION_EXTENDED => {
            read_position_extended_response(report, active_partition, reported_position)
        }
        _ => read_position_short_response(report, active_partition, reported_position),
    };
    let allocation_length = read_position_allocation_length(cdb, service_action);
    if out.len() > allocation_length {
        out.truncate(allocation_length);
    }
    out
}

pub(crate) fn read_position_allocation_length(cdb: &[u8], service_action: u8) -> usize {
    match service_action {
        READ_POSITION_SERVICE_ACTION_SHORT => 20,
        READ_POSITION_SERVICE_ACTION_LONG => 32,
        READ_POSITION_SERVICE_ACTION_EXTENDED => usize::min(32, read_be16(cdb, 7)),
        _ => 20,
    }
}

pub(crate) fn read_position_flags(report: &crate::scsi_tape::command_chain::PositionReport) -> u8 {
    let mut flags = 0u8;
    // At BOT, report BOP=1 with block_number=0 per SSC-3 convention.
    if report.current_position == 0 {
        flags |= 0x80; // BOP
    }
    if report.early_warning {
        flags |= 0x40; // EOP
    }
    flags
}

pub(crate) fn read_position_short_response(
    report: &crate::scsi_tape::command_chain::PositionReport,
    active_partition: u8,
    reported_position: u64,
) -> Vec<u8> {
    let mut out = vec![0u8; 20];
    out[0] = read_position_flags(report);
    out[1] = active_partition;
    let pos32 = reported_position.min(u32::MAX as u64) as u32;
    let blocks_in_buffer = 0u32;
    let last_pos32 = pos32.saturating_sub(blocks_in_buffer);
    out[4..8].copy_from_slice(&pos32.to_be_bytes());
    out[8..12].copy_from_slice(&last_pos32.to_be_bytes());
    out[13..16].copy_from_slice(&blocks_in_buffer.to_be_bytes()[1..4]);
    out[16..20].copy_from_slice(&0u32.to_be_bytes());
    out
}

pub(crate) fn read_position_long_response(
    report: &crate::scsi_tape::command_chain::PositionReport,
    active_partition: u8,
    reported_position: u64,
) -> Vec<u8> {
    let mut out = vec![0u8; 32];
    out[0] = read_position_flags(report);
    out[4..8].copy_from_slice(&(active_partition as u32).to_be_bytes());
    out[8..16].copy_from_slice(&reported_position.to_be_bytes());
    out[16..24].copy_from_slice(&report.file_number.to_be_bytes());
    out[24..32].copy_from_slice(&report.set_number.to_be_bytes());
    out
}

pub(crate) fn read_position_extended_response(
    report: &crate::scsi_tape::command_chain::PositionReport,
    active_partition: u8,
    reported_position: u64,
) -> Vec<u8> {
    let mut out = vec![0u8; 32];
    out[0] = read_position_flags(report);
    out[1] = active_partition;
    out[2..4].copy_from_slice(&0x001Cu16.to_be_bytes()); // additional length
    let blocks_in_buffer = 0u32;
    out[5..8].copy_from_slice(&blocks_in_buffer.to_be_bytes()[1..4]);
    out[8..16].copy_from_slice(&reported_position.to_be_bytes());
    out[16..24].copy_from_slice(
        &reported_position
            .saturating_sub(blocks_in_buffer as u64)
            .to_be_bytes(),
    );
    out[24..32].copy_from_slice(&0u64.to_be_bytes());
    out
}

pub(crate) fn sign_extend_be24(bytes: [u8; 3]) -> i64 {
    let count_raw = i32::from_be_bytes([0, bytes[0], bytes[1], bytes[2]]);
    if bytes[0] & 0x80 != 0 {
        (count_raw | (-1i32 << 24)) as i64
    } else {
        count_raw as i64
    }
}

pub(crate) fn fixed_block_size_bytes(state: &TapeState) -> Option<u64> {
    match state.block_mode.mode {
        crate::scsi_tape::state::BlockMode::Fixed if state.block_mode.fixed_block_size > 0 => {
            Some(state.block_mode.fixed_block_size as u64)
        }
        _ => None,
    }
}

fn variable_block_address_points(state: &TapeState) -> Vec<u64> {
    let mut points = state.block_starts.clone();
    points.extend(state.filemarks.iter().copied());
    points.sort_unstable();
    points.dedup();
    points
}

pub(crate) fn transport_logical_to_internal_offset(state: &TapeState, logical_block: u64) -> u64 {
    if let Some(block_size) = fixed_block_size_bytes(state) {
        logical_block.saturating_mul(block_size)
    } else {
        let points = variable_block_address_points(state);
        let index = logical_block as usize;
        if index < points.len() {
            points[index]
        } else if index == points.len() {
            state.eod_position
        } else {
            state.eod_position.saturating_add(1)
        }
    }
}

pub(crate) fn transport_internal_offset_to_logical(
    state: &TapeState,
    current_position: u64,
) -> u64 {
    if let Some(block_size) = fixed_block_size_bytes(state) {
        current_position / block_size
    } else {
        let points = variable_block_address_points(state);
        if current_position >= state.eod_position {
            points.len() as u64
        } else {
            points.partition_point(|point| *point < current_position) as u64
        }
    }
}

pub(crate) fn read_position_reported_position(current_position: u64) -> u64 {
    current_position
}

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

#[cfg(test)]
include!("cdb_server_tests.rs");
