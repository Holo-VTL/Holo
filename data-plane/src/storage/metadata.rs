use std::env;
use std::fmt;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard, OnceLock};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use super::blk_map::BlkMapRecord;
use super::layout::{checksum32, sanitize_id, SegmentKind};
use super::map_lookup::MapLookupRecord;
use super::segment::{read_segment_file_bounded, write_segment_file};

pub const MAX_MAINTENANCE_METADATA_BYTES: u64 = 8 * 1024 * 1024;
pub const MAX_MAINTENANCE_METADATA_RECORDS: usize = 65_536;
const MAX_LIVE_METADATA_ESTIMATE_BYTES: u64 = 64 * 1024 * 1024;

pub(crate) fn preflight_maintenance_metadata(paths: &[&Path]) -> Result<(), StorageError> {
    let mut total_bytes = 0u64;
    for path in paths {
        let metadata = match fs::symlink_metadata(path) {
            Ok(metadata) => metadata,
            Err(err) if err.kind() == io::ErrorKind::NotFound => continue,
            Err(err) => return Err(StorageError::Io(err)),
        };
        if metadata.file_type().is_symlink() || !metadata.is_file() {
            return Err(StorageError::Conflict(
                "unsafe maintenance metadata file".to_string(),
            ));
        }
        if metadata.len() > MAX_MAINTENANCE_METADATA_BYTES {
            return Err(StorageError::MetadataBudgetExceeded);
        }
        total_bytes = total_bytes
            .checked_add(metadata.len())
            .ok_or(StorageError::MetadataBudgetExceeded)?;
    }
    let estimated_live_bytes = total_bytes
        .checked_mul(6)
        .ok_or(StorageError::MetadataBudgetExceeded)?;
    if total_bytes > MAX_MAINTENANCE_METADATA_BYTES
        || estimated_live_bytes > MAX_LIVE_METADATA_ESTIMATE_BYTES
    {
        return Err(StorageError::MetadataBudgetExceeded);
    }
    Ok(())
}

pub(crate) fn checked_metadata_record_count(count: usize) -> Result<usize, StorageError> {
    if count > MAX_MAINTENANCE_METADATA_RECORDS {
        return Err(StorageError::MetadataBudgetExceeded);
    }
    Ok(count)
}

const DEFAULT_SLOW_LOCK_WAIT_MS: u64 = 1000;

#[derive(Debug)]
pub enum StorageError {
    Io(io::Error),
    SpaceExhausted(String),
    SpaceAdmission(super::space_guard::SpaceGuardError),
    Corrupt(String),
    VersionMismatch { expected: u16, got: u16 },
    NotFound(String),
    Conflict(String),
    MetadataBudgetExceeded,
    Cancelled,
    Internal(String),
}

impl fmt::Display for StorageError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Io(err) => write!(f, "io error: {err}"),
            Self::SpaceExhausted(msg) => write!(f, "filesystem is out of space: {msg}"),
            Self::SpaceAdmission(err) => write!(f, "filesystem space admission failed: {err}"),
            Self::Corrupt(msg) => write!(f, "corrupt state: {msg}"),
            Self::VersionMismatch { expected, got } => {
                write!(f, "version mismatch expected={expected} got={got}")
            }
            Self::NotFound(msg) => write!(f, "not found: {msg}"),
            Self::Conflict(msg) => write!(f, "conflict: {msg}"),
            Self::MetadataBudgetExceeded => write!(f, "maintenance metadata budget exceeded"),
            Self::Cancelled => write!(f, "maintenance operation cancelled"),
            Self::Internal(msg) => write!(f, "internal error: {msg}"),
        }
    }
}

impl std::error::Error for StorageError {}

impl From<io::Error> for StorageError {
    fn from(value: io::Error) -> Self {
        let raw = value.raw_os_error();
        if value.kind() == io::ErrorKind::StorageFull
            || raw == Some(rustix::io::Errno::NOSPC.raw_os_error())
            || raw == Some(rustix::io::Errno::DQUOT.raw_os_error())
        {
            return Self::SpaceExhausted(value.to_string());
        }
        Self::Io(value)
    }
}

impl From<super::space_guard::SpaceGuardError> for StorageError {
    fn from(value: super::space_guard::SpaceGuardError) -> Self {
        Self::SpaceAdmission(value)
    }
}

pub(crate) fn lock_storage_mutex<'a, T>(
    mutex: &'a Mutex<T>,
    name: &str,
) -> Result<MutexGuard<'a, T>, StorageError> {
    let started_at = Instant::now();
    let guard = mutex
        .lock()
        .map_err(|_| StorageError::Internal(format!("{name} cache poisoned")))?;
    let elapsed = started_at.elapsed();
    if elapsed >= slow_lock_wait_threshold() {
        eprintln!(
            "[storage_lock_slow_wait] name={name} elapsed_ms={}",
            elapsed.as_millis()
        );
    }
    Ok(guard)
}

fn slow_lock_wait_threshold() -> Duration {
    static THRESHOLD: OnceLock<Duration> = OnceLock::new();
    *THRESHOLD.get_or_init(|| {
        let millis = env::var("HOLO_STORAGE_SLOW_LOCK_WAIT_MS")
            .ok()
            .and_then(|raw| raw.parse::<u64>().ok())
            .filter(|value| *value > 0)
            .unwrap_or(DEFAULT_SLOW_LOCK_WAIT_MS);
        Duration::from_millis(millis)
    })
}

pub(crate) fn modified_nanos_from_result(
    modified: io::Result<SystemTime>,
) -> Result<u128, StorageError> {
    let modified = modified?;
    Ok(modified
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos())
}

pub(crate) fn checked_usize_from_u64(value: u64, field: &str) -> Result<usize, StorageError> {
    usize::try_from(value)
        .map_err(|_| StorageError::Corrupt(format!("{field} exceeds platform usize")))
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum CheckpointFlags {
    Clean = 1,
    Dirty = 2,
}

impl CheckpointFlags {
    fn from_u8(v: u8) -> Result<Self, StorageError> {
        match v {
            1 => Ok(Self::Clean),
            2 => Ok(Self::Dirty),
            _ => Err(StorageError::Corrupt(
                "invalid checkpoint flags".to_string(),
            )),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MetadataCheckpoint {
    pub page_id: u64,
    pub epoch: u64,
    pub active_blk_map_root: u64,
    pub active_lookup_root: u64,
    pub flags: CheckpointFlags,
}

impl MetadataCheckpoint {
    pub fn encode(&self) -> Vec<u8> {
        let mut payload = Vec::with_capacity(64);
        payload.extend_from_slice(&self.page_id.to_le_bytes());
        payload.extend_from_slice(&self.epoch.to_le_bytes());
        payload.extend_from_slice(&self.active_blk_map_root.to_le_bytes());
        payload.extend_from_slice(&self.active_lookup_root.to_le_bytes());
        payload.push(self.flags as u8);
        payload.extend_from_slice(&[0u8; 7]);
        let checksum = checksum32(&payload);
        payload.extend_from_slice(&checksum.to_le_bytes());
        payload
    }

    pub fn decode(payload: &[u8]) -> Result<Self, StorageError> {
        if payload.len() < 44 {
            return Err(StorageError::Corrupt(
                "metadata checkpoint payload too short".to_string(),
            ));
        }

        let data = &payload[..40];
        let found_checksum =
            u32::from_le_bytes(payload[40..44].try_into().map_err(|_| {
                StorageError::Corrupt("checkpoint checksum parse failed".to_string())
            })?);
        let expected_checksum = checksum32(data);
        if found_checksum != expected_checksum {
            return Err(StorageError::Corrupt(
                "checkpoint checksum mismatch".to_string(),
            ));
        }

        let page_id = u64::from_le_bytes(
            data[0..8]
                .try_into()
                .map_err(|_| StorageError::Corrupt("checkpoint parse failed".to_string()))?,
        );
        let epoch = u64::from_le_bytes(
            data[8..16]
                .try_into()
                .map_err(|_| StorageError::Corrupt("checkpoint parse failed".to_string()))?,
        );
        let active_blk_map_root = u64::from_le_bytes(
            data[16..24]
                .try_into()
                .map_err(|_| StorageError::Corrupt("checkpoint parse failed".to_string()))?,
        );
        let active_lookup_root = u64::from_le_bytes(
            data[24..32]
                .try_into()
                .map_err(|_| StorageError::Corrupt("checkpoint parse failed".to_string()))?,
        );
        let flags = CheckpointFlags::from_u8(data[32])?;

        Ok(Self {
            page_id,
            epoch,
            active_blk_map_root,
            active_lookup_root,
            flags,
        })
    }
}

pub fn storage_root_dir() -> PathBuf {
    if let Ok(root) = env::var("HOLO_STORAGE_ROOT") {
        return PathBuf::from(root);
    }

    let preferred = PathBuf::from("/var/lib/holo/storage");
    if can_write_dir(&preferred) {
        return preferred;
    }

    if let Ok(home) = env::var("HOME") {
        let home_default = PathBuf::from(home).join(".local/share/holo/storage");
        if can_write_dir(&home_default) {
            return home_default;
        }
    }

    PathBuf::from("/tmp/holo-storage")
}

pub fn storage_root_dir_for_cartridge(cartridge_id: &str) -> PathBuf {
    let media_state_dir = env::var("HOLO_MEDIA_STATE_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|_| PathBuf::from("/run/holo/media-state"));
    let pool_root_base = env::var("HOLO_STORAGE_POOL_ROOT_BASE")
        .ok()
        .map(PathBuf::from);
    resolve_cartridge_storage_root(
        cartridge_id,
        &media_state_dir,
        &storage_root_dir(),
        pool_root_base.as_deref(),
    )
}

fn resolve_cartridge_storage_root(
    cartridge_id: &str,
    media_state_dir: &Path,
    fallback_root: &Path,
    pool_root_base: Option<&Path>,
) -> PathBuf {
    let metadata_path =
        media_state_dir.join(format!("cartridge_{}.meta", sanitize_id(cartridge_id)));
    let Ok(raw) = fs::read_to_string(metadata_path) else {
        return fallback_root.to_path_buf();
    };
    let pool_id = raw.lines().find_map(|line| {
        let (key, value) = line.trim().split_once('=')?;
        (key.trim() == "pool_id" && !value.trim().is_empty()).then(|| value.trim())
    });
    let (Some(pool_id), Some(base)) = (pool_id, pool_root_base) else {
        return fallback_root.to_path_buf();
    };
    base.join(sanitize_id(pool_id))
}

pub fn persist_checkpoint_page(
    path: &Path,
    checkpoint: &MetadataCheckpoint,
) -> Result<(), StorageError> {
    let payload = checkpoint.encode();
    write_segment_file(
        path,
        SegmentKind::Metadata,
        checkpoint.page_id,
        checkpoint.epoch,
        &payload,
    )
}

pub fn load_checkpoint_page(path: &Path) -> Result<MetadataCheckpoint, StorageError> {
    let (_, payload) =
        read_segment_file_bounded(path, SegmentKind::Metadata, MAX_MAINTENANCE_METADATA_BYTES)?;
    MetadataCheckpoint::decode(&payload)
}

#[cfg(test)]
mod local_mount_pool_tests {
    use super::resolve_cartridge_storage_root;
    use std::fs;

    #[test]
    fn resolves_loaded_cartridge_to_its_pool_root() {
        let root = std::env::temp_dir().join(format!("holo-pool-route-{}", std::process::id()));
        let state_dir = root.join("media-state");
        let pool_base = root.join("pools");
        let fallback = root.join("fallback");
        fs::create_dir_all(&state_dir).expect("create state dir");
        fs::write(state_dir.join("cartridge_tape001.meta"), "pool_id=pool-a\n")
            .expect("write media map");

        let resolved =
            resolve_cartridge_storage_root("TAPE001", &state_dir, &fallback, Some(&pool_base));
        assert_eq!(resolved, pool_base.join("pool-a"));
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn missing_pool_metadata_keeps_existing_storage_root_behavior() {
        let root = std::env::temp_dir().join(format!("holo-pool-fallback-{}", std::process::id()));
        let state_dir = root.join("media-state");
        let fallback = root.join("fallback");
        fs::create_dir_all(&state_dir).expect("create state dir");
        fs::write(
            state_dir.join("cartridge_tape002.meta"),
            "capacity_bytes=1024\n",
        )
        .expect("write legacy metadata");

        let resolved = resolve_cartridge_storage_root(
            "TAPE002",
            &state_dir,
            &fallback,
            Some(&root.join("pools")),
        );
        assert_eq!(resolved, fallback);
        let _ = fs::remove_dir_all(root);
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FlushStep {
    Data,
    BlkMap,
    Lookup,
    Metadata,
}

#[derive(Debug, Clone)]
pub struct FlushReport {
    pub steps: Vec<FlushStep>,
}

#[allow(clippy::too_many_arguments)]
pub fn flush_chain(
    data_path: &Path,
    blk_map_path: &Path,
    lookup_path: &Path,
    metadata_path: &Path,
    data_payload: &[u8],
    blk_payload: &[u8],
    lookup_payload: &[u8],
    checkpoint: &MetadataCheckpoint,
    fail_after: Option<FlushStep>,
) -> Result<FlushReport, StorageError> {
    let mut steps = Vec::with_capacity(4);

    write_segment_file(
        data_path,
        SegmentKind::Data,
        1,
        checkpoint.epoch,
        data_payload,
    )?;
    steps.push(FlushStep::Data);
    if fail_after == Some(FlushStep::Data) {
        return Err(StorageError::Conflict(
            "flush interrupted after data".to_string(),
        ));
    }

    write_segment_file(
        blk_map_path,
        SegmentKind::BlkMap,
        checkpoint.active_blk_map_root,
        checkpoint.epoch,
        blk_payload,
    )?;
    steps.push(FlushStep::BlkMap);
    if fail_after == Some(FlushStep::BlkMap) {
        return Err(StorageError::Conflict(
            "flush interrupted after blk_map".to_string(),
        ));
    }

    write_segment_file(
        lookup_path,
        SegmentKind::Lookup,
        checkpoint.active_lookup_root,
        checkpoint.epoch,
        lookup_payload,
    )?;
    steps.push(FlushStep::Lookup);
    if fail_after == Some(FlushStep::Lookup) {
        return Err(StorageError::Conflict(
            "flush interrupted after lookup".to_string(),
        ));
    }

    persist_checkpoint_page(metadata_path, checkpoint)?;
    steps.push(FlushStep::Metadata);

    Ok(FlushReport { steps })
}

pub fn quarantine_invalid_metadata(path: &Path) -> Result<PathBuf, StorageError> {
    if !path.exists() {
        return Err(StorageError::NotFound("metadata path missing".to_string()));
    }
    let ts = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| StorageError::Corrupt("clock drift".to_string()))?
        .as_secs();
    let target = PathBuf::from(format!("{}.quarantine.{}", path.display(), ts));
    fs::rename(path, &target)?;
    Ok(target)
}

pub fn encode_blk_records(records: &[BlkMapRecord]) -> Vec<u8> {
    let mut out = Vec::new();
    out.extend_from_slice(&(records.len() as u64).to_le_bytes());
    for record in records {
        out.extend_from_slice(&record.encode());
    }
    out
}

pub fn encode_lookup_records(records: &[MapLookupRecord]) -> Vec<u8> {
    let mut out = Vec::new();
    out.extend_from_slice(&(records.len() as u64).to_le_bytes());
    for record in records {
        out.extend_from_slice(&record.encode());
    }
    out
}

fn can_write_dir(path: &Path) -> bool {
    if fs::create_dir_all(path).is_err() {
        return false;
    }
    let probe = path.join(".write-probe");
    match fs::write(&probe, b"probe") {
        Ok(_) => {
            let _ = fs::remove_file(probe);
            true
        }
        Err(_) => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    #[test]
    fn modified_nanos_failure_is_not_unix_epoch_fallback() {
        let err = modified_nanos_from_result(Err(io::Error::new(
            io::ErrorKind::PermissionDenied,
            "metadata timestamp unavailable",
        )))
        .expect_err("modified failure should propagate");

        assert!(matches!(err, StorageError::Io(_)));
    }

    #[test]
    fn modified_nanos_epoch_is_valid_when_timestamp_exists() {
        let nanos = modified_nanos_from_result(Ok(UNIX_EPOCH)).expect("epoch is valid");
        assert_eq!(nanos, 0);
    }

    #[test]
    fn cache_poison_lock_returns_internal_error() {
        let mutex = Mutex::new(());
        let _ = std::panic::catch_unwind(|| {
            let _guard = mutex.lock().expect("test lock");
            panic!("poison test mutex");
        });

        let err = lock_storage_mutex(&mutex, "test")
            .expect_err("poisoned cache lock should return an internal error");
        assert!(matches!(err, StorageError::Internal(message) if message == "test cache poisoned"));

        mutex.clear_poison();
    }
}
