use crate::scsi_tape::error::TapeError;
use crate::scsi_tape::state::TapeState;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReservationSnapshot {
    pub generation: u32,
    pub registration_count: usize,
    pub registrations: Vec<(String, u64)>,
    pub active_owner: Option<String>,
    pub active_key: Option<u64>,
}

fn validate_initiator(initiator: &str) -> Result<(), TapeError> {
    if initiator.trim().is_empty() {
        return Err(TapeError::InvalidArgument(
            "initiator id must not be empty".to_string(),
        ));
    }
    Ok(())
}

fn validate_key(key: u64, op: &str) -> Result<(), TapeError> {
    if key == 0 {
        return Err(TapeError::InvalidReservationKey(format!(
            "{op} requires non-zero key"
        )));
    }
    Ok(())
}

pub fn register_key(state: &mut TapeState, initiator: &str, key: u64) -> Result<(), TapeError> {
    validate_initiator(initiator)?;
    validate_key(key, "register")?;

    state
        .reservation_state
        .registrations
        .insert(initiator.to_string(), key);
    if state.reservation_state.active_owner.as_deref() == Some(initiator) {
        state.reservation_state.active_key = Some(key);
    }
    state.reservation_state.generation = state.reservation_state.generation.wrapping_add(1);
    Ok(())
}

pub fn register_ignore_existing(
    state: &mut TapeState,
    initiator: &str,
    service_key: u64,
) -> Result<(), TapeError> {
    register_key(state, initiator, service_key)
}

pub fn reserve_key(state: &mut TapeState, initiator: &str, key: u64) -> Result<(), TapeError> {
    validate_initiator(initiator)?;
    validate_key(key, "reserve")?;

    let registered = state
        .reservation_state
        .registrations
        .get(initiator)
        .copied()
        .ok_or_else(|| {
            TapeError::InvalidReservationKey(format!("initiator {initiator} has no registered key"))
        })?;
    if registered != key {
        return Err(TapeError::InvalidReservationKey(format!(
            "reserve key mismatch for initiator {initiator}"
        )));
    }

    match state.reservation_state.active_owner.as_deref() {
        None => {}
        Some(owner) if owner == initiator => {}
        Some(owner) => {
            return Err(TapeError::ReservationConflict(format!(
                "reservation owner is {owner}"
            )));
        }
    }

    state.reservation_state.active_owner = Some(initiator.to_string());
    state.reservation_state.active_key = Some(key);
    state.reservation_state.generation = state.reservation_state.generation.wrapping_add(1);
    Ok(())
}

pub fn preempt_key(
    state: &mut TapeState,
    initiator: &str,
    key: u64,
    service_key: u64,
) -> Result<(), TapeError> {
    validate_initiator(initiator)?;
    validate_key(key, "preempt")?;
    validate_key(service_key, "preempt service")?;

    let registered = state
        .reservation_state
        .registrations
        .get(initiator)
        .copied()
        .ok_or_else(|| {
            TapeError::InvalidReservationKey(format!("initiator {initiator} has no registered key"))
        })?;
    if registered != key {
        return Err(TapeError::InvalidReservationKey(format!(
            "preempt key mismatch for initiator {initiator}"
        )));
    }

    state
        .reservation_state
        .registrations
        .retain(|owner, registered_key| owner == initiator || *registered_key != service_key);
    state.reservation_state.active_owner = Some(initiator.to_string());
    state.reservation_state.active_key = Some(key);
    state.reservation_state.generation = state.reservation_state.generation.wrapping_add(1);
    Ok(())
}

pub fn register_and_move_key(
    state: &mut TapeState,
    initiator: &str,
    key: u64,
    service_key: u64,
    target_initiator: &str,
    unregister_source: bool,
) -> Result<(), TapeError> {
    validate_initiator(initiator)?;
    validate_initiator(target_initiator)?;
    validate_key(key, "register and move")?;
    validate_key(service_key, "register and move service")?;
    if initiator == target_initiator {
        return Err(TapeError::InvalidArgument(
            "register and move target must differ from source".to_string(),
        ));
    }

    let registered = state
        .reservation_state
        .registrations
        .get(initiator)
        .copied()
        .ok_or_else(|| {
            TapeError::InvalidReservationKey(format!("initiator {initiator} has no registered key"))
        })?;
    if registered != key {
        return Err(TapeError::InvalidReservationKey(format!(
            "register and move key mismatch for initiator {initiator}"
        )));
    }
    if state.reservation_state.active_owner.as_deref() != Some(initiator) {
        return Err(TapeError::InvalidReservationKey(
            "register and move requires source reservation owner".to_string(),
        ));
    }

    state
        .reservation_state
        .registrations
        .insert(target_initiator.to_string(), service_key);
    if unregister_source {
        state.reservation_state.registrations.remove(initiator);
    }
    state.reservation_state.active_owner = Some(target_initiator.to_string());
    state.reservation_state.active_key = Some(service_key);
    state.reservation_state.generation = state.reservation_state.generation.wrapping_add(1);
    Ok(())
}

pub fn release_key(state: &mut TapeState, initiator: &str, key: u64) -> Result<(), TapeError> {
    validate_initiator(initiator)?;
    validate_key(key, "release")?;

    let owner = state
        .reservation_state
        .active_owner
        .as_deref()
        .ok_or_else(|| TapeError::InvalidReservationRelease("no active reservation".to_string()))?;
    if owner != initiator {
        return Err(TapeError::ReservationConflict(format!(
            "reservation owner is {owner}"
        )));
    }

    if state.reservation_state.active_key != Some(key) {
        return Err(TapeError::InvalidReservationKey(
            "release key does not match owner key".to_string(),
        ));
    }

    state.reservation_state.active_owner = None;
    state.reservation_state.active_key = None;
    state.reservation_state.generation = state.reservation_state.generation.wrapping_add(1);
    Ok(())
}

pub fn clear_keys(state: &mut TapeState, initiator: &str, key: u64) -> Result<(), TapeError> {
    validate_initiator(initiator)?;
    validate_key(key, "clear")?;

    let registered = state
        .reservation_state
        .registrations
        .get(initiator)
        .copied()
        .ok_or_else(|| {
            TapeError::InvalidReservationKey(format!("initiator {initiator} has no registered key"))
        })?;
    if registered != key {
        return Err(TapeError::InvalidReservationKey(
            "clear key does not match registered key".to_string(),
        ));
    }

    state.reservation_state.registrations.clear();
    state.reservation_state.active_owner = None;
    state.reservation_state.active_key = None;
    state.reservation_state.generation = state.reservation_state.generation.wrapping_add(1);
    Ok(())
}

pub fn ensure_reservation_access(state: &TapeState, initiator: &str) -> Result<(), TapeError> {
    validate_initiator(initiator)?;
    if let Some(owner) = state.reservation_state.active_owner.as_deref() {
        if owner != initiator {
            return Err(TapeError::ReservationConflict(format!(
                "reservation owner is {owner}"
            )));
        }
    }
    Ok(())
}

pub fn snapshot(state: &TapeState) -> ReservationSnapshot {
    ReservationSnapshot {
        generation: state.reservation_state.generation,
        registration_count: state.reservation_state.registrations.len(),
        registrations: state
            .reservation_state
            .registrations
            .iter()
            .map(|(initiator, key)| (initiator.clone(), *key))
            .collect(),
        active_owner: state.reservation_state.active_owner.clone(),
        active_key: state.reservation_state.active_key,
    }
}
