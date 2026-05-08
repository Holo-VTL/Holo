use crate::scsi_tape::error::TapeError;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RetentionState {
    None,
    Locked,
    Expired,
}

pub fn enforce_write(retention_state: RetentionState) -> Result<(), TapeError> {
    if retention_state == RetentionState::Locked {
        return Err(TapeError::WormWriteProtected);
    }
    Ok(())
}
