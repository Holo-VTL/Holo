use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, OnceLock};

use super::compression::CompressionCodec;
use super::layout::SegmentKind;
use super::metadata::{
    checked_usize_from_u64, lock_storage_mutex, modified_nanos_from_result, StorageError,
};
use super::segment::{
    append_segment_payload, read_segment_file, sync_segment_file, write_segment_file,
};

const DEDUP_RECORD_SIZE: usize = 52;
const LOG_PREFIX: &[u8; 4] = b"DDV2";
pub const DEDUP_IDENTITY_BLAKE3_128: u8 = 1;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DedupIndexEntry {
    pub entry_id: u64,
    pub fingerprint_hi: u64,
    pub fingerprint_lo: u64,
    pub payload_checksum: u32,
    pub logical_len: u32,
    pub stored_blob_id: u64,
    pub stored_len: u32,
    pub compression: CompressionCodec,
    pub identity_version: u8,
    pub ref_count: u32,
}

impl DedupIndexEntry {
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::with_capacity(DEDUP_RECORD_SIZE);
        out.extend_from_slice(&self.entry_id.to_le_bytes());
        out.extend_from_slice(&self.fingerprint_hi.to_le_bytes());
        out.extend_from_slice(&self.fingerprint_lo.to_le_bytes());
        out.extend_from_slice(&self.payload_checksum.to_le_bytes());
        out.extend_from_slice(&self.logical_len.to_le_bytes());
        out.extend_from_slice(&self.stored_blob_id.to_le_bytes());
        out.extend_from_slice(&self.stored_len.to_le_bytes());
        out.push(self.compression as u8);
        out.push(self.identity_version);
        out.extend_from_slice(&[0u8; 2]);
        out.extend_from_slice(&self.ref_count.to_le_bytes());
        out
    }

    pub fn decode(buf: &[u8]) -> Result<Self, StorageError> {
        if buf.len() < DEDUP_RECORD_SIZE {
            return Err(StorageError::Corrupt("dedup record too short".to_string()));
        }
        Ok(Self {
            entry_id: u64::from_le_bytes(
                buf[0..8]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("dedup parse failed".to_string()))?,
            ),
            fingerprint_hi: u64::from_le_bytes(
                buf[8..16]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("dedup parse failed".to_string()))?,
            ),
            fingerprint_lo: u64::from_le_bytes(
                buf[16..24]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("dedup parse failed".to_string()))?,
            ),
            payload_checksum: u32::from_le_bytes(
                buf[24..28]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("dedup parse failed".to_string()))?,
            ),
            logical_len: u32::from_le_bytes(
                buf[28..32]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("dedup parse failed".to_string()))?,
            ),
            stored_blob_id: u64::from_le_bytes(
                buf[32..40]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("dedup parse failed".to_string()))?,
            ),
            stored_len: u32::from_le_bytes(
                buf[40..44]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("dedup parse failed".to_string()))?,
            ),
            compression: CompressionCodec::from_u8(buf[44])?,
            identity_version: buf[45],
            ref_count: u32::from_le_bytes(
                buf[48..52]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("dedup parse failed".to_string()))?,
            ),
        })
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DedupLookup {
    Hit(DedupIndexEntry),
    Collision,
    Miss,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DedupUpsertResult {
    Hit(DedupIndexEntry),
    Inserted(DedupIndexEntry),
    CollisionInserted(DedupIndexEntry),
}

#[derive(Debug, Clone)]
struct CachedDedup {
    stamp: FileStamp,
    sequence: u64,
    entries: Vec<DedupIndexEntry>,
    identity_index: HashMap<DedupIdentityKey, usize>,
    fingerprint_index: HashMap<DedupFingerprintKey, ()>,
    id_index: HashMap<u64, usize>,
    next_entry_id: u64,
    log_format: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct FileStamp {
    len: u64,
    modified_nanos: u128,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
struct DedupIdentityKey {
    fingerprint_hi: u64,
    fingerprint_lo: u64,
    payload_checksum: u32,
    logical_len: u32,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
struct DedupFingerprintKey {
    fingerprint_hi: u64,
    fingerprint_lo: u64,
}

fn cache() -> &'static Mutex<HashMap<PathBuf, CachedDedup>> {
    static CACHE: OnceLock<Mutex<HashMap<PathBuf, CachedDedup>>> = OnceLock::new();
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

fn identity_key(
    fingerprint_hi: u64,
    fingerprint_lo: u64,
    payload_checksum: u32,
    logical_len: u32,
) -> DedupIdentityKey {
    DedupIdentityKey {
        fingerprint_hi,
        fingerprint_lo,
        payload_checksum,
        logical_len,
    }
}

fn identity_key_for(entry: &DedupIndexEntry) -> DedupIdentityKey {
    identity_key(
        entry.fingerprint_hi,
        entry.fingerprint_lo,
        entry.payload_checksum,
        entry.logical_len,
    )
}

fn fingerprint_key(fingerprint_hi: u64, fingerprint_lo: u64) -> DedupFingerprintKey {
    DedupFingerprintKey {
        fingerprint_hi,
        fingerprint_lo,
    }
}

fn fingerprint_key_for(entry: &DedupIndexEntry) -> DedupFingerprintKey {
    fingerprint_key(entry.fingerprint_hi, entry.fingerprint_lo)
}

fn build_cached_dedup(
    stamp: FileStamp,
    sequence: u64,
    entries: Vec<DedupIndexEntry>,
    log_format: bool,
) -> CachedDedup {
    let mut identity_index = HashMap::with_capacity(entries.len());
    let mut fingerprint_index = HashMap::with_capacity(entries.len());
    let mut id_index = HashMap::with_capacity(entries.len());
    let mut max_entry_id = 0u64;
    for (index, entry) in entries.iter().enumerate() {
        identity_index.insert(identity_key_for(entry), index);
        fingerprint_index.insert(fingerprint_key_for(entry), ());
        id_index.insert(entry.entry_id, index);
        max_entry_id = max_entry_id.max(entry.entry_id);
    }

    CachedDedup {
        stamp,
        sequence,
        entries,
        identity_index,
        fingerprint_index,
        id_index,
        next_entry_id: max_entry_id.saturating_add(1),
        log_format,
    }
}

fn ensure_cache_fresh(path: &Path) -> Result<(), StorageError> {
    if trust_hot_cache() {
        let guard = lock_storage_mutex(cache(), "dedup")?;
        if guard.contains_key(path) {
            return Ok(());
        }
    }

    let stamp = file_stamp(path)?;
    {
        let guard = lock_storage_mutex(cache(), "dedup")?;
        if let Some(entry) = guard.get(path) {
            if entry.stamp == stamp {
                return Ok(());
            }
        }
    }

    let (header, payload) = read_segment_file(path, SegmentKind::Dedup)?;
    let log_format = payload.starts_with(LOG_PREFIX);
    let entries = decode_payload(&payload)?;
    lock_storage_mutex(cache(), "dedup")?.insert(
        path.to_path_buf(),
        build_cached_dedup(stamp, header.sequence, entries, log_format),
    );
    Ok(())
}

pub fn fingerprint128(payload: &[u8]) -> (u64, u64) {
    let hash = blake3::hash(payload);
    let bytes = hash.as_bytes();
    let hi = u64::from_be_bytes(bytes[0..8].try_into().expect("blake3 high half"));
    let lo = u64::from_be_bytes(bytes[8..16].try_into().expect("blake3 low half"));
    (hi, lo)
}

pub fn load_dedup_index(path: &Path) -> Result<(u64, Vec<DedupIndexEntry>), StorageError> {
    ensure_cache_fresh(path)?;
    let guard = lock_storage_mutex(cache(), "dedup")?;
    let entry = guard
        .get(path)
        .ok_or_else(|| StorageError::NotFound("dedup cache not initialized".to_string()))?;
    Ok((entry.sequence, entry.entries.clone()))
}

pub fn dedup_cache_ready(path: &Path) -> bool {
    lock_storage_mutex(cache(), "dedup")
        .map(|guard| guard.contains_key(path))
        .unwrap_or(false)
}

pub fn discard_dedup_cache(path: &Path) {
    if let Ok(mut guard) = lock_storage_mutex(cache(), "dedup") {
        guard.remove(path);
    }
}

pub fn lookup_entry_by_id(path: &Path, entry_id: u64) -> Result<DedupIndexEntry, StorageError> {
    ensure_cache_fresh(path)?;
    let guard = lock_storage_mutex(cache(), "dedup")?;
    let cached = guard
        .get(path)
        .ok_or_else(|| StorageError::NotFound("dedup cache not initialized".to_string()))?;
    cached
        .id_index
        .get(&entry_id)
        .and_then(|index| cached.entries.get(*index))
        .cloned()
        .ok_or_else(|| StorageError::NotFound("dedup entry not found".to_string()))
}

pub fn lookup_identity(
    path: &Path,
    fp_hi: u64,
    fp_lo: u64,
    payload_checksum: u32,
    logical_len: u32,
) -> Result<DedupLookup, StorageError> {
    ensure_cache_fresh(path)?;
    let guard = lock_storage_mutex(cache(), "dedup")?;
    let cached = guard
        .get(path)
        .ok_or_else(|| StorageError::NotFound("dedup cache not initialized".to_string()))?;
    if let Some(index) =
        cached
            .identity_index
            .get(&identity_key(fp_hi, fp_lo, payload_checksum, logical_len))
    {
        return Ok(DedupLookup::Hit(cached.entries[*index].clone()));
    }

    if cached
        .fingerprint_index
        .contains_key(&fingerprint_key(fp_hi, fp_lo))
    {
        Ok(DedupLookup::Collision)
    } else {
        Ok(DedupLookup::Miss)
    }
}

pub fn upsert_dedup_entry(
    path: &Path,
    mut entry: DedupIndexEntry,
) -> Result<DedupUpsertResult, StorageError> {
    ensure_cache_fresh(path)?;
    ensure_log_format_cached(path)?;

    let mut guard = lock_storage_mutex(cache(), "dedup")?;
    let cached = guard
        .get_mut(path)
        .ok_or_else(|| StorageError::NotFound("dedup cache not initialized".to_string()))?;
    let key = identity_key_for(&entry);

    if let Some(index) = cached.identity_index.get(&key).copied() {
        let mut hit = cached.entries[index].clone();
        hit.ref_count = hit
            .ref_count
            .checked_add(1)
            .ok_or_else(|| StorageError::Conflict("dedup refcount overflow".to_string()))?;
        if hit.identity_version == 0 && entry.identity_version != 0 {
            hit.identity_version = entry.identity_version;
        }
        let header = append_segment_payload(
            path,
            SegmentKind::Dedup,
            5,
            LOG_PREFIX,
            &hit.encode(),
            false,
        )?;
        cached.entries[index] = hit.clone();
        cached.sequence = header.sequence;
        if !trust_hot_cache() {
            cached.stamp = file_stamp(path)?;
        }
        cached.log_format = true;
        return Ok(DedupUpsertResult::Hit(hit));
    }

    let fp_key = fingerprint_key_for(&entry);
    let saw_collision = cached.fingerprint_index.contains_key(&fp_key);

    entry.entry_id = cached.next_entry_id;
    let header = append_segment_payload(
        path,
        SegmentKind::Dedup,
        5,
        LOG_PREFIX,
        &entry.encode(),
        false,
    )?;
    let inserted_index = cached.entries.len();
    cached.entries.push(entry.clone());
    cached.identity_index.insert(key, inserted_index);
    cached.fingerprint_index.insert(fp_key, ());
    cached.id_index.insert(entry.entry_id, inserted_index);
    cached.next_entry_id = cached.next_entry_id.saturating_add(1);
    cached.sequence = header.sequence;
    if !trust_hot_cache() {
        cached.stamp = file_stamp(path)?;
    }
    cached.log_format = true;

    if saw_collision {
        Ok(DedupUpsertResult::CollisionInserted(entry))
    } else {
        Ok(DedupUpsertResult::Inserted(entry))
    }
}

pub fn insert_dedup_collision_entry(
    path: &Path,
    mut entry: DedupIndexEntry,
) -> Result<DedupIndexEntry, StorageError> {
    ensure_cache_fresh(path)?;
    ensure_log_format_cached(path)?;

    let mut guard = lock_storage_mutex(cache(), "dedup")?;
    let cached = guard
        .get_mut(path)
        .ok_or_else(|| StorageError::NotFound("dedup cache not initialized".to_string()))?;
    let key = identity_key_for(&entry);
    let fp_key = fingerprint_key_for(&entry);
    entry.entry_id = cached.next_entry_id;
    let header = append_segment_payload(
        path,
        SegmentKind::Dedup,
        5,
        LOG_PREFIX,
        &entry.encode(),
        false,
    )?;
    let inserted_index = cached.entries.len();
    cached.entries.push(entry.clone());
    cached.identity_index.insert(key, inserted_index);
    cached.fingerprint_index.insert(fp_key, ());
    cached.id_index.insert(entry.entry_id, inserted_index);
    cached.next_entry_id = cached.next_entry_id.saturating_add(1);
    cached.sequence = header.sequence;
    if !trust_hot_cache() {
        cached.stamp = file_stamp(path)?;
    }
    cached.log_format = true;
    Ok(entry)
}

pub fn decrement_ref_count(path: &Path, entry_id: u64) -> Result<DedupIndexEntry, StorageError> {
    let mut updated = decrement_ref_counts(path, &[entry_id])?;
    updated
        .pop()
        .ok_or_else(|| StorageError::NotFound("dedup entry not found".to_string()))
}

pub fn decrement_ref_counts(
    path: &Path,
    entry_ids: &[u64],
) -> Result<Vec<DedupIndexEntry>, StorageError> {
    if entry_ids.is_empty() {
        return Ok(Vec::new());
    }

    ensure_cache_fresh(path)?;
    ensure_log_format_cached(path)?;

    let mut guard = lock_storage_mutex(cache(), "dedup")?;
    let cached = guard
        .get_mut(path)
        .ok_or_else(|| StorageError::NotFound("dedup cache not initialized".to_string()))?;
    let mut updated = Vec::new();
    let mut encoded = Vec::with_capacity(entry_ids.len() * DEDUP_RECORD_SIZE);
    for entry_id in entry_ids {
        let Some(index) = cached.id_index.get(entry_id).copied() else {
            return Err(StorageError::NotFound("dedup entry not found".to_string()));
        };
        if cached.entries[index].ref_count == 0 {
            return Err(StorageError::Conflict(
                "dedup refcount underflow".to_string(),
            ));
        }
        cached.entries[index].ref_count -= 1;
        encoded.extend_from_slice(&cached.entries[index].encode());
        updated.push(cached.entries[index].clone());
    }

    let header = append_segment_payload(path, SegmentKind::Dedup, 5, LOG_PREFIX, &encoded, false)?;
    cached.sequence = header.sequence;
    if !trust_hot_cache() {
        cached.stamp = file_stamp(path)?;
    }
    cached.log_format = true;
    Ok(updated)
}

pub fn rebuild_ref_counts(path: &Path, referenced_entries: &[u64]) -> Result<usize, StorageError> {
    let (_seq, mut entries) = load_dedup_index(path)?;
    let mut refs = HashMap::<u64, u32>::new();
    for entry_id in referenced_entries {
        let next = refs.entry(*entry_id).or_insert(0);
        *next = next
            .checked_add(1)
            .ok_or_else(|| StorageError::Conflict("dedup refcount overflow".to_string()))?;
    }

    ensure_log_format(path, &entries)?;
    let mut changed = 0usize;
    let mut encoded = Vec::new();
    for entry in &mut entries {
        let target = refs.get(&entry.entry_id).copied().unwrap_or(0);
        if entry.ref_count != target {
            entry.ref_count = target;
            changed += 1;
            encoded.extend_from_slice(&entry.encode());
        }
    }

    if changed > 0 {
        let header =
            append_segment_payload(path, SegmentKind::Dedup, 5, LOG_PREFIX, &encoded, false)?;
        update_cache(path, header.sequence, entries)?;
    } else {
        update_cache(path, 0, entries)?;
    }
    Ok(changed)
}

fn decode_payload(payload: &[u8]) -> Result<Vec<DedupIndexEntry>, StorageError> {
    if payload.is_empty() {
        return Ok(Vec::new());
    }
    if payload.starts_with(LOG_PREFIX) {
        return decode_log_payload(payload);
    }
    decode_legacy_payload(payload)
}

fn decode_legacy_payload(payload: &[u8]) -> Result<Vec<DedupIndexEntry>, StorageError> {
    if payload.len() < 8 {
        return Err(StorageError::Corrupt("dedup payload too short".to_string()));
    }

    let raw_count = u64::from_le_bytes(
        payload[0..8]
            .try_into()
            .map_err(|_| StorageError::Corrupt("dedup count parse failed".to_string()))?,
    );
    let count = checked_usize_from_u64(raw_count, "dedup count")?;
    if payload.len().saturating_sub(8) / DEDUP_RECORD_SIZE < count {
        return Err(StorageError::Corrupt("dedup payload truncated".to_string()));
    }
    let mut offset = 8usize;
    let mut entries = Vec::with_capacity(count);
    for _ in 0..count {
        if payload.len() < offset + DEDUP_RECORD_SIZE {
            return Err(StorageError::Corrupt("dedup payload truncated".to_string()));
        }
        entries.push(DedupIndexEntry::decode(
            &payload[offset..offset + DEDUP_RECORD_SIZE],
        )?);
        offset += DEDUP_RECORD_SIZE;
    }
    Ok(entries)
}

fn decode_log_payload(payload: &[u8]) -> Result<Vec<DedupIndexEntry>, StorageError> {
    if payload.len() < LOG_PREFIX.len() {
        return Err(StorageError::Corrupt(
            "dedup log payload too short".to_string(),
        ));
    }
    let mut offset = LOG_PREFIX.len();
    let mut latest = HashMap::<u64, DedupIndexEntry>::new();
    while offset < payload.len() {
        if payload.len() < offset + DEDUP_RECORD_SIZE {
            return Err(StorageError::Corrupt(
                "dedup log payload truncated".to_string(),
            ));
        }
        let entry = DedupIndexEntry::decode(&payload[offset..offset + DEDUP_RECORD_SIZE])?;
        latest.insert(entry.entry_id, entry);
        offset += DEDUP_RECORD_SIZE;
    }
    let mut entries = latest.into_values().collect::<Vec<_>>();
    entries.sort_by_key(|entry| entry.entry_id);
    Ok(entries)
}

fn ensure_log_format(path: &Path, entries: &[DedupIndexEntry]) -> Result<(), StorageError> {
    let (_, payload) = read_segment_file(path, SegmentKind::Dedup)?;
    if payload.starts_with(LOG_PREFIX) {
        return Ok(());
    }
    persist_dedup_entries(path, entries)
}

fn ensure_log_format_cached(path: &Path) -> Result<(), StorageError> {
    let entries = {
        let guard = lock_storage_mutex(cache(), "dedup")?;
        let cached = guard
            .get(path)
            .ok_or_else(|| StorageError::NotFound("dedup cache not initialized".to_string()))?;
        if cached.log_format || cached.entries.is_empty() {
            return Ok(());
        }
        cached.entries.clone()
    };

    persist_dedup_entries(path, &entries)
}

pub fn persist_dedup_entries(path: &Path, entries: &[DedupIndexEntry]) -> Result<(), StorageError> {
    let payload = encode_log_payload(entries);
    let sequence = entries.len() as u64;
    write_segment_file(path, SegmentKind::Dedup, 5, sequence, &payload)?;
    update_cache(path, sequence, entries.to_vec())
}

fn encode_log_payload(entries: &[DedupIndexEntry]) -> Vec<u8> {
    let mut payload = Vec::with_capacity(LOG_PREFIX.len() + entries.len() * DEDUP_RECORD_SIZE);
    payload.extend_from_slice(LOG_PREFIX);
    for entry in entries {
        payload.extend_from_slice(&entry.encode());
    }
    payload
}

fn update_cache(
    path: &Path,
    sequence: u64,
    entries: Vec<DedupIndexEntry>,
) -> Result<(), StorageError> {
    let stamp = file_stamp(path)?;
    let sequence = if sequence == 0 {
        lock_storage_mutex(cache(), "dedup")?
            .get(path)
            .map(|entry| entry.sequence)
            .unwrap_or(0)
    } else {
        sequence
    };
    lock_storage_mutex(cache(), "dedup")?.insert(
        path.to_path_buf(),
        build_cached_dedup(stamp, sequence, entries, true),
    );
    Ok(())
}

pub fn sync_dedup(path: &Path) -> Result<(), StorageError> {
    sync_segment_file(path)
}
