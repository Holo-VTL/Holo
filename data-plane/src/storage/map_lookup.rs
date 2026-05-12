use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, OnceLock};

use super::blk_map::{load_blk_map_records, BlkMapState};
use super::layout::SegmentKind;
use super::metadata::{
    checked_usize_from_u64, lock_storage_mutex, modified_nanos_from_result, StorageError,
};
use super::segment::{
    append_segment_payload, read_segment_file, sync_segment_file, write_segment_file,
};

const RECORD_SIZE: usize = 40;
const LOG_PREFIX: &[u8; 4] = b"LPV2";

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MapLookupRecord {
    pub lookup_id: u64,
    pub logical_min: u64,
    pub logical_max: u64,
    pub blk_map_ref_start: u64,
    pub blk_map_ref_end: u64,
}

impl MapLookupRecord {
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::with_capacity(RECORD_SIZE);
        out.extend_from_slice(&self.lookup_id.to_le_bytes());
        out.extend_from_slice(&self.logical_min.to_le_bytes());
        out.extend_from_slice(&self.logical_max.to_le_bytes());
        out.extend_from_slice(&self.blk_map_ref_start.to_le_bytes());
        out.extend_from_slice(&self.blk_map_ref_end.to_le_bytes());
        out
    }

    pub fn decode(buf: &[u8]) -> Result<Self, StorageError> {
        if buf.len() < RECORD_SIZE {
            return Err(StorageError::Corrupt(
                "map lookup record too short".to_string(),
            ));
        }
        Ok(Self {
            lookup_id: u64::from_le_bytes(
                buf[0..8]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("lookup parse failed".to_string()))?,
            ),
            logical_min: u64::from_le_bytes(
                buf[8..16]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("lookup parse failed".to_string()))?,
            ),
            logical_max: u64::from_le_bytes(
                buf[16..24]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("lookup parse failed".to_string()))?,
            ),
            blk_map_ref_start: u64::from_le_bytes(
                buf[24..32]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("lookup parse failed".to_string()))?,
            ),
            blk_map_ref_end: u64::from_le_bytes(
                buf[32..40]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("lookup parse failed".to_string()))?,
            ),
        })
    }
}

#[derive(Debug, Clone)]
struct CachedLookup {
    stamp: FileStamp,
    sequence: u64,
    records: Vec<MapLookupRecord>,
    next_lookup_id: u64,
    is_log_format: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct FileStamp {
    len: u64,
    modified_nanos: u128,
}

fn cache() -> &'static Mutex<HashMap<PathBuf, CachedLookup>> {
    static CACHE: OnceLock<Mutex<HashMap<PathBuf, CachedLookup>>> = OnceLock::new();
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

pub fn load_lookup_records(path: &Path) -> Result<(u64, Vec<MapLookupRecord>), StorageError> {
    ensure_cache_fresh(path)?;
    let guard = lock_storage_mutex(cache(), "lookup")?;
    let entry = guard
        .get(path)
        .ok_or_else(|| StorageError::NotFound("lookup cache not initialized".to_string()))?;
    Ok((entry.sequence, entry.records.clone()))
}

fn ensure_cache_fresh(path: &Path) -> Result<(), StorageError> {
    if trust_hot_cache() {
        let guard = lock_storage_mutex(cache(), "lookup")?;
        if guard.contains_key(path) {
            return Ok(());
        }
    }

    let stamp = file_stamp(path)?;
    {
        let guard = lock_storage_mutex(cache(), "lookup")?;
        if let Some(entry) = guard.get(path) {
            if entry.stamp == stamp {
                return Ok(());
            }
        }
    }

    let (header, payload) = read_segment_file(path, SegmentKind::Lookup)?;
    let records = decode_payload(&payload)?;
    lock_storage_mutex(cache(), "lookup")?.insert(
        path.to_path_buf(),
        CachedLookup {
            stamp,
            sequence: header.sequence,
            next_lookup_id: next_lookup_id(&records),
            records,
            is_log_format: payload.starts_with(LOG_PREFIX),
        },
    );
    Ok(())
}

pub fn append_lookup_record(
    path: &Path,
    mut record: MapLookupRecord,
) -> Result<MapLookupRecord, StorageError> {
    ensure_cache_fresh(path)?;

    {
        let guard = lock_storage_mutex(cache(), "lookup")?;
        let entry = guard
            .get(path)
            .ok_or_else(|| StorageError::NotFound("lookup cache not initialized".to_string()))?;
        if record.lookup_id == 0 {
            record.lookup_id = entry.next_lookup_id;
        }
    }

    let mut rewrote_legacy = false;
    {
        let guard = lock_storage_mutex(cache(), "lookup")?;
        let entry = guard
            .get(path)
            .ok_or_else(|| StorageError::NotFound("lookup cache not initialized".to_string()))?;
        if !entry.is_log_format {
            rewrote_legacy = true;
            let payload = encode_log_payload(&entry.records);
            let sequence = entry.records.len() as u64;
            write_segment_file(path, SegmentKind::Lookup, 3, sequence, &payload)?;
        }
    }
    if rewrote_legacy {
        ensure_cache_fresh(path)?;
    }

    let header = append_segment_payload(
        path,
        SegmentKind::Lookup,
        3,
        LOG_PREFIX,
        &record.encode(),
        false,
    )?;
    let mut guard = lock_storage_mutex(cache(), "lookup")?;
    let entry = guard
        .get_mut(path)
        .ok_or_else(|| StorageError::NotFound("lookup cache not initialized".to_string()))?;
    entry.sequence = header.sequence;
    entry.next_lookup_id = entry.next_lookup_id.max(record.lookup_id.saturating_add(1));
    if entry
        .records
        .last()
        .map(|item| item.logical_min <= record.logical_min)
        .unwrap_or(true)
    {
        entry.records.push(record.clone());
    } else {
        let insert_idx = entry
            .records
            .partition_point(|item| item.logical_min <= record.logical_min);
        entry.records.insert(insert_idx, record.clone());
    }
    if !trust_hot_cache() {
        entry.stamp = file_stamp(path)?;
    }
    entry.is_log_format = true;
    Ok(record)
}

pub fn locate_logical_block(
    path: &Path,
    logical_block: u64,
) -> Result<Option<MapLookupRecord>, StorageError> {
    ensure_cache_fresh(path)?;
    let guard = lock_storage_mutex(cache(), "lookup")?;
    let entry = guard
        .get(path)
        .ok_or_else(|| StorageError::NotFound("lookup cache not initialized".to_string()))?;
    let idx = entry
        .records
        .partition_point(|record| record.logical_min <= logical_block);
    if idx == 0 {
        return Ok(None);
    }
    let rec = &entry.records[idx - 1];
    if logical_block <= rec.logical_max {
        return Ok(Some(rec.clone()));
    }
    Ok(None)
}

pub fn rebuild_lookup_from_blk_map(
    blk_map_path: &Path,
    lookup_path: &Path,
) -> Result<usize, StorageError> {
    let (_seq, records) = load_blk_map_records(blk_map_path)?;
    let mut rebuilt = Vec::new();

    for rec in records
        .into_iter()
        .filter(|r| r.state == BlkMapState::Active)
    {
        if rec.logical_len == 0 {
            return Err(StorageError::Corrupt(
                "active blk map record has zero logical length".to_string(),
            ));
        }
        let logical_max = rec
            .logical_start
            .checked_add(u64::from(rec.logical_len) - 1)
            .ok_or_else(|| StorageError::Corrupt("logical range overflow".to_string()))?;
        rebuilt.push(MapLookupRecord {
            lookup_id: rebuilt.len() as u64 + 1,
            logical_min: rec.logical_start,
            logical_max,
            blk_map_ref_start: rec.record_id,
            blk_map_ref_end: rec.record_id,
        });
    }

    persist_lookup_records(lookup_path, &rebuilt)?;
    Ok(rebuilt.len())
}

pub fn persist_lookup_records(
    path: &Path,
    records: &[MapLookupRecord],
) -> Result<(), StorageError> {
    let payload = encode_log_payload(records);
    let sequence = records.len() as u64;
    write_segment_file(path, SegmentKind::Lookup, 3, sequence, &payload)?;
    update_cache(path, sequence, records.to_vec())
}

fn decode_payload(payload: &[u8]) -> Result<Vec<MapLookupRecord>, StorageError> {
    if payload.is_empty() {
        return Ok(Vec::new());
    }
    if payload.starts_with(LOG_PREFIX) {
        return decode_log_payload(payload);
    }
    decode_legacy_payload(payload)
}

fn decode_legacy_payload(payload: &[u8]) -> Result<Vec<MapLookupRecord>, StorageError> {
    if payload.len() < 8 {
        return Err(StorageError::Corrupt(
            "lookup payload too short".to_string(),
        ));
    }

    let raw_count = u64::from_le_bytes(
        payload[0..8]
            .try_into()
            .map_err(|_| StorageError::Corrupt("lookup count parse failed".to_string()))?,
    );
    let count = checked_usize_from_u64(raw_count, "lookup count")?;
    if payload.len().saturating_sub(8) / RECORD_SIZE < count {
        return Err(StorageError::Corrupt(
            "lookup payload truncated".to_string(),
        ));
    }
    let mut offset = 8;
    let mut records = Vec::with_capacity(count);
    for _ in 0..count {
        if payload.len() < offset + RECORD_SIZE {
            return Err(StorageError::Corrupt(
                "lookup payload truncated".to_string(),
            ));
        }
        records.push(MapLookupRecord::decode(
            &payload[offset..offset + RECORD_SIZE],
        )?);
        offset += RECORD_SIZE;
    }
    Ok(records)
}

fn decode_log_payload(payload: &[u8]) -> Result<Vec<MapLookupRecord>, StorageError> {
    if payload.len() < LOG_PREFIX.len() {
        return Err(StorageError::Corrupt(
            "lookup log payload too short".to_string(),
        ));
    }
    let mut offset = LOG_PREFIX.len();
    let mut latest = HashMap::<u64, MapLookupRecord>::new();
    while offset < payload.len() {
        if payload.len() < offset + RECORD_SIZE {
            return Err(StorageError::Corrupt(
                "lookup log payload truncated".to_string(),
            ));
        }
        let record = MapLookupRecord::decode(&payload[offset..offset + RECORD_SIZE])?;
        latest.insert(record.lookup_id, record);
        offset += RECORD_SIZE;
    }
    let mut records = latest.into_values().collect::<Vec<_>>();
    records.sort_by_key(|entry| entry.logical_min);
    Ok(records)
}

fn encode_log_payload(records: &[MapLookupRecord]) -> Vec<u8> {
    let mut payload = Vec::with_capacity(LOG_PREFIX.len() + records.len() * RECORD_SIZE);
    payload.extend_from_slice(LOG_PREFIX);
    for entry in records {
        payload.extend_from_slice(&entry.encode());
    }
    payload
}

fn update_cache(
    path: &Path,
    sequence: u64,
    records: Vec<MapLookupRecord>,
) -> Result<(), StorageError> {
    let stamp = file_stamp(path)?;
    lock_storage_mutex(cache(), "lookup")?.insert(
        path.to_path_buf(),
        CachedLookup {
            stamp,
            sequence,
            next_lookup_id: next_lookup_id(&records),
            records,
            is_log_format: true,
        },
    );
    Ok(())
}

fn next_lookup_id(records: &[MapLookupRecord]) -> u64 {
    records
        .iter()
        .map(|record| record.lookup_id)
        .max()
        .unwrap_or(0)
        .saturating_add(1)
}

pub fn sync_lookup(path: &Path) -> Result<(), StorageError> {
    sync_segment_file(path)
}
