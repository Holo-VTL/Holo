use crate::scsi_tape::command_chain::{
    clear_reservation, configured_initiator, erase_media, load_media, locate, log_sense,
    mode_sense, preempt_reservation, read_data, read_position, read_reservation,
    register_and_move_reservation, register_ignore_reservation, register_reservation,
    release_reservation, reserve_reservation, rewind_media, set_block_mode_fixed,
    set_block_mode_variable, set_worm_policy, space, unload_media, write_data, write_filemarks,
    EraseMode, PositionReport, ReservationReport, SpaceBranch,
};
use crate::scsi_tape::error::TapeError;
use crate::scsi_tape::identity::{
    element_status_entries, standard_inquiry_bytes, vpd_page_bytes, DeviceIdentityProfile,
    ElementAddressProfile, ElementStatusEntry,
};
use crate::scsi_tape::mam::{build_mam_baseline, encode_mam_baseline, MamBaselineRecord};
use crate::scsi_tape::sense::{resolve_sense_for_error, SenseFrame};
use crate::scsi_tape::state::TapeState;
use crate::telemetry::events::{
    scsi_pr_conflict_event, scsi_pr_lifecycle_event, scsi_space_branch_event,
    scsi_unsupported_log_page_event, scsi_unsupported_mode_page_event, scsi_worm_block_event,
};

#[derive(Debug, Clone)]
pub enum CoreCommand {
    InquiryStandard {
        profile: DeviceIdentityProfile,
        serial_seed: String,
    },
    InquiryVpd {
        profile: DeviceIdentityProfile,
        page_code: u8,
        serial_seed: String,
    },
    ReadElementStatus {
        profile: ElementAddressProfile,
        drive_serials: Vec<String>,
    },
    MamBaseline {
        barcode: String,
        media_type: String,
        capacity_bytes: u64,
        remaining_bytes: u64,
    },
    Load {
        cartridge_id: String,
    },
    Unload,
    Rewind,
    Erase {
        mode: EraseMode,
    },
    EraseAs {
        initiator: String,
        mode: EraseMode,
    },
    WriteData {
        payload: Vec<u8>,
    },
    WriteDataAs {
        initiator: String,
        payload: Vec<u8>,
    },
    ReadData,
    WriteFilemarks {
        count: u32,
    },
    WriteFilemarksAs {
        initiator: String,
        count: u32,
    },
    SpaceBlocks {
        count: i64,
    },
    SpaceFilemarks {
        count: i64,
    },
    SpaceEndOfData {
        count: i64,
    },
    SetBlockModeFixed {
        block_size: u32,
    },
    SetBlockModeVariable,
    ModeSense {
        page_code: u8,
    },
    LogSense {
        page_code: u8,
    },
    SetWormPolicy {
        is_worm_media: bool,
        retention_locked: bool,
    },
    ReservationRegister {
        initiator: String,
        key: u64,
    },
    ReservationRegisterIgnore {
        initiator: String,
        service_key: u64,
    },
    ReservationReserve {
        initiator: String,
        key: u64,
    },
    ReservationRelease {
        initiator: String,
        key: u64,
    },
    ReservationClear {
        initiator: String,
        key: u64,
    },
    ReservationPreempt {
        initiator: String,
        key: u64,
        service_key: u64,
    },
    ReservationRegisterMove {
        initiator: String,
        key: u64,
        service_key: u64,
        target_initiator: String,
        unregister_source: bool,
    },
    ReservationRead,
    ReadPosition,
    Locate {
        logical_block: u64,
    },
    // Legacy aliases kept for compatibility with early skeleton callers.
    Mount,
    Unmount,
    Read,
    Write,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum CoreResponse {
    None,
    Bytes(Vec<u8>),
    Data(Vec<u8>),
    ElementStatus(Vec<ElementStatusEntry>),
    Mam(MamBaselineRecord),
    Position(PositionReport),
    Reservation(ReservationReport),
}

pub fn execute(state: &mut TapeState, cmd: CoreCommand) -> Result<CoreResponse, TapeError> {
    match cmd {
        CoreCommand::InquiryStandard {
            profile,
            serial_seed,
        } => {
            let payload = standard_inquiry_bytes(&profile, &serial_seed)?;
            Ok(CoreResponse::Bytes(payload))
        }
        CoreCommand::InquiryVpd {
            profile,
            page_code,
            serial_seed,
        } => {
            let payload = vpd_page_bytes(&profile, page_code, &serial_seed)?;
            Ok(CoreResponse::Bytes(payload))
        }
        CoreCommand::ReadElementStatus {
            profile,
            drive_serials,
        } => {
            let entries = element_status_entries(&profile, &drive_serials)?;
            Ok(CoreResponse::ElementStatus(entries))
        }
        CoreCommand::MamBaseline {
            barcode,
            media_type,
            capacity_bytes,
            remaining_bytes,
        } => {
            let record =
                build_mam_baseline(state, barcode, media_type, capacity_bytes, remaining_bytes)?;
            Ok(CoreResponse::Mam(record))
        }
        CoreCommand::Load { cartridge_id } => {
            load_media(state, &cartridge_id)?;
            Ok(CoreResponse::None)
        }
        CoreCommand::Unload => {
            unload_media(state)?;
            Ok(CoreResponse::None)
        }
        CoreCommand::Rewind => {
            rewind_media(state)?;
            Ok(CoreResponse::None)
        }
        CoreCommand::Erase { mode } => {
            erase_media(state, mode, Some(configured_initiator()))?;
            Ok(CoreResponse::None)
        }
        CoreCommand::EraseAs { initiator, mode } => {
            erase_media(state, mode, Some(&initiator))?;
            Ok(CoreResponse::None)
        }
        CoreCommand::WriteData { payload } => {
            write_data(state, &payload, Some(configured_initiator()))?;
            Ok(CoreResponse::None)
        }
        CoreCommand::WriteDataAs { initiator, payload } => {
            write_data(state, &payload, Some(&initiator))?;
            Ok(CoreResponse::None)
        }
        CoreCommand::ReadData => {
            let payload = read_data(state)?;
            Ok(CoreResponse::Data(payload))
        }
        CoreCommand::WriteFilemarks { count } => {
            write_filemarks(state, count, Some(configured_initiator()))?;
            Ok(CoreResponse::None)
        }
        CoreCommand::WriteFilemarksAs { initiator, count } => {
            write_filemarks(state, count, Some(&initiator))?;
            Ok(CoreResponse::None)
        }
        CoreCommand::SpaceBlocks { count } => {
            space(state, SpaceBranch::Blocks, count)?;
            let _ = scsi_space_branch_event("blocks");
            Ok(CoreResponse::None)
        }
        CoreCommand::SpaceFilemarks { count } => {
            space(state, SpaceBranch::Filemarks, count)?;
            let _ = scsi_space_branch_event("filemarks");
            Ok(CoreResponse::None)
        }
        CoreCommand::SpaceEndOfData { count } => {
            space(state, SpaceBranch::EndOfData, count)?;
            let _ = scsi_space_branch_event("eod");
            Ok(CoreResponse::None)
        }
        CoreCommand::SetBlockModeFixed { block_size } => {
            set_block_mode_fixed(state, block_size)?;
            Ok(CoreResponse::None)
        }
        CoreCommand::SetBlockModeVariable => {
            set_block_mode_variable(state)?;
            Ok(CoreResponse::None)
        }
        CoreCommand::ModeSense { page_code } => {
            let payload = mode_sense(state, page_code)?;
            Ok(CoreResponse::Bytes(payload))
        }
        CoreCommand::LogSense { page_code } => {
            let payload = log_sense(state, page_code)?;
            Ok(CoreResponse::Bytes(payload))
        }
        CoreCommand::SetWormPolicy {
            is_worm_media,
            retention_locked,
        } => {
            set_worm_policy(state, is_worm_media, retention_locked)?;
            Ok(CoreResponse::None)
        }
        CoreCommand::ReservationRegister { initiator, key } => {
            register_reservation(state, Some(&initiator), key)?;
            let _ = scsi_pr_lifecycle_event("pr_register");
            Ok(CoreResponse::None)
        }
        CoreCommand::ReservationRegisterIgnore {
            initiator,
            service_key,
        } => {
            register_ignore_reservation(state, Some(&initiator), service_key)?;
            let _ = scsi_pr_lifecycle_event("pr_register_ignore");
            Ok(CoreResponse::None)
        }
        CoreCommand::ReservationReserve { initiator, key } => {
            reserve_reservation(state, Some(&initiator), key)?;
            let _ = scsi_pr_lifecycle_event("pr_reserve");
            Ok(CoreResponse::None)
        }
        CoreCommand::ReservationRelease { initiator, key } => {
            release_reservation(state, Some(&initiator), key)?;
            let _ = scsi_pr_lifecycle_event("pr_release");
            Ok(CoreResponse::None)
        }
        CoreCommand::ReservationClear { initiator, key } => {
            clear_reservation(state, Some(&initiator), key)?;
            let _ = scsi_pr_lifecycle_event("pr_clear");
            Ok(CoreResponse::None)
        }
        CoreCommand::ReservationPreempt {
            initiator,
            key,
            service_key,
        } => {
            preempt_reservation(state, Some(&initiator), key, service_key)?;
            let _ = scsi_pr_lifecycle_event("pr_preempt");
            Ok(CoreResponse::None)
        }
        CoreCommand::ReservationRegisterMove {
            initiator,
            key,
            service_key,
            target_initiator,
            unregister_source,
        } => {
            register_and_move_reservation(
                state,
                Some(&initiator),
                key,
                service_key,
                &target_initiator,
                unregister_source,
            )?;
            let _ = scsi_pr_lifecycle_event("pr_register_move");
            Ok(CoreResponse::None)
        }
        CoreCommand::ReservationRead => {
            let report = read_reservation(state)?;
            Ok(CoreResponse::Reservation(report))
        }
        CoreCommand::ReadPosition => {
            let report = read_position(state)?;
            Ok(CoreResponse::Position(report))
        }
        CoreCommand::Locate { logical_block } => {
            locate(state, logical_block)?;
            Ok(CoreResponse::None)
        }
        CoreCommand::Mount => {
            load_media(state, "auto")?;
            Ok(CoreResponse::None)
        }
        CoreCommand::Unmount => {
            unload_media(state)?;
            Ok(CoreResponse::None)
        }
        CoreCommand::Read => {
            let payload = read_data(state)?;
            Ok(CoreResponse::Data(payload))
        }
        CoreCommand::Write => Err(TapeError::InvalidArgument(
            "legacy WRITE requires payload; use WriteData".to_string(),
        )),
    }
}

pub fn execute_with_sense(
    state: &mut TapeState,
    cmd: CoreCommand,
) -> Result<CoreResponse, SenseFrame> {
    execute(state, cmd).map_err(|error| tape_error_to_sense(&error))
}

pub fn tape_error_to_sense(error: &TapeError) -> SenseFrame {
    if crate::iscsi::cdb_server::scsi_trace_enabled() {
        eprintln!("[cdb_error] {:?}", error);
    }
    match error {
        TapeError::WormWriteProtected => {
            let _ = scsi_worm_block_event();
        }
        TapeError::ReservationConflict(_) => {
            let _ = scsi_pr_conflict_event();
        }
        TapeError::UnsupportedModePage(page_code) => {
            let _ = scsi_unsupported_mode_page_event(*page_code);
        }
        TapeError::UnsupportedLogPage(page_code) => {
            let _ = scsi_unsupported_log_page_event(*page_code);
        }
        _ => {}
    }
    resolve_sense_for_error(error)
}

pub fn encode_mam_response(record: &MamBaselineRecord) -> Vec<u8> {
    encode_mam_baseline(record)
}
