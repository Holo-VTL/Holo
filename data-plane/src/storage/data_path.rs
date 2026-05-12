use std::borrow::Cow;
use std::collections::{BTreeSet, HashMap};
use std::ffi::OsStr;
use std::fs::{self, File};
use std::io::{ErrorKind, Read, Seek, SeekFrom};
use std::os::unix::fs::FileExt;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use super::blk_map::{
    append_blk_map_record, load_blk_map_records, locate_active_record, mark_blk_map_stale_batch,
    persist_blk_map_records, sync_blk_map, BlkMapRecord, BlkMapState,
};
use super::compression::{compress_payload, decompress_payload, CompressionCodec};
use super::dedup::{
    decrement_ref_counts, fingerprint128, insert_dedup_collision_entry, load_dedup_index,
    lookup_identity, persist_dedup_entries, rebuild_ref_counts, sync_dedup, upsert_dedup_entry,
    DedupIndexEntry, DedupLookup, DedupUpsertResult, DEDUP_IDENTITY_BLAKE3_128,
};
use super::layout::{checksum32, integrity32, LayoutPaths, SegmentKind, STORAGE_LAYOUT_VERSION};
use super::map_lookup::{
    append_lookup_record, locate_logical_block, persist_lookup_records,
    rebuild_lookup_from_blk_map, sync_lookup, MapLookupRecord,
};
use super::metadata::{
    checked_usize_from_u64, load_checkpoint_page, lock_storage_mutex, modified_nanos_from_result,
    persist_checkpoint_page, quarantine_invalid_metadata, CheckpointFlags, MetadataCheckpoint,
    StorageError,
};
use super::reclaim::{refresh_reclaim_safety, upsert_reclaim_candidates, ReclaimReason};
use super::segment::{
    append_segment_payload_parts, read_segment_header, segment_payload_offset, sync_segment_file,
    write_segment_file,
};
use super::segment_index::{
    data_segment_path, flush_segment_index_for_append, invalidate_segment_index_cache,
    load_segment_index, load_segment_index_for_append, persist_segment_index,
    prepare_active_segment_for_append, record_blob_append, store_segment_index_clean,
    store_segment_index_for_append, sync_indexed_segments, SegmentDescriptor, SegmentIndex,
    SegmentState,
};

const DATA_BLOB_HEADER_SIZE: usize = 24;
const DATA_LOG_PREFIX: &[u8; 4] = b"DTV2";
const DEFAULT_SYNC_EVERY_WRITES: u32 = 64;
const DEFAULT_SLOW_WRITE_STAGE_MS: u64 = 1000;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct WriteOptions {
    pub dedup_enabled: bool,
    pub preferred_codec: CompressionCodec,
    pub force_sync: bool,
    pub payload_checksum_enabled: bool,
}

impl Default for WriteOptions {
    fn default() -> Self {
        Self {
            dedup_enabled: true,
            preferred_codec: CompressionCodec::Lz4,
            force_sync: false,
            payload_checksum_enabled: true,
        }
    }
}

impl WriteOptions {
    pub fn throughput_default() -> Self {
        Self {
            dedup_enabled: false,
            preferred_codec: CompressionCodec::None,
            force_sync: false,
            payload_checksum_enabled: false,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum IngestFailpoint {
    AfterDirtyCheckpoint,
    AfterBlobPersist,
    AfterBlkMapAppend,
    AfterLookupAppend,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WriteReport {
    pub record_id: u64,
    pub dedup_entry_id: u64,
    pub dedup_hit: bool,
    pub collision_detected: bool,
    pub codec_used: CompressionCodec,
    pub checkpoint_epoch: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LogicalReadResult {
    pub record_id: u64,
    pub logical_start: u64,
    pub logical_len: u32,
    pub dedup_entry_id: u64,
    pub codec_used: CompressionCodec,
    pub payload: Vec<u8>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UnmapReport {
    pub requested_start: u64,
    pub requested_len: u32,
    pub staled_record_ids: Vec<u64>,
    pub dedup_refcount_decrements: u32,
    pub reclaim_safe_promotions: u32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RecoveryReport {
    pub dirty_detected: bool,
    pub rebuilt_lookup_entries: usize,
    pub dedup_refcount_repaired: usize,
    pub reclaim_updates: usize,
    pub checkpoint_epoch: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct DataBlob {
    pub(crate) blob_id: u64,
    pub(crate) codec: CompressionCodec,
    pub(crate) logical_len: u32,
    pub(crate) stored_len: u32,
    pub(crate) payload_checksum: u32,
    pub(crate) bytes: Vec<u8>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct DataBlobMeta {
    blob_id: u64,
    codec: CompressionCodec,
    logical_len: u32,
    stored_len: u32,
    payload_checksum: u32,
    payload_offset: u64,
    v2_integrity: bool,
}

#[derive(Debug, Clone)]
struct CachedDataIndex {
    stamp: FileStamp,
    sequence: u64,
    blobs: Vec<DataBlobMeta>,
    is_log_format: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct FileStamp {
    len: u64,
    modified_nanos: u128,
}

fn data_cache() -> &'static Mutex<HashMap<PathBuf, CachedDataIndex>> {
    static CACHE: OnceLock<Mutex<HashMap<PathBuf, CachedDataIndex>>> = OnceLock::new();
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

fn pending_sync_writes() -> &'static Mutex<HashMap<PathBuf, u32>> {
    static CACHE: OnceLock<Mutex<HashMap<PathBuf, u32>>> = OnceLock::new();
    CACHE.get_or_init(|| Mutex::new(HashMap::new()))
}

fn slow_write_stage_threshold() -> Duration {
    static THRESHOLD: OnceLock<Duration> = OnceLock::new();
    *THRESHOLD.get_or_init(|| {
        let millis = std::env::var("HOLO_STORAGE_SLOW_WRITE_STAGE_MS")
            .ok()
            .and_then(|raw| raw.parse::<u64>().ok())
            .filter(|value| *value > 0)
            .unwrap_or(DEFAULT_SLOW_WRITE_STAGE_MS);
        Duration::from_millis(millis)
    })
}

struct StorageWriteTrace<'a> {
    root: &'a Path,
    logical_start: u64,
    payload_len: usize,
    started_at: Instant,
    last_at: Instant,
    threshold: Duration,
}

impl<'a> StorageWriteTrace<'a> {
    fn new(paths: &'a LayoutPaths, logical_start: u64, payload_len: usize) -> Self {
        let now = Instant::now();
        Self {
            root: &paths.root,
            logical_start,
            payload_len,
            started_at: now,
            last_at: now,
            threshold: slow_write_stage_threshold(),
        }
    }

    fn mark(&mut self, stage: &str) {
        let now = Instant::now();
        let elapsed = now.duration_since(self.last_at);
        if elapsed >= self.threshold {
            eprintln!(
                "[storage_write_slow_stage] stage={stage} elapsed_ms={} logical_start={} payload_len={} root={}",
                elapsed.as_millis(),
                self.logical_start,
                self.payload_len,
                self.root.display()
            );
        }
        self.last_at = now;
    }

    fn finish(&self, record_id: u64, dedup_hit: bool, should_flush: bool) {
        let elapsed = self.started_at.elapsed();
        if elapsed >= self.threshold {
            eprintln!(
                "[storage_write_slow_total] elapsed_ms={} logical_start={} payload_len={} record_id={} dedup_hit={} flushed={} root={}",
                elapsed.as_millis(),
                self.logical_start,
                self.payload_len,
                record_id,
                dedup_hit,
                should_flush,
                self.root.display()
            );
        }
    }
}

fn checkpoint_cache() -> &'static Mutex<HashMap<PathBuf, MetadataCheckpoint>> {
    static CACHE: OnceLock<Mutex<HashMap<PathBuf, MetadataCheckpoint>>> = OnceLock::new();
    CACHE.get_or_init(|| Mutex::new(HashMap::new()))
}

fn data_readers() -> &'static Mutex<HashMap<PathBuf, Arc<File>>> {
    static CACHE: OnceLock<Mutex<HashMap<PathBuf, Arc<File>>>> = OnceLock::new();
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

fn load_checkpoint_cached(path: &Path) -> Result<MetadataCheckpoint, StorageError> {
    if trust_hot_cache() {
        let guard = lock_storage_mutex(checkpoint_cache(), "checkpoint")?;
        if let Some(checkpoint) = guard.get(path) {
            return Ok(checkpoint.clone());
        }
    }
    let checkpoint = load_checkpoint_page(path)?;
    lock_storage_mutex(checkpoint_cache(), "checkpoint")?
        .insert(path.to_path_buf(), checkpoint.clone());
    Ok(checkpoint)
}

fn persist_checkpoint_cached(
    path: &Path,
    checkpoint: &MetadataCheckpoint,
) -> Result<(), StorageError> {
    persist_checkpoint_page(path, checkpoint)?;
    lock_storage_mutex(checkpoint_cache(), "checkpoint")?
        .insert(path.to_path_buf(), checkpoint.clone());
    Ok(())
}

pub(crate) fn mark_checkpoint_dirty(
    paths: &LayoutPaths,
) -> Result<MetadataCheckpoint, StorageError> {
    let mut checkpoint = load_checkpoint_cached(&paths.metadata_file)?;
    if checkpoint.flags == CheckpointFlags::Clean {
        checkpoint.epoch = checkpoint.epoch.saturating_add(1);
        checkpoint.flags = CheckpointFlags::Dirty;
        persist_checkpoint_cached(&paths.metadata_file, &checkpoint)?;
    }
    Ok(checkpoint)
}

pub(crate) fn mark_checkpoint_clean(
    paths: &LayoutPaths,
) -> Result<MetadataCheckpoint, StorageError> {
    let mut checkpoint = load_checkpoint_cached(&paths.metadata_file)?;
    checkpoint.epoch = checkpoint.epoch.saturating_add(1);
    checkpoint.flags = CheckpointFlags::Clean;
    persist_checkpoint_cached(&paths.metadata_file, &checkpoint)?;
    Ok(checkpoint)
}

fn invalidate_checkpoint_cache(path: &Path) {
    match lock_storage_mutex(checkpoint_cache(), "checkpoint") {
        Ok(mut guard) => {
            guard.remove(path);
        }
        Err(err) => eprintln!("[storage] failed to invalidate checkpoint cache: {err}"),
    }
}

fn invalidate_data_reader(path: &Path) {
    match lock_storage_mutex(data_readers(), "data reader") {
        Ok(mut guard) => {
            guard.remove(path);
        }
        Err(err) => eprintln!("[storage] failed to invalidate data reader cache: {err}"),
    }
}

pub(crate) fn invalidate_data_segment_cache(path: &Path) {
    invalidate_data_reader(path);
    match lock_storage_mutex(data_cache(), "data") {
        Ok(mut guard) => {
            guard.remove(path);
        }
        Err(err) => eprintln!("[storage] failed to invalidate data cache: {err}"),
    }
}

pub fn discard_layout_caches(paths: &LayoutPaths) {
    if let Ok(mut guard) = lock_storage_mutex(checkpoint_cache(), "checkpoint") {
        guard.remove(&paths.metadata_file);
    }
    if let Ok(mut guard) = lock_storage_mutex(pending_sync_writes(), "pending write") {
        guard.remove(&paths.metadata_file);
    }
    if let Ok(mut guard) = lock_storage_mutex(data_readers(), "data reader") {
        guard.retain(|path, _| !path.starts_with(&paths.root));
    }
    if let Ok(mut guard) = lock_storage_mutex(data_cache(), "data") {
        guard.retain(|path, _| !path.starts_with(&paths.root));
    }
    invalidate_segment_index_cache(&paths.segment_index_file);
}

impl DataBlob {
    fn encode(&self) -> Vec<u8> {
        encode_data_blob_record(
            self.blob_id,
            self.codec,
            self.logical_len,
            self.stored_len,
            self.payload_checksum,
            &self.bytes,
        )
    }
}

fn encode_data_blob_record(
    blob_id: u64,
    codec: CompressionCodec,
    logical_len: u32,
    stored_len: u32,
    payload_checksum: u32,
    bytes: &[u8],
) -> Vec<u8> {
    let mut out = Vec::with_capacity(DATA_BLOB_HEADER_SIZE + bytes.len());
    out.extend_from_slice(&blob_id.to_le_bytes());
    out.push(codec as u8);
    out.extend_from_slice(&[0u8; 3]);
    out.extend_from_slice(&logical_len.to_le_bytes());
    out.extend_from_slice(&stored_len.to_le_bytes());
    out.extend_from_slice(&payload_checksum.to_le_bytes());
    out.extend_from_slice(bytes);
    out
}

fn encode_data_blob_header(
    blob_id: u64,
    codec: CompressionCodec,
    logical_len: u32,
    stored_len: u32,
    payload_checksum: u32,
) -> Vec<u8> {
    let mut out = Vec::with_capacity(DATA_BLOB_HEADER_SIZE);
    out.extend_from_slice(&blob_id.to_le_bytes());
    out.push(codec as u8);
    out.extend_from_slice(&[0u8; 3]);
    out.extend_from_slice(&logical_len.to_le_bytes());
    out.extend_from_slice(&stored_len.to_le_bytes());
    out.extend_from_slice(&payload_checksum.to_le_bytes());
    out
}

fn blob_integrity_checksum(blob_id: u64, logical_len: u32, bytes: &[u8]) -> u32 {
    let mut payload = Vec::with_capacity(12 + bytes.len());
    payload.extend_from_slice(&blob_id.to_le_bytes());
    payload.extend_from_slice(&logical_len.to_le_bytes());
    payload.extend_from_slice(bytes);
    integrity32(&payload)
}

fn prepare_payload_for_write(
    codec: CompressionCodec,
    payload: &[u8],
) -> Result<(CompressionCodec, Cow<'_, [u8]>, bool), StorageError> {
    match codec {
        CompressionCodec::None => Ok((CompressionCodec::None, Cow::Borrowed(payload), false)),
        CompressionCodec::Rle | CompressionCodec::Lz4 | CompressionCodec::Zlib => {
            let (selected_codec, encoded, compressed) = compress_payload(codec, payload)?;
            Ok((selected_codec, Cow::Owned(encoded), compressed))
        }
    }
}

pub fn write_logical_block(
    paths: &LayoutPaths,
    logical_start: u64,
    payload: &[u8],
    filemark_count: u32,
    options: WriteOptions,
    failpoint: Option<IngestFailpoint>,
) -> Result<WriteReport, StorageError> {
    if payload.is_empty() {
        return Err(StorageError::Conflict(
            "logical payload cannot be empty".to_string(),
        ));
    }

    let mut trace = StorageWriteTrace::new(paths, logical_start, payload.len());
    let mut checkpoint = load_checkpoint_cached(&paths.metadata_file)?;
    trace.mark("load_checkpoint");
    let checkpoint_was_clean = checkpoint.flags == CheckpointFlags::Clean;
    if checkpoint_was_clean {
        checkpoint.epoch = checkpoint.epoch.saturating_add(1);
        checkpoint.flags = CheckpointFlags::Dirty;
        persist_checkpoint_cached(&paths.metadata_file, &checkpoint)?;
    }
    trace.mark("dirty_checkpoint");

    if failpoint == Some(IngestFailpoint::AfterDirtyCheckpoint) {
        return Err(StorageError::Conflict(
            "ingest interrupted after dirty checkpoint".to_string(),
        ));
    }

    let logical_len = checked_u32_len(payload.len(), "logical payload length")?;
    let payload_checksum = if options.payload_checksum_enabled {
        checksum32(payload)
    } else {
        0
    };
    trace.mark("prepare_payload_metadata");

    let mut dedup_hit = false;
    let mut collision_detected = false;
    let (dedup_entry_id, blob_id, codec_used, stored_len, physical_segment_id) = if options
        .dedup_enabled
    {
        let (fp_hi, fp_lo) = fingerprint128(payload);
        let identity = lookup_identity(
            &paths.dedup_file,
            fp_hi,
            fp_lo,
            payload_checksum,
            logical_len,
        )?;
        match identity {
            DedupLookup::Hit(entry) => {
                if !should_verify_dedup_hit(&entry)
                    || dedup_entry_matches_payload(paths, &entry, payload)?
                {
                    dedup_hit = true;
                    let _ = upsert_dedup_entry(
                        &paths.dedup_file,
                        DedupIndexEntry {
                            entry_id: 0,
                            fingerprint_hi: entry.fingerprint_hi,
                            fingerprint_lo: entry.fingerprint_lo,
                            payload_checksum: entry.payload_checksum,
                            logical_len: entry.logical_len,
                            stored_blob_id: entry.stored_blob_id,
                            stored_len: entry.stored_len,
                            compression: entry.compression,
                            identity_version: DEDUP_IDENTITY_BLAKE3_128,
                            ref_count: 1,
                        },
                    )?;
                    let physical_segment_id = locate_blob_segment_id(paths, entry.stored_blob_id)?;
                    (
                        entry.entry_id,
                        entry.stored_blob_id,
                        entry.compression,
                        entry.stored_len,
                        physical_segment_id,
                    )
                } else {
                    collision_detected = true;
                    let (selected_codec, encoded_payload, _compressed) =
                        prepare_payload_for_write(options.preferred_codec, payload)?;
                    let stored_len =
                        checked_u32_len(encoded_payload.len(), "encoded payload length")?;
                    let (physical_segment_id, blob_id) = append_data_blob(
                        paths,
                        selected_codec,
                        payload_checksum,
                        logical_len,
                        encoded_payload.as_ref(),
                        options.payload_checksum_enabled,
                    )?;
                    let inserted = insert_dedup_collision_entry(
                        &paths.dedup_file,
                        DedupIndexEntry {
                            entry_id: 0,
                            fingerprint_hi: fp_hi,
                            fingerprint_lo: fp_lo,
                            payload_checksum,
                            logical_len,
                            stored_blob_id: blob_id,
                            stored_len,
                            compression: selected_codec,
                            identity_version: DEDUP_IDENTITY_BLAKE3_128,
                            ref_count: 1,
                        },
                    )?;
                    (
                        inserted.entry_id,
                        blob_id,
                        selected_codec,
                        stored_len,
                        physical_segment_id,
                    )
                }
            }
            DedupLookup::Collision | DedupLookup::Miss => {
                collision_detected = matches!(identity, DedupLookup::Collision);
                let (selected_codec, encoded_payload, _compressed) =
                    prepare_payload_for_write(options.preferred_codec, payload)?;
                let stored_len = checked_u32_len(encoded_payload.len(), "encoded payload length")?;
                let (physical_segment_id, blob_id) = append_data_blob(
                    paths,
                    selected_codec,
                    payload_checksum,
                    logical_len,
                    encoded_payload.as_ref(),
                    options.payload_checksum_enabled,
                )?;

                match upsert_dedup_entry(
                    &paths.dedup_file,
                    DedupIndexEntry {
                        entry_id: 0,
                        fingerprint_hi: fp_hi,
                        fingerprint_lo: fp_lo,
                        payload_checksum,
                        logical_len,
                        stored_blob_id: blob_id,
                        stored_len,
                        compression: selected_codec,
                        identity_version: DEDUP_IDENTITY_BLAKE3_128,
                        ref_count: 1,
                    },
                )? {
                    DedupUpsertResult::Hit(entry) => {
                        dedup_hit = true;
                        let physical_segment_id =
                            locate_blob_segment_id(paths, entry.stored_blob_id)?;
                        (
                            entry.entry_id,
                            entry.stored_blob_id,
                            entry.compression,
                            entry.stored_len,
                            physical_segment_id,
                        )
                    }
                    DedupUpsertResult::Inserted(entry) => (
                        entry.entry_id,
                        blob_id,
                        selected_codec,
                        stored_len,
                        physical_segment_id,
                    ),
                    DedupUpsertResult::CollisionInserted(entry) => {
                        collision_detected = true;
                        (
                            entry.entry_id,
                            blob_id,
                            selected_codec,
                            stored_len,
                            physical_segment_id,
                        )
                    }
                }
            }
        }
    } else {
        let (selected_codec, encoded_payload, _compressed) =
            prepare_payload_for_write(options.preferred_codec, payload)?;
        let stored_len = checked_u32_len(encoded_payload.len(), "encoded payload length")?;
        let (physical_segment_id, blob_id) = append_data_blob(
            paths,
            selected_codec,
            payload_checksum,
            logical_len,
            encoded_payload.as_ref(),
            options.payload_checksum_enabled,
        )?;
        (0, blob_id, selected_codec, stored_len, physical_segment_id)
    };
    trace.mark("dedup_and_blob_append");

    if failpoint == Some(IngestFailpoint::AfterBlobPersist) {
        return Err(StorageError::Conflict(
            "ingest interrupted after blob persist".to_string(),
        ));
    }

    let appended = append_blk_map_record(
        &paths.blk_map_file,
        BlkMapRecord {
            record_id: 0,
            logical_start,
            logical_len,
            physical_segment_id,
            physical_offset: blob_id,
            filemark_count,
            state: BlkMapState::Active,
            dedup_entry_id,
            compression: codec_used,
            compressed_len: stored_len,
            payload_checksum,
        },
    )?;
    trace.mark("append_blk_map");

    if failpoint == Some(IngestFailpoint::AfterBlkMapAppend) {
        return Err(StorageError::Conflict(
            "ingest interrupted after blk_map append".to_string(),
        ));
    }

    append_lookup_record(
        &paths.lookup_file,
        MapLookupRecord {
            lookup_id: 0,
            logical_min: appended.logical_start,
            logical_max: appended.logical_end() - 1,
            blk_map_ref_start: appended.record_id,
            blk_map_ref_end: appended.record_id,
        },
    )?;
    trace.mark("append_lookup");

    if failpoint == Some(IngestFailpoint::AfterLookupAppend) {
        return Err(StorageError::Conflict(
            "ingest interrupted after lookup append".to_string(),
        ));
    }

    let pending_writes = record_pending_write(&paths.metadata_file)?;
    trace.mark("record_pending_write");
    let should_flush =
        options.force_sync || pending_writes >= sync_every_writes() || failpoint.is_some();
    if should_flush {
        sync_layout_segments(paths)?;
        trace.mark("sync_layout_segments");
        checkpoint.flags = CheckpointFlags::Clean;
        checkpoint.epoch = checkpoint.epoch.saturating_add(1);
        persist_checkpoint_cached(&paths.metadata_file, &checkpoint)?;
        clear_pending_writes(&paths.metadata_file)?;
        trace.mark("clean_checkpoint");
    }
    trace.finish(appended.record_id, dedup_hit, should_flush);

    Ok(WriteReport {
        record_id: appended.record_id,
        dedup_entry_id,
        dedup_hit,
        collision_detected,
        codec_used,
        checkpoint_epoch: checkpoint.epoch,
    })
}

pub fn flush_pending_writes(paths: &LayoutPaths) -> Result<bool, StorageError> {
    let checkpoint = load_checkpoint_cached(&paths.metadata_file)?;
    let pending = pending_count(&paths.metadata_file)?;
    if pending == 0 && checkpoint.flags == CheckpointFlags::Clean {
        return Ok(false);
    }
    sync_layout_segments(paths)?;

    let mut next = checkpoint;
    next.epoch = next.epoch.saturating_add(1);
    next.flags = CheckpointFlags::Clean;
    persist_checkpoint_cached(&paths.metadata_file, &next)?;
    clear_pending_writes(&paths.metadata_file)?;
    Ok(true)
}

pub fn reset_layout_for_overwrite(paths: &LayoutPaths) -> Result<(), StorageError> {
    let mut checkpoint = load_checkpoint_cached(&paths.metadata_file)?;
    checkpoint.epoch = checkpoint.epoch.saturating_add(1);
    checkpoint.flags = CheckpointFlags::Dirty;
    persist_checkpoint_cached(&paths.metadata_file, &checkpoint)?;

    persist_blk_map_records(&paths.blk_map_file, &[])?;
    persist_lookup_records(&paths.lookup_file, &[])?;
    write_segment_file(
        &paths.reclaim_file,
        SegmentKind::Reclaim,
        4,
        checkpoint.epoch,
        &[],
    )?;
    persist_dedup_entries(&paths.dedup_file, &[])?;

    remove_data_segment_files(paths)?;
    let index = super::segment_index::SegmentIndex::new(
        super::segment_index::configured_max_segment_size(),
    );
    let active_path = data_segment_path(paths, 0);
    write_segment_file(&active_path, SegmentKind::Data, 1, checkpoint.epoch, &[])?;
    persist_segment_index(&paths.segment_index_file, &index)?;
    store_segment_index_clean(&paths.segment_index_file, index)?;
    invalidate_data_reader(&active_path);
    update_data_cache(&active_path, checkpoint.epoch, Vec::new())?;

    checkpoint.epoch = checkpoint.epoch.saturating_add(1);
    checkpoint.flags = CheckpointFlags::Clean;
    checkpoint.active_blk_map_root = 2;
    checkpoint.active_lookup_root = 3;
    persist_checkpoint_cached(&paths.metadata_file, &checkpoint)?;
    clear_pending_writes(&paths.metadata_file)?;
    Ok(())
}

fn remove_data_segment_files(paths: &LayoutPaths) -> Result<(), StorageError> {
    let mut removed = false;
    match fs::read_dir(&paths.root) {
        Ok(entries) => {
            for entry in entries {
                let entry = entry?;
                if !entry.file_type()?.is_file() {
                    continue;
                }
                if is_indexed_data_segment_name(&entry.file_name()) {
                    let path = entry.path();
                    invalidate_data_segment_cache(&path);
                    fs::remove_file(path)?;
                    removed = true;
                }
            }
        }
        Err(err) if err.kind() == ErrorKind::NotFound => {}
        Err(err) => return Err(StorageError::Io(err)),
    }
    if paths.data_file.exists() {
        invalidate_data_segment_cache(&paths.data_file);
        fs::remove_file(&paths.data_file)?;
        removed = true;
    }
    if removed {
        File::open(&paths.root)?.sync_all()?;
    }
    Ok(())
}

fn is_indexed_data_segment_name(name: &OsStr) -> bool {
    indexed_data_segment_seq(name).is_some()
}

fn indexed_data_segment_seq(name: &OsStr) -> Option<u32> {
    let name = name.to_str()?;
    const PREFIX: &str = "data_";
    const SUFFIX: &str = ".seg";
    if !name.starts_with(PREFIX) || !name.ends_with(SUFFIX) {
        return None;
    }
    let digits = &name[PREFIX.len()..name.len() - SUFFIX.len()];
    if digits.len() == 6 && digits.bytes().all(|byte| byte.is_ascii_digit()) {
        digits.parse::<u32>().ok()
    } else {
        None
    }
}

pub fn read_logical_block(
    paths: &LayoutPaths,
    logical_block: u64,
) -> Result<Option<LogicalReadResult>, StorageError> {
    let Some(lookup) = locate_logical_block(&paths.lookup_file, logical_block)? else {
        return Ok(None);
    };

    let Some(record) = locate_active_record(
        &paths.blk_map_file,
        logical_block,
        lookup.blk_map_ref_start,
        lookup.blk_map_ref_end,
    )?
    else {
        return Ok(None);
    };

    let blob = load_blob_by_location(paths, record.physical_segment_id, record.physical_offset)?;
    let logical_len =
        checked_usize_from_u64(u64::from(record.logical_len), "logical payload length")?;
    let payload = if record.compression == CompressionCodec::None {
        if blob.bytes.len() != logical_len {
            return Err(StorageError::Corrupt(
                "raw payload length mismatch".to_string(),
            ));
        }
        blob.bytes
    } else {
        decompress_payload(record.compression, &blob.bytes, logical_len)?
    };

    if record.payload_checksum != 0 && checksum32(&payload) != record.payload_checksum {
        return Err(StorageError::Corrupt(
            "payload checksum mismatch on read".to_string(),
        ));
    }

    Ok(Some(LogicalReadResult {
        record_id: record.record_id,
        logical_start: record.logical_start,
        logical_len: record.logical_len,
        dedup_entry_id: record.dedup_entry_id,
        codec_used: record.compression,
        payload,
    }))
}

pub fn run_unmap(
    paths: &LayoutPaths,
    logical_start: u64,
    logical_len: u32,
) -> Result<UnmapReport, StorageError> {
    let logical_end = logical_start.saturating_add(logical_len as u64);
    let (_seq, records) = load_blk_map_records(&paths.blk_map_file)?;

    let mut overlap_record_ids = Vec::new();
    let mut dedup_entry_ids = Vec::new();

    for record in records {
        if record.state != BlkMapState::Active {
            continue;
        }

        let overlap = logical_start < record.logical_end() && record.logical_start < logical_end;
        if !overlap {
            continue;
        }
        overlap_record_ids.push(record.record_id);
        if record.dedup_entry_id > 0 {
            dedup_entry_ids.push(record.dedup_entry_id);
        }
    }

    if overlap_record_ids.is_empty() {
        return Ok(UnmapReport {
            requested_start: logical_start,
            requested_len: logical_len,
            staled_record_ids: Vec::new(),
            dedup_refcount_decrements: 0,
            reclaim_safe_promotions: 0,
        });
    }

    let updated_records = mark_blk_map_stale_batch(&paths.blk_map_file, &overlap_record_ids)?;
    let staled_record_ids = updated_records
        .iter()
        .map(|record| record.record_id)
        .collect::<Vec<_>>();
    let dedup_refcount_decrements =
        decrement_ref_counts(&paths.dedup_file, &dedup_entry_ids)?.len() as u32;

    let _ = rebuild_lookup_from_blk_map(&paths.blk_map_file, &paths.lookup_file)?;
    let _ = upsert_reclaim_candidates(
        &paths.lookup_file,
        &paths.reclaim_file,
        &staled_record_ids,
        ReclaimReason::Truncated,
    )?;
    let reclaim_updates = refresh_reclaim_safety(&paths.lookup_file, &paths.reclaim_file)?;

    Ok(UnmapReport {
        requested_start: logical_start,
        requested_len: logical_len,
        staled_record_ids,
        dedup_refcount_decrements,
        reclaim_safe_promotions: reclaim_updates as u32,
    })
}

pub fn recover_dirty_state(paths: &LayoutPaths) -> Result<RecoveryReport, StorageError> {
    let mut checkpoint = match load_checkpoint_cached(&paths.metadata_file) {
        Ok(checkpoint) => checkpoint,
        Err(StorageError::Corrupt(err)) => {
            invalidate_checkpoint_cache(&paths.metadata_file);
            let _ = quarantine_invalid_metadata(&paths.metadata_file)?;
            return Err(StorageError::Corrupt(err));
        }
        Err(err) => return Err(err),
    };

    if checkpoint.flags == CheckpointFlags::Clean {
        return Ok(RecoveryReport {
            dirty_detected: false,
            rebuilt_lookup_entries: 0,
            dedup_refcount_repaired: 0,
            reclaim_updates: 0,
            checkpoint_epoch: checkpoint.epoch,
        });
    }

    let rebuilt_lookup_entries =
        rebuild_lookup_from_blk_map(&paths.blk_map_file, &paths.lookup_file)?;
    rebuild_segment_index_from_blk_map(paths)?;
    let referenced_dedup_entries = collect_active_dedup_entries(&paths.blk_map_file)?;
    let dedup_refcount_repaired = rebuild_ref_counts(&paths.dedup_file, &referenced_dedup_entries)?;
    let reclaim_updates = refresh_reclaim_safety(&paths.lookup_file, &paths.reclaim_file)?;

    checkpoint.epoch = checkpoint.epoch.saturating_add(1);
    checkpoint.flags = CheckpointFlags::Clean;
    persist_checkpoint_cached(&paths.metadata_file, &checkpoint)?;

    Ok(RecoveryReport {
        dirty_detected: true,
        rebuilt_lookup_entries,
        dedup_refcount_repaired,
        reclaim_updates,
        checkpoint_epoch: checkpoint.epoch,
    })
}

fn rebuild_segment_index_from_blk_map(paths: &LayoutPaths) -> Result<(), StorageError> {
    let max_segment_size = load_segment_index(&paths.segment_index_file)
        .map(|index| index.max_segment_size)
        .unwrap_or_else(|_| super::segment_index::configured_max_segment_size());
    let (_seq, records) = load_blk_map_records(&paths.blk_map_file)?;
    let referenced_segments: BTreeSet<u32> = records
        .iter()
        .filter(|record| record.state == BlkMapState::Active)
        .map(|record| {
            u32::try_from(record.physical_segment_id)
                .map_err(|_| StorageError::Corrupt("physical segment id exceeds u32".to_string()))
        })
        .collect::<Result<_, _>>()?;
    let highest_existing_segment = highest_indexed_data_segment(paths)?;

    let index = if referenced_segments.is_empty() {
        let active_path = data_segment_path(paths, 0);
        write_segment_file(&active_path, SegmentKind::Data, 1, 0, &[])?;
        invalidate_data_segment_cache(&active_path);
        SegmentIndex {
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
            next_segment_seq: highest_existing_segment
                .map(u64::from)
                .unwrap_or(0)
                .saturating_add(1)
                .max(1),
        }
    } else {
        let active_segment = *referenced_segments
            .iter()
            .next_back()
            .expect("referenced set cannot be empty");
        let next_segment_seq = highest_existing_segment
            .unwrap_or(active_segment)
            .max(active_segment)
            .saturating_add(1) as u64;
        let mut descriptors = Vec::with_capacity(referenced_segments.len());
        for segment_seq in referenced_segments {
            descriptors.push(rebuild_segment_descriptor(
                paths,
                segment_seq,
                if segment_seq == active_segment {
                    SegmentState::Active
                } else {
                    SegmentState::Sealed
                },
            )?);
        }
        SegmentIndex {
            max_segment_size,
            descriptors,
            next_segment_seq,
        }
    };

    persist_segment_index(&paths.segment_index_file, &index)?;
    store_segment_index_clean(&paths.segment_index_file, index)?;
    Ok(())
}

fn highest_indexed_data_segment(paths: &LayoutPaths) -> Result<Option<u32>, StorageError> {
    let mut highest = None;
    match fs::read_dir(&paths.root) {
        Ok(entries) => {
            for entry in entries {
                let entry = entry?;
                if !entry.file_type()?.is_file() {
                    continue;
                }
                if let Some(segment_seq) = indexed_data_segment_seq(&entry.file_name()) {
                    highest =
                        Some(highest.map_or(segment_seq, |current: u32| current.max(segment_seq)));
                }
            }
        }
        Err(err) if err.kind() == ErrorKind::NotFound => {}
        Err(err) => return Err(StorageError::Io(err)),
    }
    Ok(highest)
}

fn rebuild_segment_descriptor(
    paths: &LayoutPaths,
    segment_seq: u32,
    state: SegmentState,
) -> Result<SegmentDescriptor, StorageError> {
    let path = data_segment_path(paths, segment_seq);
    let header = read_segment_header(&path, SegmentKind::Data)?;
    let blobs = if header.payload_len == 0 {
        Vec::new()
    } else {
        scan_data_index(&path, header.version, header.payload_len)?.0
    };
    let first_blob_id = blobs.iter().map(|blob| blob.blob_id).min().unwrap_or(0);
    let last_blob_id = blobs.iter().map(|blob| blob.blob_id).max().unwrap_or(0);
    let live_bytes = blobs
        .iter()
        .fold(0u32, |acc, blob| acc.saturating_add(blob.stored_len));
    let compression = blobs
        .iter()
        .rev()
        .find_map(|blob| {
            if blob.codec == CompressionCodec::None {
                None
            } else {
                Some(blob.codec)
            }
        })
        .unwrap_or(CompressionCodec::None);
    Ok(SegmentDescriptor {
        segment_seq,
        payload_bytes: header.payload_len,
        first_blob_id,
        last_blob_id,
        state,
        compression,
        live_bytes,
    })
}

fn collect_active_dedup_entries(blk_map_path: &Path) -> Result<Vec<u64>, StorageError> {
    let (_seq, records) = load_blk_map_records(blk_map_path)?;
    Ok(records
        .into_iter()
        .filter(|record| record.state == BlkMapState::Active && record.dedup_entry_id > 0)
        .map(|record| record.dedup_entry_id)
        .collect())
}

pub(crate) fn append_data_blob(
    paths: &LayoutPaths,
    codec: CompressionCodec,
    _payload_checksum: u32,
    logical_len: u32,
    encoded_payload: &[u8],
    checksum_enabled: bool,
) -> Result<(u64, u64), StorageError> {
    let mut index = load_segment_index_for_append(&paths.segment_index_file)?;
    let record_len = DATA_BLOB_HEADER_SIZE + encoded_payload.len();
    let segment_seq =
        prepare_active_segment_for_append(paths, &mut index, record_len, DATA_LOG_PREFIX.len())?;
    let path = data_segment_path(paths, segment_seq);
    ensure_data_cache_fresh(&path)?;
    let blob_id = next_blob_id(&index);
    let rewrite_legacy = {
        let guard = lock_storage_mutex(data_cache(), "data")?;
        let entry = guard
            .get(&path)
            .ok_or_else(|| StorageError::NotFound("data cache not initialized".to_string()))?;
        !entry.is_log_format
    };

    if rewrite_legacy {
        let legacy_blobs = {
            let guard = lock_storage_mutex(data_cache(), "data")?;
            let entry = guard
                .get(&path)
                .ok_or_else(|| StorageError::NotFound("data cache not initialized".to_string()))?;
            entry.blobs.clone()
        };
        persist_data_blobs_as_log(&path, &legacy_blobs)?;
        ensure_data_cache_fresh(&path)?;
    }

    let stored_len = checked_u32_len(encoded_payload.len(), "encoded payload length")?;
    let blob_checksum = blob_integrity_checksum(blob_id, logical_len, encoded_payload);
    let encoded_header =
        encode_data_blob_header(blob_id, codec, logical_len, stored_len, blob_checksum);
    let header = append_segment_payload_parts(
        &path,
        SegmentKind::Data,
        1,
        DATA_LOG_PREFIX,
        &[encoded_header.as_slice(), encoded_payload],
        false,
        checksum_enabled,
    )?;
    let next_blob_meta = DataBlobMeta {
        blob_id,
        codec,
        logical_len,
        stored_len,
        payload_checksum: blob_checksum,
        payload_offset: segment_payload_offset(&header) + header.payload_len - stored_len as u64,
        v2_integrity: true,
    };
    let mut guard = lock_storage_mutex(data_cache(), "data")?;
    let entry = guard
        .get_mut(&path)
        .ok_or_else(|| StorageError::NotFound("data cache not initialized".to_string()))?;
    entry.sequence = header.sequence;
    entry.blobs.push(next_blob_meta);
    if !trust_hot_cache() {
        entry.stamp = file_stamp(&path)?;
    }
    entry.is_log_format = true;
    record_blob_append(
        &mut index,
        segment_seq,
        header.payload_len,
        blob_id,
        stored_len,
        codec,
    )?;
    store_segment_index_for_append(&paths.segment_index_file, index)?;
    Ok((segment_seq as u64, blob_id))
}

fn dedup_entry_matches_payload(
    paths: &LayoutPaths,
    entry: &DedupIndexEntry,
    payload: &[u8],
) -> Result<bool, StorageError> {
    let logical_len = checked_usize_from_u64(u64::from(entry.logical_len), "dedup logical length")?;
    if logical_len != payload.len() {
        return Ok(false);
    }
    let physical_segment_id = locate_blob_segment_id(paths, entry.stored_blob_id)?;
    let blob = load_blob_by_location(paths, physical_segment_id, entry.stored_blob_id)?;
    if blob.logical_len != entry.logical_len || blob.stored_len != entry.stored_len {
        return Ok(false);
    }
    let candidate = if blob.codec == CompressionCodec::None {
        blob.bytes
    } else {
        let blob_logical_len =
            checked_usize_from_u64(u64::from(blob.logical_len), "data blob logical length")?;
        decompress_payload(blob.codec, &blob.bytes, blob_logical_len)?
    };
    Ok(candidate == payload)
}

fn should_verify_dedup_hit(entry: &DedupIndexEntry) -> bool {
    if entry.identity_version != DEDUP_IDENTITY_BLAKE3_128 {
        return true;
    }
    std::env::var("HOLO_TAPE_DEDUP_VERIFY_HITS")
        .ok()
        .map(|raw| {
            !matches!(
                raw.trim().to_ascii_lowercase().as_str(),
                "0" | "false" | "no"
            )
        })
        .unwrap_or(true)
}

fn load_blob_by_id(path: &Path, blob_id: u64) -> Result<DataBlob, StorageError> {
    ensure_data_cache_fresh(path)?;
    let meta = {
        let guard = lock_storage_mutex(data_cache(), "data")?;
        let entry = guard
            .get(path)
            .ok_or_else(|| StorageError::NotFound("data cache not initialized".to_string()))?;
        entry
            .blobs
            .binary_search_by_key(&blob_id, |blob| blob.blob_id)
            .ok()
            .and_then(|idx| entry.blobs.get(idx))
            .cloned()
    }
    .ok_or_else(|| StorageError::NotFound("data blob not found".to_string()))?;
    let file = {
        let mut guard = lock_storage_mutex(data_readers(), "data reader")?;
        if !guard.contains_key(path) {
            guard.insert(path.to_path_buf(), Arc::new(File::open(path)?));
        }
        guard
            .get(path)
            .cloned()
            .ok_or_else(|| StorageError::NotFound("data reader not initialized".to_string()))?
    };
    let stored_len = checked_usize_from_u64(u64::from(meta.stored_len), "data blob stored length")?;
    let bytes = match read_blob_payload(file.as_ref(), meta.payload_offset, stored_len) {
        Ok(bytes) => bytes,
        Err(StorageError::Io(err)) if err.kind() == ErrorKind::UnexpectedEof => {
            let reopened = Arc::new(File::open(path)?);
            let bytes = read_blob_payload(reopened.as_ref(), meta.payload_offset, stored_len)?;
            lock_storage_mutex(data_readers(), "data reader")?.insert(path.to_path_buf(), reopened);
            bytes
        }
        Err(err) => return Err(err),
    };
    if meta.v2_integrity
        && meta.payload_checksum != blob_integrity_checksum(meta.blob_id, meta.logical_len, &bytes)
    {
        return Err(StorageError::Corrupt(
            "data blob integrity checksum mismatch".to_string(),
        ));
    }
    Ok(DataBlob {
        blob_id: meta.blob_id,
        codec: meta.codec,
        logical_len: meta.logical_len,
        stored_len: meta.stored_len,
        payload_checksum: meta.payload_checksum,
        bytes,
    })
}

fn next_blob_id(index: &super::segment_index::SegmentIndex) -> u64 {
    index
        .descriptors
        .iter()
        .map(|descriptor| descriptor.last_blob_id)
        .max()
        .unwrap_or(0)
        .saturating_add(1)
}

pub(crate) fn load_blob_by_location(
    paths: &LayoutPaths,
    segment_id: u64,
    blob_id: u64,
) -> Result<DataBlob, StorageError> {
    let index = load_segment_index_for_append(&paths.segment_index_file)?;
    if let Some(descriptor) = index.descriptor_for_segment(segment_id) {
        let result = load_blob_by_id(&data_segment_path(paths, descriptor.segment_seq), blob_id);
        if result.is_ok() || segment_id != 1 {
            return result;
        }
    } else if segment_id != 1 {
        return Err(StorageError::NotFound(
            "data segment descriptor not found".to_string(),
        ));
    }

    let Some(first_descriptor) = index.descriptors.first() else {
        return Err(StorageError::NotFound(
            "data segment descriptor not found".to_string(),
        ));
    };
    if first_descriptor.segment_seq != 0 {
        return Err(StorageError::NotFound(
            "legacy data segment descriptor not found".to_string(),
        ));
    }
    load_blob_by_id(
        &data_segment_path(paths, first_descriptor.segment_seq),
        blob_id,
    )
}

fn locate_blob_segment_id(paths: &LayoutPaths, blob_id: u64) -> Result<u64, StorageError> {
    let index = load_segment_index(&paths.segment_index_file)?;
    if let Some(descriptor) = index.descriptor_for_blob(blob_id) {
        return Ok(descriptor.segment_seq as u64);
    }
    for descriptor in &index.descriptors {
        if load_blob_by_id(&data_segment_path(paths, descriptor.segment_seq), blob_id).is_ok() {
            return Ok(descriptor.segment_seq as u64);
        }
    }
    Err(StorageError::NotFound(
        "data blob segment not found".to_string(),
    ))
}

fn read_blob_payload(file: &File, offset: u64, len: usize) -> Result<Vec<u8>, StorageError> {
    let mut bytes = vec![0u8; len];
    let mut read_total = 0usize;
    while read_total < len {
        let n = file.read_at(&mut bytes[read_total..], offset + read_total as u64)?;
        if n == 0 {
            return Err(StorageError::Io(std::io::Error::new(
                ErrorKind::UnexpectedEof,
                "short read from data segment",
            )));
        }
        read_total += n;
    }
    Ok(bytes)
}

fn ensure_data_cache_fresh(path: &Path) -> Result<(), StorageError> {
    if trust_hot_cache() {
        let guard = lock_storage_mutex(data_cache(), "data")?;
        if guard.contains_key(path) {
            return Ok(());
        }
    }

    let stamp = file_stamp(path)?;
    {
        let guard = lock_storage_mutex(data_cache(), "data")?;
        if let Some(entry) = guard.get(path) {
            if entry.stamp == stamp {
                return Ok(());
            }
        }
    }

    let header = read_segment_header(path, SegmentKind::Data)?;
    let (blobs, is_log_format) = if header.payload_len == 0 {
        (Vec::new(), true)
    } else {
        scan_data_index(path, header.version, header.payload_len)?
    };
    invalidate_data_reader(path);
    lock_storage_mutex(data_cache(), "data")?.insert(
        path.to_path_buf(),
        CachedDataIndex {
            stamp,
            sequence: header.sequence,
            blobs,
            is_log_format,
        },
    );
    Ok(())
}

fn scan_data_index(
    path: &Path,
    header_version: u16,
    payload_len: u64,
) -> Result<(Vec<DataBlobMeta>, bool), StorageError> {
    let mut file = File::open(path)?;
    let payload_offset = if header_version == STORAGE_LAYOUT_VERSION {
        super::segment::SEGMENT_HEADER_V2_TOTAL_SIZE as u64
    } else {
        super::segment::SEGMENT_HEADER_SIZE as u64
    };
    file.seek(SeekFrom::Start(payload_offset))?;
    let mut prefix = [0u8; 4];
    file.read_exact(&mut prefix)?;
    if prefix == *DATA_LOG_PREFIX {
        return Ok((
            scan_log_data_index(file, header_version, payload_offset, payload_len)?,
            true,
        ));
    }
    Ok((
        scan_legacy_data_index(file, header_version, payload_offset, payload_len)?,
        false,
    ))
}

fn scan_log_data_index(
    mut file: File,
    header_version: u16,
    payload_offset: u64,
    payload_len: u64,
) -> Result<Vec<DataBlobMeta>, StorageError> {
    let mut offset = payload_offset + DATA_LOG_PREFIX.len() as u64;
    let payload_end = payload_offset + payload_len;
    let mut blobs = Vec::new();
    while offset < payload_end {
        if offset
            .checked_add(DATA_BLOB_HEADER_SIZE as u64)
            .is_none_or(|header_end| header_end > payload_end)
        {
            return Err(StorageError::Corrupt(
                "data blob header exceeds segment payload".to_string(),
            ));
        }
        let mut header_buf = [0u8; DATA_BLOB_HEADER_SIZE];
        file.seek(SeekFrom::Start(offset))?;
        file.read_exact(&mut header_buf)?;
        let meta = decode_blob_meta_from_header(&header_buf)?;
        let next_offset = checked_blob_next_offset(offset, meta.stored_len, payload_end)?;
        blobs.push(DataBlobMeta {
            blob_id: meta.blob_id,
            codec: meta.codec,
            logical_len: meta.logical_len,
            stored_len: meta.stored_len,
            payload_checksum: meta.payload_checksum,
            payload_offset: offset + DATA_BLOB_HEADER_SIZE as u64,
            v2_integrity: header_version == STORAGE_LAYOUT_VERSION,
        });
        offset = next_offset;
    }
    Ok(blobs)
}

fn scan_legacy_data_index(
    mut file: File,
    header_version: u16,
    payload_offset: u64,
    payload_len: u64,
) -> Result<Vec<DataBlobMeta>, StorageError> {
    if payload_len < 8 {
        return Err(StorageError::Corrupt("data payload too short".to_string()));
    }
    file.seek(SeekFrom::Start(payload_offset))?;
    let mut count_buf = [0u8; 8];
    file.read_exact(&mut count_buf)?;
    let count_u64 = u64::from_le_bytes(count_buf);
    let max_possible_count = (payload_len - 8) / DATA_BLOB_HEADER_SIZE as u64;
    if count_u64 > max_possible_count {
        return Err(StorageError::Corrupt(
            "legacy data blob count exceeds segment payload".to_string(),
        ));
    }
    let count = usize::try_from(count_u64).map_err(|_| {
        StorageError::Corrupt("legacy data blob count exceeds platform limits".to_string())
    })?;
    let mut offset = payload_offset + 8;
    let payload_end = payload_offset + payload_len;
    let mut blobs = Vec::with_capacity(count);
    for _ in 0..count {
        let mut header_buf = [0u8; DATA_BLOB_HEADER_SIZE];
        file.seek(SeekFrom::Start(offset))?;
        file.read_exact(&mut header_buf)?;
        let meta = decode_blob_meta_from_header(&header_buf)?;
        let next_offset = checked_blob_next_offset(offset, meta.stored_len, payload_end)?;
        blobs.push(DataBlobMeta {
            blob_id: meta.blob_id,
            codec: meta.codec,
            logical_len: meta.logical_len,
            stored_len: meta.stored_len,
            payload_checksum: meta.payload_checksum,
            payload_offset: offset + DATA_BLOB_HEADER_SIZE as u64,
            v2_integrity: header_version == STORAGE_LAYOUT_VERSION,
        });
        offset = next_offset;
    }
    Ok(blobs)
}

fn checked_blob_next_offset(
    offset: u64,
    stored_len: u32,
    payload_end: u64,
) -> Result<u64, StorageError> {
    let blob_len = (DATA_BLOB_HEADER_SIZE as u64)
        .checked_add(stored_len as u64)
        .ok_or_else(|| StorageError::Corrupt("data blob length overflow".to_string()))?;
    let next_offset = offset
        .checked_add(blob_len)
        .ok_or_else(|| StorageError::Corrupt("data blob offset overflow".to_string()))?;
    if next_offset > payload_end {
        return Err(StorageError::Corrupt(
            "data blob exceeds segment payload".to_string(),
        ));
    }
    Ok(next_offset)
}

fn decode_blob_meta_from_header(
    buf: &[u8; DATA_BLOB_HEADER_SIZE],
) -> Result<DataBlobMeta, StorageError> {
    let blob_id = u64::from_le_bytes(
        buf[0..8]
            .try_into()
            .map_err(|_| StorageError::Corrupt("data blob parse failed".to_string()))?,
    );
    let codec = CompressionCodec::from_u8(buf[8])?;
    let logical_len = u32::from_le_bytes(
        buf[12..16]
            .try_into()
            .map_err(|_| StorageError::Corrupt("data blob parse failed".to_string()))?,
    );
    let stored_len = u32::from_le_bytes(
        buf[16..20]
            .try_into()
            .map_err(|_| StorageError::Corrupt("data blob parse failed".to_string()))?,
    );
    let payload_checksum = u32::from_le_bytes(
        buf[20..24]
            .try_into()
            .map_err(|_| StorageError::Corrupt("data blob parse failed".to_string()))?,
    );
    Ok(DataBlobMeta {
        blob_id,
        codec,
        logical_len,
        stored_len,
        payload_checksum,
        payload_offset: 0,
        v2_integrity: false,
    })
}

fn persist_data_blobs_as_log(path: &Path, blobs: &[DataBlobMeta]) -> Result<(), StorageError> {
    let mut payload = Vec::new();
    payload.extend_from_slice(DATA_LOG_PREFIX);
    let mut file = File::open(path)?;
    for blob in blobs {
        let stored_len =
            checked_usize_from_u64(u64::from(blob.stored_len), "data blob stored length")?;
        let mut bytes = vec![0u8; stored_len];
        file.seek(SeekFrom::Start(blob.payload_offset))?;
        file.read_exact(&mut bytes)?;
        let payload_checksum = blob_integrity_checksum(blob.blob_id, blob.logical_len, &bytes);
        payload.extend_from_slice(
            &DataBlob {
                blob_id: blob.blob_id,
                codec: blob.codec,
                logical_len: blob.logical_len,
                stored_len: blob.stored_len,
                payload_checksum,
                bytes,
            }
            .encode(),
        );
    }
    write_segment_file(path, SegmentKind::Data, 1, blobs.len() as u64, &payload)?;
    invalidate_data_reader(path);
    update_data_cache(path, blobs.len() as u64, blobs.to_vec())
}

fn update_data_cache(
    path: &Path,
    sequence: u64,
    blobs: Vec<DataBlobMeta>,
) -> Result<(), StorageError> {
    let stamp = file_stamp(path)?;
    lock_storage_mutex(data_cache(), "data")?.insert(
        path.to_path_buf(),
        CachedDataIndex {
            stamp,
            sequence,
            blobs,
            is_log_format: true,
        },
    );
    Ok(())
}

pub(crate) fn sync_layout_segments(paths: &LayoutPaths) -> Result<(), StorageError> {
    sync_indexed_segments(paths)?;
    flush_segment_index_for_append(&paths.segment_index_file)?;
    sync_segment_file(&paths.segment_index_file)?;
    sync_blk_map(&paths.blk_map_file)?;
    sync_lookup(&paths.lookup_file)?;
    sync_dedup(&paths.dedup_file)?;
    Ok(())
}

fn sync_every_writes() -> u32 {
    std::env::var("HOLO_STORAGE_SYNC_EVERY_WRITES")
        .ok()
        .and_then(|raw| raw.parse::<u32>().ok())
        .filter(|value| *value > 0)
        .unwrap_or(DEFAULT_SYNC_EVERY_WRITES)
}

fn record_pending_write(path: &Path) -> Result<u32, StorageError> {
    let mut guard = lock_storage_mutex(pending_sync_writes(), "pending write")?;
    let next = guard.get(path).copied().unwrap_or(0).saturating_add(1);
    guard.insert(path.to_path_buf(), next);
    Ok(next)
}

fn pending_count(path: &Path) -> Result<u32, StorageError> {
    Ok(lock_storage_mutex(pending_sync_writes(), "pending write")?
        .get(path)
        .copied()
        .unwrap_or(0))
}

fn clear_pending_writes(path: &Path) -> Result<(), StorageError> {
    lock_storage_mutex(pending_sync_writes(), "pending write")?.remove(path);
    Ok(())
}

pub fn current_dedup_refcounts(paths: &LayoutPaths) -> Result<Vec<(u64, u32)>, StorageError> {
    let (_seq, entries) = load_dedup_index(&paths.dedup_file)?;
    Ok(entries
        .into_iter()
        .map(|entry| (entry.entry_id, entry.ref_count))
        .collect())
}

pub fn current_checkpoint(paths: &LayoutPaths) -> Result<MetadataCheckpoint, StorageError> {
    load_checkpoint_cached(&paths.metadata_file)
}

fn checked_u32_len(len: usize, what: &str) -> Result<u32, StorageError> {
    u32::try_from(len).map_err(|_| StorageError::Conflict(format!("{what} exceeds u32::MAX")))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    static ENV_LOCK: Mutex<()> = Mutex::new(());

    fn temp_segment_path(name: &str) -> PathBuf {
        let mut dir = std::env::temp_dir();
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .expect("system time should be after epoch")
            .as_nanos();
        dir.push(format!("holo-data-path-{name}-{nanos}"));
        fs::create_dir_all(&dir).expect("create temp segment dir");
        dir.push("segment.dat");
        dir
    }

    #[test]
    fn checked_u32_len_allows_u32_max() {
        let value =
            checked_u32_len(u32::MAX as usize, "test length").expect("must accept u32::MAX");
        assert_eq!(value, u32::MAX);
    }

    #[test]
    fn checked_u32_len_rejects_overflow() {
        let err = checked_u32_len(usize::MAX, "test length").expect_err("usize::MAX must overflow");
        let msg = format!("{err}");
        assert!(msg.contains("exceeds u32::MAX"));
    }

    #[test]
    fn scan_legacy_data_index_rejects_unbounded_count() {
        let path = temp_segment_path("legacy-count");
        let payload = u64::MAX.to_le_bytes().to_vec();
        write_segment_file(&path, SegmentKind::Data, 1, 1, &payload)
            .expect("write corrupt legacy segment");

        let header = read_segment_header(&path, SegmentKind::Data).expect("read header");
        let err = scan_data_index(&path, header.version, payload.len() as u64)
            .expect_err("unbounded legacy count must be rejected");
        let msg = format!("{err}");
        assert!(msg.contains("legacy data blob count exceeds segment payload"));

        let _ = fs::remove_file(&path);
        let _ = fs::remove_dir(path.parent().expect("temp path has parent"));
    }

    #[test]
    fn scan_log_data_index_rejects_blob_beyond_payload() {
        let path = temp_segment_path("log-stored-len");
        let mut payload = Vec::new();
        payload.extend_from_slice(DATA_LOG_PREFIX);
        payload.extend_from_slice(&encode_data_blob_header(
            7,
            CompressionCodec::None,
            4,
            1024,
            0,
        ));
        write_segment_file(&path, SegmentKind::Data, 1, 1, &payload)
            .expect("write corrupt log segment");

        let header = read_segment_header(&path, SegmentKind::Data).expect("read header");
        let err = scan_data_index(&path, header.version, payload.len() as u64)
            .expect_err("oversized stored_len must be rejected");
        let msg = format!("{err}");
        assert!(msg.contains("data blob exceeds segment payload"));

        let _ = fs::remove_file(&path);
        let _ = fs::remove_dir(path.parent().expect("temp path has parent"));
    }

    fn test_dedup_entry(identity_version: u8) -> DedupIndexEntry {
        DedupIndexEntry {
            entry_id: 1,
            fingerprint_hi: 1,
            fingerprint_lo: 2,
            payload_checksum: 3,
            logical_len: 4,
            stored_blob_id: 5,
            stored_len: 6,
            compression: CompressionCodec::None,
            identity_version,
            ref_count: 1,
        }
    }

    #[test]
    fn dedup_hit_verification_defaults_on_for_blake3_identity() {
        let _guard = ENV_LOCK.lock().expect("env lock");
        std::env::remove_var("HOLO_TAPE_DEDUP_VERIFY_HITS");

        assert!(should_verify_dedup_hit(&test_dedup_entry(
            DEDUP_IDENTITY_BLAKE3_128
        )));
    }

    #[test]
    fn dedup_hit_verification_can_be_explicitly_disabled_for_perf() {
        let _guard = ENV_LOCK.lock().expect("env lock");
        std::env::set_var("HOLO_TAPE_DEDUP_VERIFY_HITS", "0");

        assert!(!should_verify_dedup_hit(&test_dedup_entry(
            DEDUP_IDENTITY_BLAKE3_128
        )));

        std::env::remove_var("HOLO_TAPE_DEDUP_VERIFY_HITS");
    }

    #[test]
    fn legacy_dedup_identity_always_verifies_even_when_opted_out() {
        let _guard = ENV_LOCK.lock().expect("env lock");
        std::env::set_var("HOLO_TAPE_DEDUP_VERIFY_HITS", "false");

        assert!(should_verify_dedup_hit(&test_dedup_entry(0)));

        std::env::remove_var("HOLO_TAPE_DEDUP_VERIFY_HITS");
    }
}
