//! Dispatch-facing view over the CDB server composition.
//!
//! `cdb_server` remains the behavior-preserving entrypoint for now.
//! This module centralizes the dispatch API and shared gate helpers so
//! further decomposition can happen without touching call sites again.
#![allow(unused_imports)]

pub use crate::iscsi::cdb_server::{dispatch_raw_cdb, sense_frame_to_bytes};

pub(crate) use crate::iscsi::cdb_server::{
    bytes_to_hex, expected_cdb_length, is_data_write_opcode, is_legacy_empty_write_probe,
    is_legacy_single_byte_write_probe, projected_write_bytes, read_be16, read_be24, read_be32,
    read_be64, scsi_trace_enabled, trace_cdb_if_enabled, try_read_be16, try_read_be24,
    try_read_be32, validate_cdb_structure,
};
