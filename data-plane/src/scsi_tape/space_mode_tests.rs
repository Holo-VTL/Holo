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
