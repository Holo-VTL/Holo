use crate::storage::StorageError;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum TapeError {
    #[error("invalid tape state transition")]
    InvalidTransition,
    #[error("invalid device identity profile: {0}")]
    InvalidProfile(String),
    #[error("unsupported VPD page: 0x{0:02X}")]
    UnsupportedVpdPage(u8),
    #[error("unsupported MODE SENSE page: 0x{0:02X}")]
    UnsupportedModePage(u8),
    #[error("unsupported LOG SENSE page: 0x{0:02X}")]
    UnsupportedLogPage(u8),
    #[error("device not ready: {0}")]
    NotReady(String),
    #[error("access denied: {0}")]
    AccessDenied(String),
    #[error("unsupported operation: {0}")]
    Unsupported(String),
    #[error("invalid argument: {0}")]
    InvalidArgument(String),
    #[error("fixed block size mismatch: expected {expected}, actual {actual}")]
    FixedBlockSizeMismatch { expected: u32, actual: u32 },
    #[error("not found: {0}")]
    NotFound(String),
    #[error("out of range: {0}")]
    OutOfRange(String),
    #[error("volume overflow")]
    VolumeOverflow,
    #[error("retention policy blocks operation")]
    RetentionBlocked,
    #[error("worm write-protect violation")]
    WormWriteProtected,
    #[error("reservation conflict: {0}")]
    ReservationConflict(String),
    #[error("invalid reservation key: {0}")]
    InvalidReservationKey(String),
    #[error("invalid reservation release: {0}")]
    InvalidReservationRelease(String),
    #[error("unauthorized target access")]
    Unauthorized,
    #[error("storage error: {0}")]
    Storage(#[from] StorageError),
}
