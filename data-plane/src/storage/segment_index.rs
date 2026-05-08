use std::collections::HashMap;
use std::env;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, OnceLock};

use super::compression::CompressionCodec;
use super::layout::{checksum32, LayoutPaths, SegmentKind};
use super::metadata::{lock_storage_mutex, modified_nanos_from_result, StorageError};
use super::segment::{read_segment_file, validate_segment_shape, write_segment_file};

pub const DEFAULT_MAX_DATA_SEGMENT_SIZE: u32 = 256 * 1024 * 1024;
const SEGMENT_INDEX_PREFIX: &[u8; 4] = b"SDI1";
const SEGMENT_DESCRIPTOR_SIZE: usize = 40;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum SegmentState {
    Active = 1,
    Sealed = 2,
    Compacting = 3,
    Reclaimable = 4,
}

impl SegmentState {
    fn from_u8(raw: u8) -> Result<Self, StorageError> {
        match raw {
            1 => Ok(Self::Active),
            2 => Ok(Self::Sealed),
            3 => Ok(Self::Compacting),
            4 => Ok(Self::Reclaimable),
            _ => Err(StorageError::Corrupt("invalid segment state".to_string())),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SegmentDescriptor {
    pub segment_seq: u32,
    pub payload_bytes: u64,
    pub first_blob_id: u64,
    pub last_blob_id: u64,
    pub state: SegmentState,
    pub compression: CompressionCodec,
    pub live_bytes: u32,
}

impl SegmentDescriptor {
    pub fn contains_blob(&self, blob_id: u64) -> bool {
        // first_blob_id == 0 marks an empty segment descriptor; generated blob ids start at 1.
        self.first_blob_id > 0 && blob_id >= self.first_blob_id && blob_id <= self.last_blob_id
    }

    fn encode(&self) -> Vec<u8> {
        let mut out = Vec::with_capacity(SEGMENT_DESCRIPTOR_SIZE);
        out.extend_from_slice(&self.segment_seq.to_le_bytes());
        out.extend_from_slice(&self.payload_bytes.to_le_bytes());
        out.extend_from_slice(&self.first_blob_id.to_le_bytes());
        out.extend_from_slice(&self.last_blob_id.to_le_bytes());
        out.push(self.state as u8);
        out.push(self.compression as u8);
        out.extend_from_slice(&self.live_bytes.to_le_bytes());
        let checksum = descriptor_checksum(&out);
        out.extend_from_slice(&checksum.to_le_bytes());
        out.extend_from_slice(&[0u8; 4]);
        out
    }

    fn decode(raw: &[u8]) -> Result<Self, StorageError> {
        if raw.len() != SEGMENT_DESCRIPTOR_SIZE {
            return Err(StorageError::Corrupt(
                "segment descriptor size mismatch".to_string(),
            ));
        }
        let checksum =
            u16::from_le_bytes(raw[34..36].try_into().map_err(|_| {
                StorageError::Corrupt("descriptor checksum parse failed".to_string())
            })?);
        if descriptor_checksum(&raw[..34]) != checksum {
            return Err(StorageError::Corrupt(
                "segment descriptor checksum mismatch".to_string(),
            ));
        }
        Ok(Self {
            segment_seq: u32::from_le_bytes(
                raw[0..4].try_into().map_err(|_| {
                    StorageError::Corrupt("descriptor seq parse failed".to_string())
                })?,
            ),
            payload_bytes: u64::from_le_bytes(raw[4..12].try_into().map_err(|_| {
                StorageError::Corrupt("descriptor payload parse failed".to_string())
            })?),
            first_blob_id: u64::from_le_bytes(raw[12..20].try_into().map_err(|_| {
                StorageError::Corrupt("descriptor first blob parse failed".to_string())
            })?),
            last_blob_id: u64::from_le_bytes(raw[20..28].try_into().map_err(|_| {
                StorageError::Corrupt("descriptor last blob parse failed".to_string())
            })?),
            state: SegmentState::from_u8(raw[28])?,
            compression: CompressionCodec::from_u8(raw[29])?,
            live_bytes: u32::from_le_bytes(raw[30..34].try_into().map_err(|_| {
                StorageError::Corrupt("descriptor live bytes parse failed".to_string())
            })?),
        })
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SegmentIndex {
    pub max_segment_size: u32,
    pub descriptors: Vec<SegmentDescriptor>,
    pub next_segment_seq: u64,
}

#[derive(Debug, Clone)]
struct CachedSegmentIndex {
    index: SegmentIndex,
    dirty: bool,
    generation: u64,
    stamp: Option<FileStamp>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct FileStamp {
    len: u64,
    modified_nanos: u128,
}

fn cache() -> &'static Mutex<HashMap<PathBuf, CachedSegmentIndex>> {
    static CACHE: OnceLock<Mutex<HashMap<PathBuf, CachedSegmentIndex>>> = OnceLock::new();
    CACHE.get_or_init(|| Mutex::new(HashMap::new()))
}

fn file_stamp(path: &Path) -> Result<FileStamp, StorageError> {
    let metadata = fs::metadata(path)?;
    let modified_nanos = modified_nanos_from_result(metadata.modified())?;
    Ok(FileStamp {
        len: metadata.len(),
        modified_nanos,
    })
}

impl SegmentIndex {
    pub fn new(max_segment_size: u32) -> Self {
        Self {
            max_segment_size,
            descriptors: vec![SegmentDescriptor {
                segment_seq: 0,
                payload_bytes: 0,
                first_blob_id: 0,
                last_blob_id: 0,
                state: SegmentState::Active,
                compression: CompressionCodec::None,
                live_bytes: 0,
            }],
            next_segment_seq: 1,
        }
    }

    pub fn active_descriptor(&self) -> Result<&SegmentDescriptor, StorageError> {
        self.descriptors
            .iter()
            .find(|descriptor| descriptor.state == SegmentState::Active)
            .ok_or_else(|| StorageError::Corrupt("active data segment missing".to_string()))
    }

    pub fn active_descriptor_mut(&mut self) -> Result<&mut SegmentDescriptor, StorageError> {
        self.descriptors
            .iter_mut()
            .find(|descriptor| descriptor.state == SegmentState::Active)
            .ok_or_else(|| StorageError::Corrupt("active data segment missing".to_string()))
    }

    pub fn descriptor_for_segment(&self, segment_id: u64) -> Option<&SegmentDescriptor> {
        self.descriptors
            .iter()
            .find(|descriptor| descriptor.segment_seq as u64 == segment_id)
    }

    pub fn descriptor_for_blob(&self, blob_id: u64) -> Option<&SegmentDescriptor> {
        self.descriptors
            .iter()
            .find(|descriptor| descriptor.contains_blob(blob_id))
    }

    fn encode(&self) -> Vec<u8> {
        let active_count = self
            .descriptors
            .iter()
            .filter(|descriptor| descriptor.state == SegmentState::Active)
            .count() as u32;
        let mut out = Vec::with_capacity(24 + self.descriptors.len() * SEGMENT_DESCRIPTOR_SIZE);
        out.extend_from_slice(SEGMENT_INDEX_PREFIX);
        out.extend_from_slice(&self.max_segment_size.to_le_bytes());
        out.extend_from_slice(&active_count.to_le_bytes());
        out.extend_from_slice(&(self.descriptors.len() as u32).to_le_bytes());
        out.extend_from_slice(&self.next_segment_seq.to_le_bytes());
        for descriptor in &self.descriptors {
            out.extend_from_slice(&descriptor.encode());
        }
        out
    }
}

pub fn configured_max_segment_size() -> u32 {
    env::var("HOLO_SEGMENT_SIZE")
        .ok()
        .and_then(|raw| raw.trim().parse::<u32>().ok())
        .filter(|value| *value > 0)
        .unwrap_or(DEFAULT_MAX_DATA_SEGMENT_SIZE)
}

pub fn data_segment_path(layout: &LayoutPaths, seq: u32) -> PathBuf {
    layout.root.join(format!("data_{seq:06}.seg"))
}

pub fn load_segment_index(path: &Path) -> Result<SegmentIndex, StorageError> {
    let (_header, payload) = read_segment_file(path, SegmentKind::SegmentIndex)?;
    decode_segment_index(&payload)
}

pub fn load_segment_index_for_append(path: &Path) -> Result<SegmentIndex, StorageError> {
    if let Some(cached) = lock_storage_mutex(cache(), "segment index")?
        .get(path)
        .cloned()
    {
        if cached.dirty {
            return Ok(cached.index);
        }
        let current_stamp = file_stamp(path).ok();
        if cached.stamp == current_stamp {
            return Ok(cached.index);
        }
    }
    let index = load_segment_index(path)?;
    let stamp = file_stamp(path).ok();
    lock_storage_mutex(cache(), "segment index")?.insert(
        path.to_path_buf(),
        CachedSegmentIndex {
            index: index.clone(),
            dirty: false,
            generation: 0,
            stamp,
        },
    );
    Ok(index)
}

pub fn store_segment_index_for_append(
    path: &Path,
    index: SegmentIndex,
) -> Result<(), StorageError> {
    let mut guard = lock_storage_mutex(cache(), "segment index")?;
    let generation = guard
        .get(path)
        .map(|cached| cached.generation.saturating_add(1))
        .unwrap_or(1);
    let stamp = guard.get(path).and_then(|cached| cached.stamp);
    guard.insert(
        path.to_path_buf(),
        CachedSegmentIndex {
            index,
            dirty: true,
            generation,
            stamp,
        },
    );
    Ok(())
}

pub fn store_segment_index_clean(path: &Path, index: SegmentIndex) -> Result<(), StorageError> {
    let mut guard = lock_storage_mutex(cache(), "segment index")?;
    let generation = guard.get(path).map(|cached| cached.generation).unwrap_or(0);
    guard.insert(
        path.to_path_buf(),
        CachedSegmentIndex {
            index,
            dirty: false,
            generation,
            stamp: file_stamp(path).ok(),
        },
    );
    Ok(())
}

pub fn invalidate_segment_index_cache(path: &Path) {
    match lock_storage_mutex(cache(), "segment index") {
        Ok(mut guard) => {
            guard.remove(path);
        }
        Err(err) => eprintln!("[storage] failed to invalidate segment index cache: {err}"),
    }
}

pub fn flush_segment_index_for_append(path: &Path) -> Result<(), StorageError> {
    let cached = lock_storage_mutex(cache(), "segment index")?
        .get(path)
        .cloned();
    let Some(cached) = cached else {
        return Ok(());
    };
    if !cached.dirty {
        return Ok(());
    }
    persist_segment_index(path, &cached.index)?;
    let stamp = file_stamp(path).ok();
    let mut guard = lock_storage_mutex(cache(), "segment index")?;
    if let Some(current) = guard.get_mut(path) {
        if current.generation == cached.generation {
            current.dirty = false;
            current.stamp = stamp;
        }
    }
    Ok(())
}

pub fn persist_segment_index(path: &Path, index: &SegmentIndex) -> Result<(), StorageError> {
    write_segment_file(
        path,
        SegmentKind::SegmentIndex,
        7,
        index.next_segment_seq,
        &index.encode(),
    )
}

pub fn initialize_segment_index(layout: &LayoutPaths) -> Result<SegmentIndex, StorageError> {
    if layout.segment_index_file.exists() {
        return load_segment_index(&layout.segment_index_file);
    }
    let index = SegmentIndex::new(configured_max_segment_size());
    let active_path = data_segment_path(layout, 0);
    if !active_path.exists() {
        write_segment_file(&active_path, SegmentKind::Data, 1, 0, &[])?;
    }
    persist_segment_index(&layout.segment_index_file, &index)?;
    Ok(index)
}

pub fn active_segment_path(
    layout: &LayoutPaths,
    index: &SegmentIndex,
) -> Result<PathBuf, StorageError> {
    Ok(data_segment_path(
        layout,
        index.active_descriptor()?.segment_seq,
    ))
}

pub fn prepare_active_segment_for_append(
    layout: &LayoutPaths,
    index: &mut SegmentIndex,
    record_len: usize,
    prefix_len: usize,
) -> Result<u32, StorageError> {
    let max_segment_size = index.max_segment_size as u64;
    let should_rotate = {
        let active = index.active_descriptor()?;
        let prefix = if active.payload_bytes == 0 {
            prefix_len as u64
        } else {
            0
        };
        let projected = active
            .payload_bytes
            .saturating_add(prefix)
            .saturating_add(record_len as u64);
        active.payload_bytes > 0 && projected > max_segment_size
    };
    if should_rotate {
        rotate_active_segment(layout, index)?;
    }
    Ok(index.active_descriptor()?.segment_seq)
}

pub fn record_blob_append(
    index: &mut SegmentIndex,
    segment_seq: u32,
    payload_bytes: u64,
    blob_id: u64,
    stored_len: u32,
    codec: CompressionCodec,
) -> Result<(), StorageError> {
    let descriptor = index
        .descriptors
        .iter_mut()
        .find(|descriptor| descriptor.segment_seq == segment_seq)
        .ok_or_else(|| StorageError::NotFound("segment descriptor not found".to_string()))?;
    descriptor.payload_bytes = payload_bytes;
    if descriptor.first_blob_id == 0 || blob_id < descriptor.first_blob_id {
        descriptor.first_blob_id = blob_id;
    }
    if blob_id > descriptor.last_blob_id {
        descriptor.last_blob_id = blob_id;
    }
    descriptor.live_bytes = descriptor.live_bytes.saturating_add(stored_len);
    if codec != CompressionCodec::None {
        descriptor.compression = codec;
    }
    Ok(())
}

pub fn seal_segment(index: &mut SegmentIndex, segment_seq: u32) -> Result<(), StorageError> {
    let descriptor = index
        .descriptors
        .iter_mut()
        .find(|descriptor| descriptor.segment_seq == segment_seq)
        .ok_or_else(|| StorageError::NotFound("segment descriptor not found".to_string()))?;
    descriptor.state = SegmentState::Sealed;
    Ok(())
}

pub fn remove_segment_descriptor(index: &mut SegmentIndex, segment_seq: u32) {
    index
        .descriptors
        .retain(|descriptor| descriptor.segment_seq != segment_seq);
}

fn rotate_active_segment(
    layout: &LayoutPaths,
    index: &mut SegmentIndex,
) -> Result<(), StorageError> {
    {
        let active = index.active_descriptor_mut()?;
        active.state = SegmentState::Sealed;
    }
    let seq = u32::try_from(index.next_segment_seq)
        .map_err(|_| StorageError::Conflict("segment sequence exceeds u32".to_string()))?;
    write_segment_file(
        &data_segment_path(layout, seq),
        SegmentKind::Data,
        seq as u64,
        0,
        &[],
    )?;
    index.descriptors.push(SegmentDescriptor {
        segment_seq: seq,
        payload_bytes: 0,
        first_blob_id: 0,
        last_blob_id: 0,
        state: SegmentState::Active,
        compression: CompressionCodec::None,
        live_bytes: 0,
    });
    index.next_segment_seq = index.next_segment_seq.saturating_add(1);
    persist_segment_index(&layout.segment_index_file, index)
}

fn decode_segment_index(payload: &[u8]) -> Result<SegmentIndex, StorageError> {
    if payload.len() < 24 || &payload[..4] != SEGMENT_INDEX_PREFIX {
        return Err(StorageError::Corrupt(
            "segment index prefix mismatch".to_string(),
        ));
    }
    let max_segment_size = u32::from_le_bytes(
        payload[4..8]
            .try_into()
            .map_err(|_| StorageError::Corrupt("segment index max parse failed".to_string()))?,
    );
    let active_segment_count = u32::from_le_bytes(
        payload[8..12]
            .try_into()
            .map_err(|_| StorageError::Corrupt("segment index active parse failed".to_string()))?,
    );
    let total_segment_count = u32::from_le_bytes(
        payload[12..16]
            .try_into()
            .map_err(|_| StorageError::Corrupt("segment index total parse failed".to_string()))?,
    ) as usize;
    let next_segment_seq = u64::from_le_bytes(
        payload[16..24]
            .try_into()
            .map_err(|_| StorageError::Corrupt("segment index next parse failed".to_string()))?,
    );
    let expected_len = 24 + total_segment_count * SEGMENT_DESCRIPTOR_SIZE;
    if payload.len() != expected_len {
        return Err(StorageError::Corrupt(
            "segment index descriptor length mismatch".to_string(),
        ));
    }
    let mut descriptors = Vec::with_capacity(total_segment_count);
    for idx in 0..total_segment_count {
        let start = 24 + idx * SEGMENT_DESCRIPTOR_SIZE;
        descriptors.push(SegmentDescriptor::decode(
            &payload[start..start + SEGMENT_DESCRIPTOR_SIZE],
        )?);
    }
    let found_active = descriptors
        .iter()
        .filter(|descriptor| descriptor.state == SegmentState::Active)
        .count() as u32;
    if found_active != active_segment_count {
        return Err(StorageError::Corrupt(
            "segment index active count mismatch".to_string(),
        ));
    }
    Ok(SegmentIndex {
        max_segment_size,
        descriptors,
        next_segment_seq,
    })
}

pub fn sync_indexed_segments(layout: &LayoutPaths) -> Result<(), StorageError> {
    let index = load_segment_index_for_append(&layout.segment_index_file)?;
    for descriptor in index.descriptors {
        let path = data_segment_path(layout, descriptor.segment_seq);
        if path.exists() {
            let _ = validate_segment_shape(&path, SegmentKind::Data)?;
            let file = fs::OpenOptions::new().read(true).write(true).open(path)?;
            file.sync_all()?;
        }
    }
    Ok(())
}

fn descriptor_checksum(bytes: &[u8]) -> u16 {
    (checksum32(bytes) & 0xFFFF) as u16
}
