use crate::scsi_tape::error::TapeError;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SenseContext {
    NotReady,
    IllegalRequest,
    BlankCheckEod,
    VolumeOverflow,
    AccessDenied,
    DataProtect,
    MediumError,
    ReservationConflict,
    InvalidPersistentRelease,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SenseFrame {
    pub status: u8,
    pub sense_key: u8,
    pub asc: u8,
    pub ascq: u8,
    pub end_of_medium: bool,
    pub information_valid: bool,
    pub information: u32,
}

pub fn sense_for_context(context: SenseContext) -> SenseFrame {
    match context {
        SenseContext::NotReady => SenseFrame {
            status: 0x02,
            sense_key: 0x02,
            asc: 0x3A,
            ascq: 0x00,
            end_of_medium: false,
            information_valid: false,
            information: 0,
        },
        SenseContext::IllegalRequest => SenseFrame {
            status: 0x02,
            sense_key: 0x05,
            asc: 0x24,
            ascq: 0x00,
            end_of_medium: false,
            information_valid: false,
            information: 0,
        },
        SenseContext::BlankCheckEod => SenseFrame {
            status: 0x02,
            sense_key: 0x08,
            asc: 0x00,
            ascq: 0x05,
            end_of_medium: false,
            // Blank check at BOT: report VALID info field=1 per common initiator expectations.
            information_valid: true,
            information: 1,
        },
        SenseContext::VolumeOverflow => SenseFrame {
            status: 0x02,
            sense_key: 0x0D,
            asc: 0x00,
            ascq: 0x00,
            end_of_medium: true,
            information_valid: false,
            information: 0,
        },
        SenseContext::AccessDenied => SenseFrame {
            status: 0x02,
            sense_key: 0x07,
            asc: 0x27,
            ascq: 0x00,
            end_of_medium: false,
            information_valid: false,
            information: 0,
        },
        SenseContext::DataProtect => SenseFrame {
            status: 0x02,
            sense_key: 0x07,
            asc: 0x30,
            ascq: 0x0C,
            end_of_medium: false,
            information_valid: false,
            information: 0,
        },
        SenseContext::MediumError => SenseFrame {
            status: 0x02,
            sense_key: 0x03,
            asc: 0x11,
            ascq: 0x00,
            end_of_medium: false,
            information_valid: false,
            information: 0,
        },
        SenseContext::ReservationConflict => SenseFrame {
            status: 0x18,
            sense_key: 0x0B,
            asc: 0x18,
            ascq: 0x02,
            end_of_medium: false,
            information_valid: false,
            information: 0,
        },
        SenseContext::InvalidPersistentRelease => SenseFrame {
            status: 0x02,
            sense_key: 0x05,
            asc: 0x26,
            ascq: 0x04,
            end_of_medium: false,
            information_valid: false,
            information: 0,
        },
    }
}

pub fn resolve_sense_for_error(error: &TapeError) -> SenseFrame {
    match error {
        TapeError::Unauthorized | TapeError::AccessDenied(_) => {
            sense_for_context(SenseContext::AccessDenied)
        }
        TapeError::WormWriteProtected => sense_for_context(SenseContext::DataProtect),
        TapeError::ReservationConflict(_) => sense_for_context(SenseContext::ReservationConflict),
        TapeError::InvalidReservationRelease(_) => {
            sense_for_context(SenseContext::InvalidPersistentRelease)
        }
        TapeError::InvalidReservationKey(_) => sense_for_context(SenseContext::IllegalRequest),
        TapeError::NotReady(_) => sense_for_context(SenseContext::NotReady),
        TapeError::VolumeOverflow => sense_for_context(SenseContext::VolumeOverflow),
        TapeError::UnsupportedVpdPage(_)
        | TapeError::UnsupportedModePage(_)
        | TapeError::UnsupportedLogPage(_)
        | TapeError::InvalidProfile(_)
        | TapeError::InvalidArgument(_)
        | TapeError::FixedBlockSizeMismatch { .. }
        | TapeError::OutOfRange(_)
        | TapeError::InvalidTransition
        | TapeError::RetentionBlocked
        | TapeError::Unsupported(_) => sense_for_context(SenseContext::IllegalRequest),
        TapeError::Storage(_) => sense_for_context(SenseContext::MediumError),
        TapeError::NotFound(_) => sense_for_context(SenseContext::BlankCheckEod),
    }
}
