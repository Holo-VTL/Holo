use super::command_chain::EraseMode;
use super::commands_core::{execute, execute_with_sense, CoreCommand, CoreResponse};
use super::error::TapeError;
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
fn worm_locked_media_blocks_write_and_erase() {
    let mut state = new_state("worm-lock");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-worm-lock".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("variable mode");

    execute(
        &mut state,
        CoreCommand::SetWormPolicy {
            is_worm_media: true,
            retention_locked: true,
        },
    )
    .expect("worm policy should be set");

    let write_sense = execute_with_sense(
        &mut state,
        CoreCommand::WriteData {
            payload: b"blocked".to_vec(),
        },
    )
    .expect_err("write must be blocked for locked worm");
    assert_eq!(write_sense.sense_key, 0x07);
    assert_eq!(write_sense.asc, 0x30);
    assert_eq!(write_sense.ascq, 0x0C);

    let erase_sense = execute_with_sense(
        &mut state,
        CoreCommand::Erase {
            mode: EraseMode::Short,
        },
    )
    .expect_err("erase must be blocked");
    assert_eq!(erase_sense.sense_key, 0x07);

    let unlock_err = execute_with_sense(
        &mut state,
        CoreCommand::SetWormPolicy {
            is_worm_media: true,
            retention_locked: false,
        },
    )
    .expect_err("locked worm media must not allow policy downgrade");
    assert_eq!(unlock_err.sense_key, 0x07);

    cleanup(&state);
}

#[test]
fn unlocked_worm_media_blocks_overwrite_after_data_is_written() {
    let mut state = new_state("worm-unlocked-overwrite");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-worm-unlocked-overwrite".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("variable mode");

    execute(
        &mut state,
        CoreCommand::SetWormPolicy {
            is_worm_media: true,
            retention_locked: false,
        },
    )
    .expect("worm policy should be set");

    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"original".to_vec(),
        },
    )
    .expect("initial append to WORM media should pass");
    execute(&mut state, CoreCommand::Locate { logical_block: 0 }).expect("locate to BOT");

    let overwrite = execute_with_sense(
        &mut state,
        CoreCommand::WriteData {
            payload: b"mutated".to_vec(),
        },
    )
    .expect_err("overwrite of written WORM media must be blocked");
    assert_eq!(overwrite.sense_key, 0x07);
    assert_eq!(overwrite.asc, 0x30);

    cleanup(&state);
}

#[test]
fn unlocked_worm_media_blocks_erase_after_data_is_written() {
    let mut state = new_state("worm-unlocked-erase");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-worm-unlocked-erase".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("variable mode");

    execute(
        &mut state,
        CoreCommand::SetWormPolicy {
            is_worm_media: true,
            retention_locked: false,
        },
    )
    .expect("worm policy should be set");

    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"preserve".to_vec(),
        },
    )
    .expect("initial append to WORM media should pass");

    for mode in [EraseMode::Short, EraseMode::Long] {
        let erase = execute_with_sense(&mut state, CoreCommand::Erase { mode })
            .expect_err("erase of written WORM media must be blocked");
        assert_eq!(erase.sense_key, 0x07);
        assert_eq!(erase.asc, 0x30);
    }

    cleanup(&state);
}

#[test]
fn persistent_reservation_blocks_non_owner_protected_commands() {
    let mut state = new_state("pr-owner");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-pr-owner".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("variable mode");

    execute(
        &mut state,
        CoreCommand::ReservationRegister {
            initiator: "host-a".to_string(),
            key: 11,
        },
    )
    .expect("host-a register should pass");
    execute(
        &mut state,
        CoreCommand::ReservationRegister {
            initiator: "host-b".to_string(),
            key: 22,
        },
    )
    .expect("host-b register should pass");
    execute(
        &mut state,
        CoreCommand::ReservationReserve {
            initiator: "host-a".to_string(),
            key: 11,
        },
    )
    .expect("host-a reserve should pass");

    let conflict = execute_with_sense(
        &mut state,
        CoreCommand::WriteDataAs {
            initiator: "host-b".to_string(),
            payload: b"conflict".to_vec(),
        },
    )
    .expect_err("host-b write should conflict");
    assert_eq!(conflict.status, 0x18);
    assert_eq!(conflict.asc, 0x18);
    assert_eq!(conflict.ascq, 0x02);

    execute(
        &mut state,
        CoreCommand::ReservationRelease {
            initiator: "host-a".to_string(),
            key: 11,
        },
    )
    .expect("release should pass");

    execute(
        &mut state,
        CoreCommand::WriteDataAs {
            initiator: "host-b".to_string(),
            payload: b"allowed".to_vec(),
        },
    )
    .expect("host-b write should pass after release");

    cleanup(&state);
}

#[test]
fn reservation_invalid_key_and_readback_are_deterministic() {
    let mut state = new_state("pr-invalid");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-pr-invalid".to_string(),
        },
    )
    .expect("load should pass");

    let reserve_without_register = execute_with_sense(
        &mut state,
        CoreCommand::ReservationReserve {
            initiator: "host-a".to_string(),
            key: 10,
        },
    )
    .expect_err("reserve without register should fail");
    assert_eq!(reserve_without_register.sense_key, 0x05);

    execute(
        &mut state,
        CoreCommand::ReservationRegister {
            initiator: "host-a".to_string(),
            key: 10,
        },
    )
    .expect("register should pass");
    execute(
        &mut state,
        CoreCommand::ReservationReserve {
            initiator: "host-a".to_string(),
            key: 10,
        },
    )
    .expect("reserve should pass");

    let invalid_release = execute_with_sense(
        &mut state,
        CoreCommand::ReservationRelease {
            initiator: "host-a".to_string(),
            key: 99,
        },
    )
    .expect_err("release with wrong key should fail");
    assert_eq!(invalid_release.sense_key, 0x05);

    let report =
        execute(&mut state, CoreCommand::ReservationRead).expect("read report should pass");
    match report {
        CoreResponse::Reservation(snapshot) => {
            assert_eq!(snapshot.registration_count, 1);
            assert_eq!(snapshot.active_owner.as_deref(), Some("host-a"));
            assert_eq!(snapshot.active_key, Some(10));
        }
        _ => panic!("unexpected response for reservation read"),
    }

    execute(
        &mut state,
        CoreCommand::ReservationClear {
            initiator: "host-a".to_string(),
            key: 10,
        },
    )
    .expect("clear should pass");

    let cleared =
        execute(&mut state, CoreCommand::ReservationRead).expect("read report should pass");
    match cleared {
        CoreResponse::Reservation(snapshot) => {
            assert_eq!(snapshot.registration_count, 0);
            assert!(snapshot.active_owner.is_none());
            assert!(snapshot.active_key.is_none());
        }
        _ => panic!("unexpected response for reservation read"),
    }

    cleanup(&state);
}

#[test]
fn non_owner_release_returns_conflict_error() {
    let mut state = new_state("pr-release-conflict");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-pr-release-conflict".to_string(),
        },
    )
    .expect("load should pass");

    execute(
        &mut state,
        CoreCommand::ReservationRegister {
            initiator: "host-a".to_string(),
            key: 31,
        },
    )
    .expect("register should pass");
    execute(
        &mut state,
        CoreCommand::ReservationRegister {
            initiator: "host-b".to_string(),
            key: 32,
        },
    )
    .expect("register should pass");
    execute(
        &mut state,
        CoreCommand::ReservationReserve {
            initiator: "host-a".to_string(),
            key: 31,
        },
    )
    .expect("reserve should pass");

    let err = execute(
        &mut state,
        CoreCommand::ReservationRelease {
            initiator: "host-b".to_string(),
            key: 32,
        },
    )
    .expect_err("non-owner release should fail");
    assert!(matches!(err, TapeError::ReservationConflict(_)));

    cleanup(&state);
}

#[test]
fn registrant_can_clear_even_when_not_reservation_owner() {
    let mut state = new_state("pr-clear-registrant");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-pr-clear-registrant".to_string(),
        },
    )
    .expect("load should pass");

    execute(
        &mut state,
        CoreCommand::ReservationRegister {
            initiator: "host-a".to_string(),
            key: 41,
        },
    )
    .expect("host-a register should pass");
    execute(
        &mut state,
        CoreCommand::ReservationRegister {
            initiator: "host-b".to_string(),
            key: 42,
        },
    )
    .expect("host-b register should pass");
    execute(
        &mut state,
        CoreCommand::ReservationReserve {
            initiator: "host-a".to_string(),
            key: 41,
        },
    )
    .expect("host-a reserve should pass");

    execute(
        &mut state,
        CoreCommand::ReservationClear {
            initiator: "host-b".to_string(),
            key: 42,
        },
    )
    .expect("registrant host-b should be able to clear reservation");

    let cleared =
        execute(&mut state, CoreCommand::ReservationRead).expect("read report should pass");
    match cleared {
        CoreResponse::Reservation(snapshot) => {
            assert_eq!(snapshot.registration_count, 0);
            assert!(snapshot.active_owner.is_none());
            assert!(snapshot.active_key.is_none());
        }
        _ => panic!("unexpected response for reservation read"),
    }

    cleanup(&state);
}

#[test]
fn register_ignore_and_preempt_replace_existing_reservation_owner() {
    let mut state = new_state("pr-register-ignore-preempt");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-pr-register-ignore-preempt".to_string(),
        },
    )
    .expect("load should pass");

    execute(
        &mut state,
        CoreCommand::ReservationRegister {
            initiator: "host-a".to_string(),
            key: 61,
        },
    )
    .expect("host-a register");
    execute(
        &mut state,
        CoreCommand::ReservationReserve {
            initiator: "host-a".to_string(),
            key: 61,
        },
    )
    .expect("host-a reserve");
    execute(
        &mut state,
        CoreCommand::ReservationRegisterIgnore {
            initiator: "host-b".to_string(),
            service_key: 62,
        },
    )
    .expect("host-b register-and-ignore");
    execute(
        &mut state,
        CoreCommand::ReservationPreempt {
            initiator: "host-b".to_string(),
            key: 62,
            service_key: 61,
        },
    )
    .expect("host-b preempt");

    let report =
        execute(&mut state, CoreCommand::ReservationRead).expect("read report should pass");
    match report {
        CoreResponse::Reservation(snapshot) => {
            assert_eq!(snapshot.registration_count, 1);
            assert_eq!(snapshot.active_owner.as_deref(), Some("host-b"));
            assert_eq!(snapshot.active_key, Some(62));
        }
        _ => panic!("unexpected response for reservation read"),
    }

    cleanup(&state);
}
