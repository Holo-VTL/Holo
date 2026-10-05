use std::ffi::OsStr;
use std::fs;
use std::path::{Path, PathBuf};

use super::layout_lease::LayoutLease;
use super::metadata::{
    load_checkpoint_page, persist_checkpoint_page, storage_root_dir_for_cartridge, CheckpointFlags,
    MetadataCheckpoint, StorageError,
};
use super::segment::{read_segment_file, validate_segment_shape, write_segment_file};
use super::segment_index::{data_segment_path, initialize_segment_index};

pub const STORAGE_LAYOUT_MAGIC: u32 = 0x56544C58;
pub const STORAGE_LAYOUT_VERSION_V1: u16 = 1;
pub const STORAGE_LAYOUT_VERSION: u16 = 2;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u16)]
pub enum SegmentKind {
    Data = 1,
    Metadata = 2,
    BlkMap = 3,
    Lookup = 4,
    Reclaim = 5,
    Dedup = 6,
    SegmentIndex = 7,
}

impl SegmentKind {
    pub fn from_u16(v: u16) -> Option<Self> {
        match v {
            1 => Some(Self::Data),
            2 => Some(Self::Metadata),
            3 => Some(Self::BlkMap),
            4 => Some(Self::Lookup),
            5 => Some(Self::Reclaim),
            6 => Some(Self::Dedup),
            7 => Some(Self::SegmentIndex),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SegmentHeader {
    pub magic: u32,
    pub version: u16,
    pub kind: SegmentKind,
    pub segment_id: u64,
    pub sequence: u64,
    pub payload_len: u64,
    pub checksum: u32,
}

impl SegmentHeader {
    pub fn new(kind: SegmentKind, segment_id: u64, sequence: u64, payload: &[u8]) -> Self {
        Self {
            magic: STORAGE_LAYOUT_MAGIC,
            version: STORAGE_LAYOUT_VERSION,
            kind,
            segment_id,
            sequence,
            payload_len: payload.len() as u64,
            checksum: integrity32(payload),
        }
    }

    pub fn validate(&self, payload: &[u8]) -> Result<(), StorageError> {
        if self.magic != STORAGE_LAYOUT_MAGIC {
            return Err(StorageError::Corrupt("invalid magic".to_string()));
        }
        if self.version != STORAGE_LAYOUT_VERSION && self.version != STORAGE_LAYOUT_VERSION_V1 {
            return Err(StorageError::VersionMismatch {
                expected: STORAGE_LAYOUT_VERSION,
                got: self.version,
            });
        }
        if self.payload_len != payload.len() as u64 {
            return Err(StorageError::Corrupt("payload length mismatch".to_string()));
        }
        let checksum = if self.version == STORAGE_LAYOUT_VERSION_V1 {
            checksum32(payload)
        } else {
            integrity32(payload)
        };
        if self.version == STORAGE_LAYOUT_VERSION_V1 && self.checksum == 0 {
            return Ok(());
        }
        if self.checksum != checksum {
            return Err(StorageError::Corrupt(
                "payload checksum mismatch".to_string(),
            ));
        }
        Ok(())
    }

    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::with_capacity(36);
        out.extend_from_slice(&self.magic.to_le_bytes());
        out.extend_from_slice(&self.version.to_le_bytes());
        out.extend_from_slice(&(self.kind as u16).to_le_bytes());
        out.extend_from_slice(&self.segment_id.to_le_bytes());
        out.extend_from_slice(&self.sequence.to_le_bytes());
        out.extend_from_slice(&self.payload_len.to_le_bytes());
        out.extend_from_slice(&self.checksum.to_le_bytes());
        out
    }

    pub fn decode(buf: &[u8]) -> Result<Self, StorageError> {
        if buf.len() < 36 {
            return Err(StorageError::Corrupt(
                "segment header too short".to_string(),
            ));
        }
        let magic = u32::from_le_bytes(
            buf[0..4]
                .try_into()
                .map_err(|_| StorageError::Corrupt("segment header parse failed".to_string()))?,
        );
        let version = u16::from_le_bytes(
            buf[4..6]
                .try_into()
                .map_err(|_| StorageError::Corrupt("segment header parse failed".to_string()))?,
        );
        let kind_raw = u16::from_le_bytes(
            buf[6..8]
                .try_into()
                .map_err(|_| StorageError::Corrupt("segment header parse failed".to_string()))?,
        );
        let kind = SegmentKind::from_u16(kind_raw)
            .ok_or_else(|| StorageError::Corrupt("unknown segment kind".to_string()))?;
        let segment_id = u64::from_le_bytes(
            buf[8..16]
                .try_into()
                .map_err(|_| StorageError::Corrupt("segment header parse failed".to_string()))?,
        );
        let sequence = u64::from_le_bytes(
            buf[16..24]
                .try_into()
                .map_err(|_| StorageError::Corrupt("segment header parse failed".to_string()))?,
        );
        let payload_len = u64::from_le_bytes(
            buf[24..32]
                .try_into()
                .map_err(|_| StorageError::Corrupt("segment header parse failed".to_string()))?,
        );
        let checksum = u32::from_le_bytes(
            buf[32..36]
                .try_into()
                .map_err(|_| StorageError::Corrupt("segment header parse failed".to_string()))?,
        );

        Ok(Self {
            magic,
            version,
            kind,
            segment_id,
            sequence,
            payload_len,
            checksum,
        })
    }
}

#[derive(Debug, Clone)]
pub struct LayoutPaths {
    pub root: PathBuf,
    // Legacy data.segment path. Fresh writes use data_XXXXXX.seg via segment_index.segment.
    pub data_file: PathBuf,
    pub metadata_file: PathBuf,
    pub blk_map_file: PathBuf,
    pub lookup_file: PathBuf,
    pub reclaim_file: PathBuf,
    pub dedup_file: PathBuf,
    pub segment_index_file: PathBuf,
}

impl LayoutPaths {
    pub fn for_cartridge(root: &Path, library_id: &str, cartridge_id: &str) -> Self {
        let dir = canonical_cartridge_dir(root, library_id, cartridge_id);
        Self {
            root: dir.clone(),
            data_file: dir.join("data.segment"),
            metadata_file: dir.join("metadata.segment"),
            blk_map_file: dir.join("blk_map.segment"),
            lookup_file: dir.join("lookup.segment"),
            reclaim_file: dir.join("reclaim.segment"),
            dedup_file: dir.join("dedup.segment"),
            segment_index_file: dir.join("segment_index.segment"),
        }
    }

    pub fn usage_counters_file(&self) -> PathBuf {
        self.root.join("usage.counters")
    }
}

fn layout_scope_key(library_id: &str) -> String {
    let scope = sanitize_id(library_id);
    if scope.is_empty() {
        "global".to_string()
    } else {
        scope
    }
}

fn canonical_cartridge_dir(root: &Path, library_id: &str, cartridge_id: &str) -> PathBuf {
    root.join("cartridges")
        .join(layout_scope_key(library_id))
        .join(sanitize_id(cartridge_id))
}

fn legacy_cartridge_dir(root: &Path, drive_id: &str, cartridge_id: &str) -> PathBuf {
    root.join(sanitize_id(drive_id))
        .join(sanitize_id(cartridge_id))
}

fn discover_legacy_dirs(root: &Path, cartridge_id: &str) -> Result<Vec<PathBuf>, StorageError> {
    let cartridge = sanitize_id(cartridge_id);
    let mut dirs = Vec::new();
    let entries = match fs::read_dir(root) {
        Ok(entries) => entries,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(dirs),
        Err(err) => return Err(StorageError::Io(err)),
    };
    for entry in entries {
        let entry = entry?;
        let path = entry.path();
        if entry.file_name() == OsStr::new("cartridges") {
            continue;
        }
        let entry_type = entry.file_type()?;
        if entry_type.is_symlink() {
            return Err(StorageError::Conflict("identity_conflict".to_string()));
        }
        if !entry_type.is_dir() {
            continue;
        }
        let candidate = path.join(&cartridge);
        if is_layout_directory(&candidate)? {
            dirs.push(candidate);
        }
    }
    Ok(dirs)
}

fn resolve_layout_dir(
    root: &Path,
    library_id: &str,
    drive_id: &str,
    cartridge_id: &str,
) -> Result<PathBuf, StorageError> {
    let canonical = canonical_cartridge_dir(root, library_id, cartridge_id);
    let mut candidates = Vec::new();
    if is_layout_directory(&canonical)? {
        candidates.push(canonical.clone());
    }
    let preferred = legacy_cartridge_dir(root, drive_id, cartridge_id);
    if is_layout_directory(&preferred)? {
        candidates.push(preferred);
    }
    for candidate in discover_legacy_dirs(root, cartridge_id)? {
        if !candidates.iter().any(|existing| existing == &candidate) {
            candidates.push(candidate);
        }
    }
    match candidates.len() {
        0 => Ok(canonical),
        1 => Ok(candidates.remove(0)),
        _ => Err(StorageError::Conflict("ambiguous_layout".to_string())),
    }
}

fn is_layout_directory(path: &Path) -> Result<bool, StorageError> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() => {
            Err(StorageError::Conflict("identity_conflict".to_string()))
        }
        Ok(metadata) => Ok(metadata.is_dir()),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(err) => Err(StorageError::Io(err)),
    }
}

#[derive(Debug, Clone)]
pub struct LayoutSnapshot {
    pub paths: LayoutPaths,
    pub checkpoint: MetadataCheckpoint,
}

pub fn bootstrap_for_mount(
    library_id: &str,
    drive_id: &str,
    cartridge_id: &str,
) -> Result<LayoutSnapshot, StorageError> {
    bootstrap_for_mount_inner(library_id, drive_id, cartridge_id, None)
}

pub fn bootstrap_for_mount_with_lease(
    library_id: &str,
    drive_id: &str,
    cartridge_id: &str,
    lease: &LayoutLease,
) -> Result<LayoutSnapshot, StorageError> {
    bootstrap_for_mount_inner(library_id, drive_id, cartridge_id, Some(lease))
}

fn bootstrap_for_mount_inner(
    library_id: &str,
    drive_id: &str,
    cartridge_id: &str,
    lease: Option<&LayoutLease>,
) -> Result<LayoutSnapshot, StorageError> {
    let root = storage_root_dir_for_cartridge(cartridge_id);
    if let Some(lease) = lease {
        lease
            .verify_pool_root(&root)
            .map_err(super::layout_lease::storage_error_from_lease)?;
    }
    let paths = resolve_layout_paths(&root, library_id, drive_id, cartridge_id)?;
    if let Some(lease) = lease {
        lease
            .verify_pool_root(&root)
            .map_err(super::layout_lease::storage_error_from_lease)?;
    }
    initialize_layout(&paths)
}

pub fn resolve_layout_paths(
    root: &Path,
    library_id: &str,
    drive_id: &str,
    cartridge_id: &str,
) -> Result<LayoutPaths, StorageError> {
    let layout_root = resolve_layout_dir(root, library_id, drive_id, cartridge_id)?;
    Ok(LayoutPaths {
        root: layout_root.clone(),
        data_file: layout_root.join("data.segment"),
        metadata_file: layout_root.join("metadata.segment"),
        blk_map_file: layout_root.join("blk_map.segment"),
        lookup_file: layout_root.join("lookup.segment"),
        reclaim_file: layout_root.join("reclaim.segment"),
        dedup_file: layout_root.join("dedup.segment"),
        segment_index_file: layout_root.join("segment_index.segment"),
    })
}

pub fn initialize_layout(paths: &LayoutPaths) -> Result<LayoutSnapshot, StorageError> {
    fs::create_dir_all(&paths.root)?;

    let _ = initialize_segment_index(paths)?;
    if !paths.blk_map_file.exists() {
        write_segment_file(&paths.blk_map_file, SegmentKind::BlkMap, 2, 0, &[])?;
    }
    if !paths.lookup_file.exists() {
        write_segment_file(&paths.lookup_file, SegmentKind::Lookup, 3, 0, &[])?;
    }
    if !paths.reclaim_file.exists() {
        write_segment_file(&paths.reclaim_file, SegmentKind::Reclaim, 4, 0, &[])?;
    }
    if !paths.dedup_file.exists() {
        write_segment_file(&paths.dedup_file, SegmentKind::Dedup, 5, 0, &[])?;
    }

    if !paths.metadata_file.exists() {
        let checkpoint = MetadataCheckpoint {
            page_id: 1,
            epoch: 1,
            active_blk_map_root: 2,
            active_lookup_root: 3,
            flags: CheckpointFlags::Clean,
        };
        persist_checkpoint_page(&paths.metadata_file, &checkpoint)?;
    }

    load_layout(paths)
}

pub fn load_layout(paths: &LayoutPaths) -> Result<LayoutSnapshot, StorageError> {
    if !paths.root.exists() {
        return Err(StorageError::NotFound("layout root not found".to_string()));
    }

    let index = initialize_segment_index(paths)?;
    for descriptor in &index.descriptors {
        let _ = validate_segment_shape(
            &data_segment_path(paths, descriptor.segment_seq),
            SegmentKind::Data,
        )?;
    }
    let _ = read_segment_file(&paths.blk_map_file, SegmentKind::BlkMap)?;
    let _ = read_segment_file(&paths.lookup_file, SegmentKind::Lookup)?;
    let _ = read_segment_file(&paths.reclaim_file, SegmentKind::Reclaim)?;
    let _ = read_segment_file(&paths.dedup_file, SegmentKind::Dedup)?;
    let checkpoint = load_checkpoint_page(&paths.metadata_file)?;

    Ok(LayoutSnapshot {
        paths: paths.clone(),
        checkpoint,
    })
}

pub fn sanitize_id(raw: &str) -> String {
    let mut out = String::with_capacity(raw.len());
    for ch in raw.chars() {
        if ch.is_ascii_alphanumeric() || ch == '-' || ch == '_' {
            out.push(ch.to_ascii_lowercase());
        } else {
            out.push('_');
        }
    }
    if out.is_empty() {
        "unknown".to_string()
    } else {
        out
    }
}

pub fn checksum32(data: &[u8]) -> u32 {
    let mut acc: u32 = 0x811C9DC5;
    for b in data {
        acc ^= *b as u32;
        acc = acc.wrapping_mul(16777619);
    }
    acc
}

pub fn integrity32(data: &[u8]) -> u32 {
    crc32c::crc32c(data)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn unique_root(test_name: &str) -> PathBuf {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let root = std::env::temp_dir().join(format!("holo-layout-{test_name}-{nanos}"));
        fs::create_dir_all(&root).expect("create test root");
        root
    }

    #[test]
    fn resolver_rejects_canonical_and_legacy_layout_ambiguity() {
        let root = unique_root("canonical");
        let canonical = canonical_cartridge_dir(&root, "library-a", "VTL000001");
        let legacy = root.join("drive-a").join("vtl000001");
        fs::create_dir_all(&canonical).expect("create canonical");
        fs::create_dir_all(&legacy).expect("create legacy");

        let err = resolve_layout_dir(&root, "library-a", "drive-a", "VTL000001")
            .expect_err("canonical and legacy layouts must not be guessed");
        assert_eq!(err.to_string(), "conflict: ambiguous_layout");
        assert!(
            canonical.is_dir() && legacy.is_dir(),
            "resolver must preserve both layouts"
        );
    }

    #[test]
    fn resolver_rejects_multiple_legacy_layouts_without_scoring() {
        let root = unique_root("legacy-share");
        let drive_a = root.join("drive-a").join("vtl000001");
        let drive_b = root.join("drive-b").join("vtl000001");
        fs::create_dir_all(&drive_a).expect("create drive-a");
        fs::create_dir_all(&drive_b).expect("create drive-b");
        fs::write(drive_a.join("data.segment"), vec![0u8; 1024]).expect("write drive-a data");
        fs::write(drive_b.join("data.segment"), vec![0u8; 64]).expect("write drive-b data");

        let err = resolve_layout_dir(&root, "library-a", "drive-b", "VTL000001")
            .expect_err("multiple legacy layouts must not be selected by size");
        assert_eq!(err.to_string(), "conflict: ambiguous_layout");
        assert_eq!(
            fs::metadata(drive_a.join("data.segment"))
                .expect("drive-a data")
                .len(),
            1024
        );
        assert_eq!(
            fs::metadata(drive_b.join("data.segment"))
                .expect("drive-b data")
                .len(),
            64
        );
    }

    #[test]
    fn layout_scope_uses_explicit_library_identity() {
        assert_eq!(layout_scope_key("library-a"), "library-a");
        assert_eq!(layout_scope_key("library-a__drive-a"), "library-a__drive-a");
    }

    #[test]
    fn resolver_reuses_one_legacy_layout_only_when_unique() {
        let root = unique_root("legacy-unique");
        let legacy = root.join("drive-a").join("vtl000001");
        fs::create_dir_all(&legacy).expect("create legacy layout");

        let resolved = resolve_layout_dir(&root, "library-a", "drive-a", "VTL000001")
            .expect("single legacy layout should resolve");
        assert_eq!(resolved, legacy);
    }
}
