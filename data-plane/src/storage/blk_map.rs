use std::collections::{HashMap, HashSet};
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, OnceLock};

use super::compression::CompressionCodec;
use super::layout::SegmentKind;
use super::metadata::{lock_storage_mutex, modified_nanos_from_result, StorageError};
use super::segment::{
    append_segment_payload, read_segment_file, sync_segment_file, write_segment_file,
};

pub const MAX_RECORDS_PER_SEGMENT: usize = 1024;
const LEGACY_RECORD_SIZE: usize = 41;
const EXTENDED_RECORD_SIZE: usize = 58;
const LOG_PREFIX: &[u8; 4] = b"BMV2";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum BlkMapState {
    Active = 1,
    Stale = 2,
}

impl BlkMapState {
    fn from_u8(v: u8) -> Result<Self, StorageError> {
        match v {
            1 => Ok(Self::Active),
            2 => Ok(Self::Stale),
            _ => Err(StorageError::Corrupt("invalid blk map state".to_string())),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BlkMapRecord {
    pub record_id: u64,
    pub logical_start: u64,
    pub logical_len: u32,
    pub physical_segment_id: u64,
    pub physical_offset: u64,
    pub filemark_count: u32,
    pub state: BlkMapState,
    pub dedup_entry_id: u64,
    pub compression: CompressionCodec,
    pub compressed_len: u32,
    pub payload_checksum: u32,
}

impl BlkMapRecord {
    pub fn logical_end(&self) -> u64 {
        self.logical_start.saturating_add(self.logical_len as u64)
    }

    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::with_capacity(EXTENDED_RECORD_SIZE);
        out.extend_from_slice(&self.record_id.to_le_bytes());
        out.extend_from_slice(&self.logical_start.to_le_bytes());
        out.extend_from_slice(&self.logical_len.to_le_bytes());
        out.extend_from_slice(&self.physical_segment_id.to_le_bytes());
        out.extend_from_slice(&self.physical_offset.to_le_bytes());
        out.extend_from_slice(&self.filemark_count.to_le_bytes());
        out.push(self.state as u8);
        out.extend_from_slice(&self.dedup_entry_id.to_le_bytes());
        out.push(self.compression as u8);
        out.extend_from_slice(&self.compressed_len.to_le_bytes());
        out.extend_from_slice(&self.payload_checksum.to_le_bytes());
        out
    }

    pub fn decode(buf: &[u8]) -> Result<Self, StorageError> {
        match buf.len() {
            LEGACY_RECORD_SIZE => Self::decode_legacy(buf),
            EXTENDED_RECORD_SIZE => Self::decode_extended(buf),
            _ => Err(StorageError::Corrupt(
                "unsupported blk map record size".to_string(),
            )),
        }
    }

    fn decode_legacy(buf: &[u8]) -> Result<Self, StorageError> {
        Ok(Self {
            record_id: u64::from_le_bytes(
                buf[0..8]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            logical_start: u64::from_le_bytes(
                buf[8..16]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            logical_len: u32::from_le_bytes(
                buf[16..20]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            physical_segment_id: u64::from_le_bytes(
                buf[20..28]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            physical_offset: u64::from_le_bytes(
                buf[28..36]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            filemark_count: u32::from_le_bytes(
                buf[36..40]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            state: BlkMapState::from_u8(buf[40])?,
            dedup_entry_id: 0,
            compression: CompressionCodec::None,
            compressed_len: u32::from_le_bytes(
                buf[16..20]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            payload_checksum: 0,
        })
    }

    fn decode_extended(buf: &[u8]) -> Result<Self, StorageError> {
        Ok(Self {
            record_id: u64::from_le_bytes(
                buf[0..8]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            logical_start: u64::from_le_bytes(
                buf[8..16]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            logical_len: u32::from_le_bytes(
                buf[16..20]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            physical_segment_id: u64::from_le_bytes(
                buf[20..28]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            physical_offset: u64::from_le_bytes(
                buf[28..36]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            filemark_count: u32::from_le_bytes(
                buf[36..40]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            state: BlkMapState::from_u8(buf[40])?,
            dedup_entry_id: u64::from_le_bytes(
                buf[41..49]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            compression: CompressionCodec::from_u8(buf[49])?,
            compressed_len: u32::from_le_bytes(
                buf[50..54]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
            payload_checksum: u32::from_le_bytes(
                buf[54..58]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("blk map parse failed".to_string()))?,
            ),
        })
    }
}

#[derive(Debug, Clone)]
struct CachedBlkMap {
    stamp: FileStamp,
    sequence: u64,
    records: Vec<BlkMapRecord>,
    record_index: HashMap<u64, usize>,
    next_record_id: u64,
    is_log_format: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct FileStamp {
    len: u64,
    modified_nanos: u128,
}

fn cache() -> &'static Mutex<HashMap<PathBuf, CachedBlkMap>> {
    static CACHE: OnceLock<Mutex<HashMap<PathBuf, CachedBlkMap>>> = OnceLock::new();
    CACHE.get_or_init(|| Mutex::new(HashMap::new()))
}

fn trust_hot_cache() -> bool {
    static TRUST: OnceLock<bool> = OnceLock::new();
    *TRUST.get_or_init(|| {
        std::env::var("HOLO_STORAGE_TRUST_HOT_CACHE")
            .ok()
            .map(|raw| {
                !matches!(
                    raw.trim().to_ascii_lowercase().as_str(),
                    "0" | "false" | "no"
                )
            })
            .unwrap_or(false)
    })
}

fn file_stamp(path: &Path) -> Result<FileStamp, StorageError> {
    let metadata = fs::metadata(path)?;
    let modified_nanos = modified_nanos_from_result(metadata.modified())?;
    Ok(FileStamp {
        len: metadata.len(),
        modified_nanos,
    })
}

pub fn load_blk_map_records(path: &Path) -> Result<(u64, Vec<BlkMapRecord>), StorageError> {
    ensure_cache_fresh(path)?;
    let guard = lock_storage_mutex(cache(), "blk_map")?;
    let entry = guard
        .get(path)
        .ok_or_else(|| StorageError::NotFound("blk map cache not initialized".to_string()))?;
    Ok((entry.sequence, entry.records.clone()))
}

pub fn locate_active_record(
    path: &Path,
    logical_block: u64,
    record_id_start: u64,
    record_id_end: u64,
) -> Result<Option<BlkMapRecord>, StorageError> {
    ensure_cache_fresh(path)?;
    let guard = lock_storage_mutex(cache(), "blk_map")?;
    let entry = guard
        .get(path)
        .ok_or_else(|| StorageError::NotFound("blk map cache not initialized".to_string()))?;
    if record_id_start == record_id_end {
        if let Some(idx) = entry.record_index.get(&record_id_start) {
            if let Some(record) = entry.records.get(*idx) {
                if record.state == BlkMapState::Active
                    && logical_block >= record.logical_start
                    && logical_block < record.logical_end()
                {
                    return Ok(Some(record.clone()));
                }
            }
        }
    }
    Ok(entry
        .records
        .iter()
        .find(|record| {
            record.state == BlkMapState::Active
                && record.record_id >= record_id_start
                && record.record_id <= record_id_end
                && logical_block >= record.logical_start
                && logical_block < record.logical_end()
        })
        .cloned())
}

fn ensure_cache_fresh(path: &Path) -> Result<(), StorageError> {
    if trust_hot_cache() {
        let guard = lock_storage_mutex(cache(), "blk_map")?;
        if guard.contains_key(path) {
            return Ok(());
        }
    }

    let stamp = file_stamp(path)?;
    {
        let guard = lock_storage_mutex(cache(), "blk_map")?;
        if let Some(entry) = guard.get(path) {
            if entry.stamp == stamp {
                return Ok(());
            }
        }
    }

    let (header, payload) = read_segment_file(path, SegmentKind::BlkMap)?;
    let records = decode_payload(&payload)?;
    lock_storage_mutex(cache(), "blk_map")?.insert(
        path.to_path_buf(),
        CachedBlkMap {
            stamp,
            sequence: header.sequence,
            record_index: build_record_index(&records),
            next_record_id: next_record_id(&records),
            records,
            is_log_format: payload.starts_with(LOG_PREFIX),
        },
    );
    Ok(())
}

pub fn append_blk_map_record(
    path: &Path,
    mut record: BlkMapRecord,
) -> Result<BlkMapRecord, StorageError> {
    ensure_cache_fresh(path)?;

    {
        let guard = lock_storage_mutex(cache(), "blk_map")?;
        let entry = guard
            .get(path)
            .ok_or_else(|| StorageError::NotFound("blk map cache not initialized".to_string()))?;
        let insert_idx = entry
            .records
            .partition_point(|existing| existing.logical_start < record.logical_start);
        let previous_active = entry.records[..insert_idx]
            .iter()
            .rev()
            .find(|existing| existing.state == BlkMapState::Active);
        let next_active = entry.records[insert_idx..]
            .iter()
            .find(|existing| existing.state == BlkMapState::Active);
        let overlaps_previous = previous_active
            .map(|existing| record.logical_start < existing.logical_end())
            .unwrap_or(false);
        let overlaps_next = next_active
            .map(|existing| existing.logical_start < record.logical_end())
            .unwrap_or(false);
        if overlaps_previous || overlaps_next {
            return Err(StorageError::Conflict(
                "active logical range overlap".to_string(),
            ));
        }
        if record.record_id == 0 {
            record.record_id = entry.next_record_id;
        }
    }

    let mut rewrote_legacy = false;
    {
        let guard = lock_storage_mutex(cache(), "blk_map")?;
        let entry = guard
            .get(path)
            .ok_or_else(|| StorageError::NotFound("blk map cache not initialized".to_string()))?;
        if !entry.is_log_format {
            rewrote_legacy = true;
            let payload = encode_log_payload(&entry.records);
            let sequence = ((entry.records.len() / MAX_RECORDS_PER_SEGMENT) as u64).max(1);
            write_segment_file(path, SegmentKind::BlkMap, 2, sequence, &payload)?;
        }
    }
    if rewrote_legacy {
        ensure_cache_fresh(path)?;
    }

    let header = append_segment_payload(
        path,
        SegmentKind::BlkMap,
        2,
        LOG_PREFIX,
        &record.encode(),
        false,
    )?;
    let mut guard = lock_storage_mutex(cache(), "blk_map")?;
    let entry = guard
        .get_mut(path)
        .ok_or_else(|| StorageError::NotFound("blk map cache not initialized".to_string()))?;
    entry.sequence = header.sequence;
    entry.next_record_id = entry.next_record_id.max(record.record_id.saturating_add(1));
    if entry
        .records
        .last()
        .map(|item| item.logical_start <= record.logical_start)
        .unwrap_or(true)
    {
        entry
            .record_index
            .insert(record.record_id, entry.records.len());
        entry.records.push(record.clone());
    } else {
        let insert_idx = entry
            .records
            .partition_point(|item| item.logical_start <= record.logical_start);
        entry.records.insert(insert_idx, record.clone());
        entry.record_index = build_record_index(&entry.records);
    }
    if !trust_hot_cache() {
        entry.stamp = file_stamp(path)?;
    }
    entry.is_log_format = true;
    Ok(record)
}

pub fn mark_blk_map_stale(path: &Path, record_id: u64) -> Result<BlkMapRecord, StorageError> {
    let mut updated = mark_blk_map_stale_batch(path, &[record_id])?;
    updated
        .pop()
        .ok_or_else(|| StorageError::NotFound("blk map record not found".to_string()))
}

pub fn mark_blk_map_stale_batch(
    path: &Path,
    record_ids: &[u64],
) -> Result<Vec<BlkMapRecord>, StorageError> {
    if record_ids.is_empty() {
        return Ok(Vec::new());
    }

    let (_sequence, mut records) = load_blk_map_records(path)?;
    // Convert the input slice to a HashSet so the per-record lookup is O(1)
    // instead of O(M). The previous Vec::contains scan made the whole loop
    // O(N*M), which becomes painful during reclaim batches.
    let target_ids: HashSet<u64> = record_ids.iter().copied().collect();
    let mut updated = Vec::new();
    for rec in &records {
        if target_ids.contains(&rec.record_id) {
            let mut next = rec.clone();
            next.state = BlkMapState::Stale;
            updated.push(next);
        }
    }
    if updated.len() != target_ids.len() {
        return Err(StorageError::NotFound(
            "one or more blk map records not found".to_string(),
        ));
    }

    ensure_log_format(path, &records)?;
    let mut appended = Vec::with_capacity(updated.len() * EXTENDED_RECORD_SIZE);
    for rec in &updated {
        appended.extend_from_slice(&rec.encode());
    }
    let header =
        append_segment_payload(path, SegmentKind::BlkMap, 2, LOG_PREFIX, &appended, false)?;

    for next in &updated {
        if let Some(existing) = records
            .iter_mut()
            .find(|rec| rec.record_id == next.record_id)
        {
            *existing = next.clone();
        }
    }
    records.sort_by_key(|entry| entry.logical_start);
    update_cache(path, header.sequence, records)?;
    Ok(updated)
}

pub fn persist_blk_map_records(path: &Path, records: &[BlkMapRecord]) -> Result<(), StorageError> {
    let payload = encode_log_payload(records);
    let sequence = ((records.len() / MAX_RECORDS_PER_SEGMENT) as u64).max(1);
    write_segment_file(path, SegmentKind::BlkMap, 2, sequence, &payload)?;
    update_cache(path, sequence, records.to_vec())
}

fn decode_payload(payload: &[u8]) -> Result<Vec<BlkMapRecord>, StorageError> {
    if payload.is_empty() {
        return Ok(Vec::new());
    }
    if payload.starts_with(LOG_PREFIX) {
        return decode_log_payload(payload);
    }
    decode_legacy_payload(payload)
}

fn decode_legacy_payload(payload: &[u8]) -> Result<Vec<BlkMapRecord>, StorageError> {
    if payload.len() < 8 {
        return Err(StorageError::Corrupt(
            "blk map payload too short".to_string(),
        ));
    }

    let count = u64::from_le_bytes(
        payload[0..8]
            .try_into()
            .map_err(|_| StorageError::Corrupt("blk map count parse failed".to_string()))?,
    ) as usize;
    let mut offset = 8;
    let mut records = Vec::with_capacity(count);
    if count == 0 {
        return Ok(records);
    }
    let remaining = payload.len() - offset;
    if remaining % count != 0 {
        return Err(StorageError::Corrupt(
            "blk map record size mismatch".to_string(),
        ));
    }
    let record_size = remaining / count;
    if record_size != LEGACY_RECORD_SIZE && record_size != EXTENDED_RECORD_SIZE {
        return Err(StorageError::Corrupt(
            "unsupported blk map record size".to_string(),
        ));
    }
    for _ in 0..count {
        if payload.len() < offset + record_size {
            return Err(StorageError::Corrupt(
                "blk map payload truncated".to_string(),
            ));
        }
        let record = BlkMapRecord::decode(&payload[offset..offset + record_size])?;
        records.push(record);
        offset += record_size;
    }
    Ok(records)
}

fn decode_log_payload(payload: &[u8]) -> Result<Vec<BlkMapRecord>, StorageError> {
    if payload.len() < LOG_PREFIX.len() {
        return Err(StorageError::Corrupt(
            "blk map log payload too short".to_string(),
        ));
    }
    let mut offset = LOG_PREFIX.len();
    let mut latest = HashMap::<u64, BlkMapRecord>::new();
    while offset < payload.len() {
        if payload.len() < offset + EXTENDED_RECORD_SIZE {
            return Err(StorageError::Corrupt(
                "blk map log payload truncated".to_string(),
            ));
        }
        let record = BlkMapRecord::decode(&payload[offset..offset + EXTENDED_RECORD_SIZE])?;
        latest.insert(record.record_id, record);
        offset += EXTENDED_RECORD_SIZE;
    }
    let mut records = latest.into_values().collect::<Vec<_>>();
    records.sort_by_key(|entry| entry.logical_start);
    Ok(records)
}

fn ensure_log_format(path: &Path, records: &[BlkMapRecord]) -> Result<(), StorageError> {
    let (_, payload) = read_segment_file(path, SegmentKind::BlkMap)?;
    if payload.starts_with(LOG_PREFIX) {
        return Ok(());
    }
    let payload = encode_log_payload(records);
    let sequence = ((records.len() / MAX_RECORDS_PER_SEGMENT) as u64).max(1);
    write_segment_file(path, SegmentKind::BlkMap, 2, sequence, &payload)?;
    update_cache(path, sequence, records.to_vec())
}

fn encode_log_payload(records: &[BlkMapRecord]) -> Vec<u8> {
    let mut payload = Vec::with_capacity(LOG_PREFIX.len() + records.len() * EXTENDED_RECORD_SIZE);
    payload.extend_from_slice(LOG_PREFIX);
    for entry in records {
        payload.extend_from_slice(&entry.encode());
    }
    payload
}

fn update_cache(
    path: &Path,
    sequence: u64,
    records: Vec<BlkMapRecord>,
) -> Result<(), StorageError> {
    let stamp = file_stamp(path)?;
    lock_storage_mutex(cache(), "blk_map")?.insert(
        path.to_path_buf(),
        CachedBlkMap {
            stamp,
            sequence,
            record_index: build_record_index(&records),
            next_record_id: next_record_id(&records),
            records,
            is_log_format: true,
        },
    );
    Ok(())
}

fn build_record_index(records: &[BlkMapRecord]) -> HashMap<u64, usize> {
    records
        .iter()
        .enumerate()
        .map(|(idx, record)| (record.record_id, idx))
        .collect()
}

fn next_record_id(records: &[BlkMapRecord]) -> u64 {
    records
        .iter()
        .map(|record| record.record_id)
        .max()
        .unwrap_or(0)
        .saturating_add(1)
}

pub fn sync_blk_map(path: &Path) -> Result<(), StorageError> {
    sync_segment_file(path)
}
