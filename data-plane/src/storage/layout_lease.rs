use std::fs::{self, File};
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use rustix::fs::{flock, open, FlockOperation, Mode, OFlags};

use super::layout::sanitize_id;
use super::metadata::StorageError;

#[derive(Debug)]
pub enum LayoutLeaseError {
    Busy,
    InvalidIdentity,
    IdentityConflict,
    Io(std::io::Error),
}

impl From<std::io::Error> for LayoutLeaseError {
    fn from(value: std::io::Error) -> Self {
        Self::Io(value)
    }
}

impl std::fmt::Display for LayoutLeaseError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Busy => write!(f, "cartridge layout is busy"),
            Self::InvalidIdentity => write!(f, "invalid cartridge layout identity"),
            Self::IdentityConflict => write!(f, "identity_conflict"),
            Self::Io(err) => write!(f, "layout lease I/O failed: {err}"),
        }
    }
}

impl std::error::Error for LayoutLeaseError {}

#[derive(Clone, Debug)]
pub struct LayoutLease(Arc<LeaseInner>);

#[derive(Debug)]
struct LeaseInner {
    _file: File,
    _lock_path: PathBuf,
    root_device: u64,
    root_inode: u64,
}

impl LayoutLease {
    pub fn acquire(
        pool_root: &Path,
        cartridge_id: &str,
        runtime_dir: &Path,
    ) -> Result<Self, LayoutLeaseError> {
        if !valid_management_id(cartridge_id) {
            return Err(LayoutLeaseError::InvalidIdentity);
        }
        let canonical_root = fs::canonicalize(pool_root)?;
        let (root_device, root_inode) = pool_root_identity(&canonical_root)?;
        let key = layout_lease_key(root_device, root_inode, cartridge_id);
        let lock_dir = ensure_lock_dir(runtime_dir)?;
        let lock_path = lock_dir.join(format!("layout-{key}.lock"));
        let file = open_lock_file(&lock_path)?;
        match flock(&file, FlockOperation::NonBlockingLockExclusive) {
            Ok(()) => Ok(Self(Arc::new(LeaseInner {
                _file: file,
                _lock_path: lock_path,
                root_device,
                root_inode,
            }))),
            Err(err) if err.kind() == std::io::ErrorKind::WouldBlock => Err(LayoutLeaseError::Busy),
            Err(err) => Err(LayoutLeaseError::Io(err.into())),
        }
    }

    pub fn lock_path(&self) -> &Path {
        &self.0._lock_path
    }

    pub fn verify_pool_root(&self, pool_root: &Path) -> Result<(), LayoutLeaseError> {
        let canonical =
            fs::canonicalize(pool_root).map_err(|_| LayoutLeaseError::IdentityConflict)?;
        let (device, inode) =
            pool_root_identity(&canonical).map_err(|_| LayoutLeaseError::IdentityConflict)?;
        if device != self.0.root_device || inode != self.0.root_inode {
            return Err(LayoutLeaseError::IdentityConflict);
        }
        Ok(())
    }
}

pub fn layout_lease_key(device: u64, inode: u64, cartridge_id: &str) -> String {
    use sha2::{Digest, Sha256};

    let mut digest = Sha256::new();
    digest.update(b"holo-layout-v2");
    digest.update([0]);
    digest.update(device.to_string().as_bytes());
    digest.update([0]);
    digest.update(inode.to_string().as_bytes());
    digest.update([0]);
    digest.update(sanitize_id(cartridge_id).as_bytes());
    format!("{:x}", digest.finalize())
}

fn pool_root_identity(root: &Path) -> Result<(u64, u64), LayoutLeaseError> {
    let fd = open(
        root,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC | OFlags::NOFOLLOW,
        Mode::empty(),
    )
    .map_err(|err| LayoutLeaseError::Io(err.into()))?;
    let directory = File::from(fd);
    let metadata = directory.metadata()?;
    if !metadata.is_dir() {
        return Err(LayoutLeaseError::InvalidIdentity);
    }
    Ok((metadata.dev(), metadata.ino()))
}

pub fn storage_runtime_dir() -> PathBuf {
    std::env::var_os("HOLO_RUN_DIR")
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            if cfg!(test) {
                std::env::temp_dir().join(format!("holo-runtime-test-{}", std::process::id()))
            } else if cfg!(target_os = "linux") {
                PathBuf::from("/run/holo")
            } else {
                std::env::temp_dir().join("holo-runtime")
            }
        })
}

fn valid_management_id(value: &str) -> bool {
    let bytes = value.as_bytes();
    !bytes.is_empty()
        && bytes.len() <= 128
        && bytes[0].is_ascii_alphanumeric()
        && !value.contains("..")
        && bytes.iter().all(|byte| {
            byte.is_ascii_alphanumeric() || matches!(*byte, b'.' | b'_' | b':' | b' ' | b'-')
        })
}

fn ensure_lock_dir(runtime_dir: &Path) -> Result<PathBuf, LayoutLeaseError> {
    fs::create_dir_all(runtime_dir)?;
    let runtime_meta = fs::symlink_metadata(runtime_dir)?;
    if !runtime_meta.is_dir() || runtime_meta.file_type().is_symlink() {
        return Err(LayoutLeaseError::InvalidIdentity);
    }
    let lock_dir = runtime_dir.join("storage-locks");
    match fs::create_dir(&lock_dir) {
        Ok(()) => {}
        Err(err) if err.kind() == std::io::ErrorKind::AlreadyExists => {}
        Err(err) => return Err(LayoutLeaseError::Io(err)),
    }
    let metadata = fs::symlink_metadata(&lock_dir)?;
    if !metadata.is_dir() || metadata.file_type().is_symlink() {
        return Err(LayoutLeaseError::InvalidIdentity);
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&lock_dir, fs::Permissions::from_mode(0o700))?;
    }
    Ok(lock_dir)
}

fn open_lock_file(path: &Path) -> Result<File, LayoutLeaseError> {
    let fd = open(
        path,
        OFlags::RDWR | OFlags::CREATE | OFlags::CLOEXEC | OFlags::NOFOLLOW,
        Mode::from_bits_truncate(0o600),
    )
    .map_err(|err| LayoutLeaseError::Io(err.into()))?;
    let file = File::from(fd);
    let metadata = file.metadata()?;
    if !metadata.is_file() {
        return Err(LayoutLeaseError::InvalidIdentity);
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        file.set_permissions(fs::Permissions::from_mode(0o600))?;
    }
    Ok(file)
}

pub(crate) fn storage_error_from_lease(err: LayoutLeaseError) -> StorageError {
    match err {
        LayoutLeaseError::Busy => StorageError::Conflict("layout is busy".to_string()),
        LayoutLeaseError::InvalidIdentity => {
            StorageError::Conflict("invalid cartridge layout identity".to_string())
        }
        LayoutLeaseError::IdentityConflict => {
            StorageError::Conflict("identity_conflict".to_string())
        }
        LayoutLeaseError::Io(err) => StorageError::Io(err),
    }
}
