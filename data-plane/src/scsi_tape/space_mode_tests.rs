use super::commands_core::{execute, execute_with_sense, CoreCommand, CoreResponse};
use super::state::TapeState;

fn new_state(case: &str) -> TapeState {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .expect("clock should be valid")
        .as_nanos();
    TapeState::new(format!("drive-{case}-{nanos}"))
}

fn cleanup(state: &TapeState) {
    if let Some(layout) = &state.active_layout {
        let _ = std::fs::remove_dir_all(&layout.root);
    }
}

#[test]
fn space_branches_and_early_warning_are_deterministic() {
    let mut state = new_state("space-branches");

    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-space-branches".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeFixed { block_size: 4 }).expect("fixed mode");
    // Keep this test deterministic with capacity-based EW calculation.
    state.partition_runtime.partition_sizes_bytes[0] = 16;
    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"ABCD".to_vec(),
        },
    )
    .expect("write block A should pass");
    execute(&mut state, CoreCommand::WriteFilemarks { count: 1 }).expect("filemark should pass");
    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"EFGH".to_vec(),
        },
    )
    .expect("write block B should pass");
    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"IJKL".to_vec(),
        },
    )
    .expect("write block C should pass");

    execute(&mut state, CoreCommand::Locate { logical_block: 0 })
        .expect("locate start should pass");
    execute(&mut state, CoreCommand::SpaceBlocks { count: 1 }).expect("space blocks +1");
    assert_eq!(state.current_position, 8);
    execute(&mut state, CoreCommand::SpaceBlocks { count: 1 }).expect("space blocks +1");
    assert_eq!(state.current_position, 12);
    execute(&mut state, CoreCommand::SpaceBlocks { count: -1 }).expect("space blocks -1");
    assert_eq!(state.current_position, 8);

    execute(&mut state, CoreCommand::Locate { logical_block: 0 })
        .expect("locate start should pass");
    execute(&mut state, CoreCommand::SpaceFilemarks { count: 1 })
        .expect("space filemarks +1 should pass");
    assert_eq!(state.current_position, 8);
    execute(&mut state, CoreCommand::SpaceFilemarks { count: -1 })
        .expect("space filemarks -1 should pass");
    assert_eq!(state.current_position, 4);

    execute(&mut state, CoreCommand::SpaceEndOfData { count: 1 }).expect("space eod should pass");
    assert_eq!(state.current_position, 16);
    let eod_report = execute(&mut state, CoreCommand::ReadPosition).expect("read position");
    match eod_report {
        CoreResponse::Position(report) => {
            assert_eq!(report.eod_position, 16);
            assert!(report.early_warning);
        }
        _ => panic!("unexpected response type"),
    }

    execute(&mut state, CoreCommand::Locate { logical_block: 4 })
        .expect("locate near BOT should pass");
    let near_bot = execute(&mut state, CoreCommand::ReadPosition).expect("read position");
    match near_bot {
        CoreResponse::Position(report) => assert!(!report.early_warning),
        _ => panic!("unexpected response type"),
    }

    cleanup(&state);
}

#[test]
fn reverse_space_filemarks_positions_at_previous_files_last_record() {
    let mut state = new_state("space-filemarks-backward");

    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-space-filemarks-backward".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeFixed { block_size: 4 }).expect("fixed mode");
    for payload in [b"AAAA".to_vec(), b"BBBB".to_vec()] {
        execute(&mut state, CoreCommand::WriteData { payload }).expect("write first file record");
    }
    execute(&mut state, CoreCommand::WriteFilemarks { count: 1 }).expect("write filemark");
    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"CCCC".to_vec(),
        },
    )
    .expect("write next file record");

    execute(&mut state, CoreCommand::SpaceFilemarks { count: -1 }).expect("backspace one filemark");
    // SSC-3: positioned on the BOT side of the filemark.
    assert_eq!(state.current_position, 8);
    // Reaching the last record of the previous file takes an extra SPACE -1 block.
    execute(&mut state, CoreCommand::SpaceBlocks { count: -1 }).expect("backspace one block");
    let previous_file_last_record =
        execute(&mut state, CoreCommand::ReadData).expect("read previous file's last record");
    assert_eq!(
        previous_file_last_record,
        CoreResponse::Data(b"BBBB".to_vec())
    );

    cleanup(&state);
}

#[test]
fn fixed_variable_modes_and_mode_log_pages_are_available() {
    let mut state = new_state("mode-log-pages");

    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-mode-log".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeFixed { block_size: 4 })
        .expect("set fixed mode should pass");

    let mismatch = execute_with_sense(
        &mut state,
        CoreCommand::WriteData {
            payload: b"xyz".to_vec(),
        },
    )
    .expect_err("fixed mode mismatch should fail");
    assert_eq!(mismatch.sense_key, 0x05);

    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"ABCD".to_vec(),
        },
    )
    .expect("fixed-size write should pass");

    let mode_payload =
        execute(&mut state, CoreCommand::ModeSense { page_code: 0x10 }).expect("mode page");
    match mode_payload {
        CoreResponse::Bytes(bytes) => {
            assert_eq!(bytes[0], 0x10);
            assert_eq!(bytes[2], 0x01);
            assert_eq!(
                u32::from_be_bytes([bytes[4], bytes[5], bytes[6], bytes[7]]),
                4
            );
        }
        _ => panic!("unexpected response type"),
    }

    let log_payload =
        execute(&mut state, CoreCommand::LogSense { page_code: 0x31 }).expect("log page");
    match log_payload {
        CoreResponse::Bytes(bytes) => {
            assert_eq!(bytes[0], 0x31);
            assert!(bytes.len() >= 2 + (8 * 8));
        }
        _ => panic!("unexpected response type"),
    }

    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("switch variable mode");
    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"xyz".to_vec(),
        },
    )
    .expect("variable mode should allow mixed length");

    cleanup(&state);
}

#[test]
fn unsupported_pages_and_space_bounds_map_illegal_request() {
    let mut state = new_state("unsupported-boundary");

    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-unsupported".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("variable mode");

    let bad_mode = execute_with_sense(&mut state, CoreCommand::ModeSense { page_code: 0x99 })
        .expect_err("unsupported mode page should fail");
    assert_eq!(bad_mode.sense_key, 0x05);
    assert_eq!(bad_mode.asc, 0x24);

    let bad_log = execute_with_sense(&mut state, CoreCommand::LogSense { page_code: 0x77 })
        .expect_err("unsupported log page should fail");
    assert_eq!(bad_log.sense_key, 0x05);

    let no_block = execute_with_sense(&mut state, CoreCommand::SpaceBlocks { count: 1 })
        .expect_err("space blocks without records should report EOD");
    assert_eq!(no_block.sense_key, 0x08);
    assert_eq!(no_block.asc, 0x00);
    assert_eq!(no_block.ascq, 0x05);

    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"ABCD".to_vec(),
        },
    )
    .expect("write should pass");
    execute(&mut state, CoreCommand::Locate { logical_block: 0 }).expect("locate should pass");

    let before_bot = execute_with_sense(&mut state, CoreCommand::SpaceBlocks { count: -1 })
        .expect_err("space blocks before BOT should fail");
    assert_eq!(before_bot.sense_key, 0x05);

    let invalid_eod = execute_with_sense(&mut state, CoreCommand::SpaceEndOfData { count: 3 })
        .expect_err("unsupported eod count should fail");
    assert_eq!(invalid_eod.sense_key, 0x05);

    cleanup(&state);
}

#[test]
fn locate_rejects_non_addressable_positions() {
    let mut state = new_state("locate-boundary");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-locate-boundary".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeFixed { block_size: 4 }).expect("fixed mode");

    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"ABCD".to_vec(),
        },
    )
    .expect("write block should pass");

    let err = execute_with_sense(&mut state, CoreCommand::Locate { logical_block: 2 })
        .expect_err("locate into the middle of a logical block should fail");
    assert_eq!(err.sense_key, 0x05);
}

/// Veeam append probe: EOD, SPACE -1 FM twice, READ one fixed block.
/// Layout: D D FM C FM FM (data set, filemark, catalog, two trailing filemarks).
#[test]
fn veeam_append_probe_reads_filemark_after_reverse_space() {
    use super::command_chain::read_fixed_blocks;
    use super::error::TapeError;
    let mut state = new_state("veeam-append-probe");
    execute(&mut state, CoreCommand::Load { cartridge_id: "cart-veeam-append".to_string() })
        .expect("load");
    let bs = 262_144usize;
    execute(&mut state, CoreCommand::SetBlockModeFixed { block_size: bs as u32 }).expect("fixed");
    for b in [0x11u8, 0x22] {
        execute(&mut state, CoreCommand::WriteData { payload: vec![b; bs] }).expect("data");
    }
    execute(&mut state, CoreCommand::WriteFilemarks { count: 1 }).expect("fm1");
    execute(&mut state, CoreCommand::WriteData { payload: vec![0xCC; bs] }).expect("catalog");
    execute(&mut state, CoreCommand::WriteFilemarks { count: 2 }).expect("fm2+fm3");
    let bsu = bs as u64;
    assert_eq!(state.filemarks, vec![2 * bsu, 4 * bsu, 5 * bsu]);

    execute(&mut state, CoreCommand::SpaceEndOfData { count: 1 }).expect("eod");
    execute(&mut state, CoreCommand::SpaceFilemarks { count: -1 }).expect("bsf 1");
    assert_eq!(state.current_position, 5 * bsu);
    execute(&mut state, CoreCommand::SpaceFilemarks { count: -1 }).expect("bsf 2");
    assert_eq!(state.current_position, 4 * bsu, "BOT side of second-to-last filemark");

    // First READ must report FILEMARK (not blank check / medium error) and step past it.
    match read_fixed_blocks(&mut state, bs, 1) {
        Err(TapeError::FilemarkDetected { residue, .. }) => assert_eq!(residue, 1),
        other => panic!("expected FilemarkDetected, got {other:?}"),
    }
    assert_eq!(state.current_position, 5 * bsu);
    // And the sense must be NO SENSE + FILEMARK bit, 00/01.
    let frame = super::commands_core::tape_error_to_sense(&TapeError::FilemarkDetected {
        residue: 1,
        partial_data: Vec::new(),
    });
    let bytes = crate::iscsi::cdb_server::sense_frame_to_bytes(&frame);
    assert_eq!(bytes[2], 0x80);
    assert_eq!((bytes[12], bytes[13]), (0x00, 0x01));

    // Spacing back over the catalog file reaches its data: SPACE -2 FM then +1 FM.
    execute(&mut state, CoreCommand::SpaceEndOfData { count: 1 }).expect("eod");
    execute(&mut state, CoreCommand::SpaceFilemarks { count: -2 }).expect("bsf 2");
    execute(&mut state, CoreCommand::SpaceFilemarks { count: -1 }).expect("bsf 1");
    assert_eq!(state.current_position, 2 * bsu);
    execute(&mut state, CoreCommand::SpaceFilemarks { count: 1 }).expect("fsf 1");
    let catalog = read_fixed_blocks(&mut state, bs, 1).expect("read catalog block");
    assert_eq!(catalog, vec![0xCC; bs]);
    cleanup(&state);
}
