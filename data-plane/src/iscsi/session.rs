use crate::scsi_tape::error::TapeError;
use crate::scsi_tape::state::{MountState, TapeState};

#[derive(Debug)]
pub struct Session {
    pub client_id: String,
    pub drive_state: TapeState,
}

impl Session {
    pub fn new(client_id: impl Into<String>, drive_id: impl Into<String>) -> Self {
        Self {
            client_id: client_id.into(),
            drive_state: TapeState::new(drive_id),
        }
    }

    pub fn begin_io(&mut self) -> Result<(), TapeError> {
        if self.drive_state.mount_state != MountState::Loaded {
            return Err(TapeError::InvalidTransition);
        }
        self.drive_state.mount_state = MountState::Busy;
        Ok(())
    }

    pub fn end_io(&mut self) {
        if self.drive_state.mount_state == MountState::Busy {
            self.drive_state.mount_state = MountState::Loaded;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn end_io_only_transitions_busy_session() {
        let mut session = Session::new("client-a", "drive-a");
        let original = session.drive_state.mount_state;
        session.end_io();
        assert_eq!(session.drive_state.mount_state, original);

        session.drive_state.mount_state = MountState::Loaded;
        session
            .begin_io()
            .expect("begin io from loaded should pass");
        assert_eq!(session.drive_state.mount_state, MountState::Busy);
        session.end_io();
        assert_eq!(session.drive_state.mount_state, MountState::Loaded);
    }
}
