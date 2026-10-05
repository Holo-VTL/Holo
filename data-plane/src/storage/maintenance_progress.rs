use super::metadata::StorageError;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum MaintenancePhase {
    Scan,
    Verify,
    Copy,
    Commit,
}

impl MaintenancePhase {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Scan => "scan",
            Self::Verify => "verify",
            Self::Copy => "copy",
            Self::Commit => "commit",
        }
    }
}

pub type MaintenanceProgress<'a> =
    dyn FnMut(MaintenancePhase, u64, u64) -> Result<(), StorageError> + 'a;

pub(crate) fn no_maintenance_progress(
    _phase: MaintenancePhase,
    _bytes: u64,
    _records: u64,
) -> Result<(), StorageError> {
    Ok(())
}
