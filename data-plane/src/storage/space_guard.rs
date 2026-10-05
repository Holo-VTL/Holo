use std::collections::HashMap;
use std::fs::{self, File};
use std::io::{Read, Write};
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};

use rustix::fs::{flock, fstatvfs, open, openat, FlockOperation, Mode, OFlags};

use super::layout_lease::storage_runtime_dir;

pub const SPACE_RESERVE_BYTES: u64 = 64 * 1024 * 1024;
const BLOB_RECORD_OVERHEAD: u64 = 24 + 59 + 40 + 52;
const SEGMENT_OVERHEAD: u64 = 96 + 4;
static NEXT_RESERVATION_ID: AtomicU64 = AtomicU64::new(1);

fn lock_dir_handles() -> &'static Mutex<HashMap<PathBuf, Arc<File>>> {
    static HANDLES: OnceLock<Mutex<HashMap<PathBuf, Arc<File>>>> = OnceLock::new();
    HANDLES.get_or_init(|| Mutex::new(HashMap::new()))
}

#[derive(Debug, PartialEq, Eq)]
pub enum SpaceGuardError {
    Busy,
    UnknownCapacity,
    InsufficientSpace { available: u64, required: u64 },
    ArithmeticOverflow,
    InvalidAllocationUnit,
    Io(String),
}

impl std::fmt::Display for SpaceGuardError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Busy => write!(f, "filesystem storage is busy"),
            Self::UnknownCapacity => write!(f, "filesystem capacity is unavailable"),
            Self::InsufficientSpace {
                available,
                required,
            } => {
                write!(
                    f,
                    "insufficient filesystem space: {available} available, {required} required"
                )
            }
            Self::ArithmeticOverflow => write!(f, "filesystem space estimate overflow"),
            Self::InvalidAllocationUnit => write!(f, "filesystem allocation unit is invalid"),
            Self::Io(message) => write!(f, "filesystem space operation failed: {message}"),
        }
    }
}

impl std::error::Error for SpaceGuardError {}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct FilesystemSnapshot {
    pub filesystem_id: u64,
    pub allocation_unit: u64,
    pub available_bytes: u64,
}

impl FilesystemSnapshot {
    pub fn probe(root: &Path) -> Result<Self, SpaceGuardError> {
        let canonical_root = fs::canonicalize(root).map_err(io_error)?;
        if !canonical_root.is_dir() {
            return Err(SpaceGuardError::UnknownCapacity);
        }
        let file = File::open(&canonical_root).map_err(io_error)?;
        let stats = fstatvfs(&file).map_err(io_error)?;
        let allocation_unit = stats.f_frsize;
        if allocation_unit == 0 {
            return Err(SpaceGuardError::InvalidAllocationUnit);
        }
        let available_bytes = stats
            .f_bavail
            .checked_mul(allocation_unit)
            .ok_or(SpaceGuardError::ArithmeticOverflow)?;
        let filesystem_id = fs::metadata(&canonical_root).map_err(io_error)?.dev();
        Ok(Self {
            filesystem_id,
            allocation_unit,
            available_bytes,
        })
    }
}

#[derive(Debug)]
pub struct FilesystemLock {
    _file: File,
    root: File,
    root_path: PathBuf,
    root_inode: u64,
    _lock_dir: Arc<File>,
    lock_dir_path: PathBuf,
    pub filesystem_id: u64,
    _lock_path: PathBuf,
}

#[derive(Debug)]
pub struct FilesystemReservation {
    _file: File,
    path: PathBuf,
}

impl Drop for FilesystemReservation {
    fn drop(&mut self) {
        if let Err(err) = fs::remove_file(&self.path) {
            if err.kind() != std::io::ErrorKind::NotFound {
                eprintln!("[space_guard] reservation cleanup failed: {err}");
            }
        }
    }
}

impl FilesystemLock {
    pub fn acquire(root: &Path, runtime_dir: &Path) -> Result<Self, SpaceGuardError> {
        let canonical_root = fs::canonicalize(root).map_err(io_error)?;
        let root = File::open(&canonical_root).map_err(io_error)?;
        let root_metadata = root.metadata().map_err(io_error)?;
        if !root_metadata.is_dir() {
            return Err(SpaceGuardError::UnknownCapacity);
        }
        let filesystem_id = root_metadata.dev();
        let root_inode = root_metadata.ino();
        let (lock_dir_path, lock_dir) = ensure_lock_dir(runtime_dir)?;
        let lock_path = lock_dir_path.join(format!("fs-{filesystem_id}.lock"));
        let lock_name = format!("fs-{filesystem_id}.lock");
        let fd = openat(
            lock_dir.as_ref(),
            lock_name.as_str(),
            OFlags::RDWR | OFlags::CREATE | OFlags::CLOEXEC | OFlags::NOFOLLOW,
            Mode::from_bits_truncate(0o600),
        )
        .map_err(io_error)?;
        let file = File::from(fd);
        match flock(&file, FlockOperation::NonBlockingLockExclusive) {
            Ok(()) => Ok(Self {
                _file: file,
                root,
                root_path: canonical_root,
                root_inode,
                _lock_dir: lock_dir,
                lock_dir_path,
                filesystem_id,
                _lock_path: lock_path,
            }),
            Err(err) if err.kind() == std::io::ErrorKind::WouldBlock => Err(SpaceGuardError::Busy),
            Err(err) => Err(io_error(err)),
        }
    }

    pub fn probe(&self) -> Result<FilesystemSnapshot, SpaceGuardError> {
        let current = fs::metadata(&self.root_path).map_err(io_error)?;
        if !current.is_dir()
            || current.dev() != self.filesystem_id
            || current.ino() != self.root_inode
        {
            return Err(SpaceGuardError::UnknownCapacity);
        }
        let stats = fstatvfs(&self.root).map_err(io_error)?;
        let allocation_unit = stats.f_frsize;
        if allocation_unit == 0 {
            return Err(SpaceGuardError::InvalidAllocationUnit);
        }
        let available_bytes = stats
            .f_bavail
            .checked_mul(allocation_unit)
            .ok_or(SpaceGuardError::ArithmeticOverflow)?;
        Ok(FilesystemSnapshot {
            filesystem_id: self.filesystem_id,
            allocation_unit,
            available_bytes,
        })
    }

    pub fn active_reservation_bytes(&self) -> Result<u64, SpaceGuardError> {
        let mut total = 0u64;
        for entry in fs::read_dir(&self.lock_dir_path).map_err(io_error)? {
            let entry = entry.map_err(io_error)?;
            let name = entry.file_name();
            let name = name.to_string_lossy();
            let Some(reservation_id) = name
                .strip_prefix(&format!("fs-{}-", self.filesystem_id))
                .and_then(|name| name.strip_suffix(".reserve"))
            else {
                continue;
            };
            if reservation_id.is_empty()
                || !reservation_id
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
            {
                continue;
            }
            let fd = match openat(
                self._lock_dir.as_ref(),
                name.as_ref(),
                OFlags::RDWR | OFlags::CLOEXEC | OFlags::NOFOLLOW,
                Mode::empty(),
            ) {
                Ok(fd) => fd,
                Err(err) if err.kind() == std::io::ErrorKind::NotFound => continue,
                Err(err) => return Err(io_error(err)),
            };
            let mut file = File::from(fd);
            match flock(&file, FlockOperation::NonBlockingLockExclusive) {
                Ok(()) => {
                    drop(file);
                    match fs::remove_file(entry.path()) {
                        Ok(()) => {}
                        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {}
                        Err(err) => return Err(io_error(err)),
                    }
                }
                Err(err) if err.kind() == std::io::ErrorKind::WouldBlock => {
                    let mut encoded = [0u8; 8];
                    file.read_exact(&mut encoded).map_err(io_error)?;
                    total = total
                        .checked_add(u64::from_le_bytes(encoded))
                        .ok_or(SpaceGuardError::ArithmeticOverflow)?;
                }
                Err(err) => return Err(io_error(err)),
            }
        }
        Ok(total)
    }

    pub fn reserve(&self, bytes: u64) -> Result<FilesystemReservation, SpaceGuardError> {
        let process_id = std::process::id();
        loop {
            let sequence = NEXT_RESERVATION_ID.fetch_add(1, Ordering::Relaxed);
            let file_name = format!("fs-{}-{process_id}-{sequence}.reserve", self.filesystem_id);
            let path = self.lock_dir_path.join(&file_name);
            let fd = match openat(
                self._lock_dir.as_ref(),
                file_name.as_str(),
                OFlags::RDWR | OFlags::CREATE | OFlags::EXCL | OFlags::CLOEXEC | OFlags::NOFOLLOW,
                Mode::from_bits_truncate(0o600),
            ) {
                Ok(fd) => fd,
                Err(err) if err.kind() == std::io::ErrorKind::AlreadyExists => continue,
                Err(err) => return Err(io_error(err)),
            };
            let mut file = File::from(fd);
            flock(&file, FlockOperation::NonBlockingLockExclusive).map_err(io_error)?;
            file.write_all(&bytes.to_le_bytes()).map_err(io_error)?;
            return Ok(FilesystemReservation { _file: file, path });
        }
    }

    pub fn acquire_wait(
        root: &Path,
        runtime_dir: &Path,
        timeout: std::time::Duration,
    ) -> Result<Self, SpaceGuardError> {
        let started = std::time::Instant::now();
        loop {
            match Self::acquire(root, runtime_dir) {
                Ok(lock) => return Ok(lock),
                Err(SpaceGuardError::Busy) if started.elapsed() < timeout => {
                    std::thread::sleep(std::time::Duration::from_millis(10));
                }
                Err(err) => return Err(err),
            }
        }
    }
}

pub fn round_up_to_allocation_unit(
    bytes: u64,
    allocation_unit: u64,
) -> Result<u64, SpaceGuardError> {
    if allocation_unit == 0 {
        return Err(SpaceGuardError::InvalidAllocationUnit);
    }
    if bytes == 0 {
        return Ok(0);
    }
    let remainder = bytes % allocation_unit;
    if remainder == 0 {
        return Ok(bytes);
    }
    bytes
        .checked_add(allocation_unit - remainder)
        .ok_or(SpaceGuardError::ArithmeticOverflow)
}

pub fn ensure_capacity(
    available_bytes: u64,
    peak_extra_bytes: u64,
    reserve_bytes: u64,
) -> Result<(), SpaceGuardError> {
    let required = peak_extra_bytes
        .checked_add(reserve_bytes)
        .ok_or(SpaceGuardError::ArithmeticOverflow)?;
    if available_bytes < required {
        return Err(SpaceGuardError::InsufficientSpace {
            available: available_bytes,
            required,
        });
    }
    Ok(())
}

pub fn estimate_write_peak_bytes(
    payload_bytes: u64,
    blob_count: u64,
    touched_segments: u64,
    metadata_temporary_bytes: u64,
    allocation_unit: u64,
) -> Result<u64, SpaceGuardError> {
    let payload = round_up_to_allocation_unit(payload_bytes, allocation_unit)?;
    let records = blob_count
        .checked_mul(BLOB_RECORD_OVERHEAD)
        .ok_or(SpaceGuardError::ArithmeticOverflow)?;
    let segments = touched_segments
        .checked_mul(SEGMENT_OVERHEAD)
        .ok_or(SpaceGuardError::ArithmeticOverflow)?;
    let metadata = round_up_to_allocation_unit(metadata_temporary_bytes, allocation_unit)?;
    [payload, records, segments, metadata]
        .into_iter()
        .try_fold(0u64, |sum, value| {
            sum.checked_add(value)
                .ok_or(SpaceGuardError::ArithmeticOverflow)
        })
}

fn ensure_lock_dir(runtime_dir: &Path) -> Result<(PathBuf, Arc<File>), SpaceGuardError> {
    let lock_dir = runtime_dir.join("storage-locks");
    {
        let handles = lock_dir_handles().lock().map_err(|_| {
            SpaceGuardError::Io("storage lock directory cache is unavailable".into())
        })?;
        if let Some(handle) = handles.get(&lock_dir) {
            return Ok((lock_dir, Arc::clone(handle)));
        }
    }
    fs::create_dir_all(runtime_dir).map_err(io_error)?;
    let metadata = fs::symlink_metadata(runtime_dir).map_err(io_error)?;
    if !metadata.is_dir() || metadata.file_type().is_symlink() {
        return Err(SpaceGuardError::Io(
            "runtime directory is not a real directory".to_string(),
        ));
    }
    match fs::create_dir(&lock_dir) {
        Ok(()) => {}
        Err(err) if err.kind() == std::io::ErrorKind::AlreadyExists => {}
        Err(err) => return Err(io_error(err)),
    }
    let metadata = fs::symlink_metadata(&lock_dir).map_err(io_error)?;
    if !metadata.is_dir() || metadata.file_type().is_symlink() {
        return Err(SpaceGuardError::Io(
            "storage lock directory is unsafe".to_string(),
        ));
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&lock_dir, fs::Permissions::from_mode(0o700)).map_err(io_error)?;
    }
    let mut handles = lock_dir_handles()
        .lock()
        .map_err(|_| SpaceGuardError::Io("storage lock directory cache is unavailable".into()))?;
    if let Some(handle) = handles.get(&lock_dir) {
        return Ok((lock_dir, Arc::clone(handle)));
    }
    let fd = open(
        &lock_dir,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC | OFlags::NOFOLLOW,
        Mode::empty(),
    )
    .map_err(io_error)?;
    let handle = Arc::new(File::from(fd));
    if !handle.metadata().map_err(io_error)?.is_dir() {
        return Err(SpaceGuardError::Io(
            "storage lock path is not a directory".into(),
        ));
    }
    handles.insert(lock_dir.clone(), Arc::clone(&handle));
    Ok((lock_dir, handle))
}

fn io_error(err: impl std::fmt::Display) -> SpaceGuardError {
    SpaceGuardError::Io(err.to_string())
}

pub fn runtime_dir() -> PathBuf {
    storage_runtime_dir()
}
