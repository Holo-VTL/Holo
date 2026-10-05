use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::collections::{HashMap, HashSet};
use std::fs::{self, File};
use std::io::{Read, Seek, SeekFrom, Write};
use std::os::unix::fs::MetadataExt;
use std::path::Path;

use super::blk_map::{load_blk_map_records, persist_blk_map_records, BlkMapRecord, BlkMapState};
use super::data_path::{
    discard_layout_caches_checked, flush_pending_writes, invalidate_data_segment_cache,
    DATA_LOG_PREFIX,
};
use super::dedup::{load_dedup_index, persist_dedup_entries};
use super::layout::{LayoutPaths, SegmentKind};
use super::layout_lease::LayoutLease;
use super::maintenance_progress::{no_maintenance_progress, MaintenancePhase, MaintenanceProgress};
use super::map_lookup::rebuild_lookup_from_blk_map;
use super::metadata::StorageError;
use super::runtime_state::load_retention_state;
use super::segment::{read_segment_header, segment_payload_offset, write_segment_file_streaming};
use super::segment_index::{
    data_segment_path, invalidate_segment_index_cache_checked, load_segment_index,
    persist_segment_index, SegmentDescriptor, SegmentState,
};
use super::space_guard::{
    ensure_capacity, estimate_write_peak_bytes, FilesystemLock, FilesystemReservation,
    SPACE_RESERVE_BYTES,
};

const RECLAIM_MIN_DEAD_PERCENT: u128 = 25;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct OfflineReclaimReport {
    pub processed_segments: u32,
    pub has_more: bool,
    pub scan_cursor: Option<String>,
    pub allocated_before_bytes: u64,
    pub allocated_after_bytes: u64,
    pub net_freed_bytes: u64,
}

#[derive(Debug, Clone)]
struct SegmentCandidate {
    descriptor: SegmentDescriptor,
    live_blob_ids: HashSet<u64>,
    live_bytes: u64,
    last_blob_id: u64,
    source_sequence: u64,
    source_file_len: u64,
}

#[derive(Debug)]
struct CandidateScan {
    candidate: Option<SegmentCandidate>,
    has_more_candidates: bool,
    scan_cursor: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct MaintenanceScanCursor {
    schema_version: u8,
    snapshot: String,
    #[serde(rename = "segment_ordinal")]
    descriptor_ordinal: u64,
    in_segment: bool,
    segment_seq: u32,
    segment_sequence: u64,
    segment_checksum: u32,
    segment_file_len: u64,
    byte_offset: u64,
    #[serde(rename = "verified_headers")]
    segment_headers_scanned: u64,
    segment_source_bytes: u64,
    #[serde(rename = "active_bytes")]
    segment_live_bytes: u64,
    segment_last_blob_id: u64,
    best_candidate: Option<MaintenanceCandidateSummary>,
    eligible_candidate_count: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct MaintenanceCandidateSummary {
    segment_seq: u32,
    source_bytes: u64,
    live_bytes: u64,
    last_blob_id: u64,
    source_sequence: u64,
    source_file_len: u64,
}

pub fn reclaim_one_segment(
    paths: &LayoutPaths,
    pool_root: &Path,
    library_id: &str,
    cartridge_id: &str,
    runtime_dir: &Path,
) -> Result<OfflineReclaimReport, StorageError> {
    reclaim_one_segment_with_cursor(
        paths,
        pool_root,
        library_id,
        cartridge_id,
        runtime_dir,
        None,
    )
}

pub fn reclaim_one_segment_with_cursor(
    paths: &LayoutPaths,
    pool_root: &Path,
    library_id: &str,
    cartridge_id: &str,
    runtime_dir: &Path,
    scan_cursor: Option<&str>,
) -> Result<OfflineReclaimReport, StorageError> {
    let mut progress = no_maintenance_progress;
    reclaim_one_segment_with_cursor_and_progress(
        paths,
        pool_root,
        library_id,
        cartridge_id,
        runtime_dir,
        scan_cursor,
        &mut progress,
    )
}

pub fn reclaim_one_segment_with_cursor_and_progress(
    paths: &LayoutPaths,
    pool_root: &Path,
    library_id: &str,
    cartridge_id: &str,
    runtime_dir: &Path,
    scan_cursor: Option<&str>,
    progress: &mut MaintenanceProgress<'_>,
) -> Result<OfflineReclaimReport, StorageError> {
    let media_state_dir = std::env::var_os("HOLO_MEDIA_STATE_DIR")
        .map(std::path::PathBuf::from)
        .unwrap_or_else(|| std::path::PathBuf::from("/run/holo/media-state"));
    reclaim_one_segment_with_media_state_dir_and_progress(
        paths,
        pool_root,
        library_id,
        cartridge_id,
        ReclaimRuntimeDirs {
            runtime_dir,
            media_state_dir: &media_state_dir,
        },
        scan_cursor,
        progress,
    )
}

#[cfg(test)]
pub(super) fn reclaim_one_segment_with_media_state_dir(
    paths: &LayoutPaths,
    pool_root: &Path,
    library_id: &str,
    cartridge_id: &str,
    runtime_dir: &Path,
    media_state_dir: &Path,
    scan_cursor: Option<&str>,
) -> Result<OfflineReclaimReport, StorageError> {
    let mut progress = no_maintenance_progress;
    reclaim_one_segment_with_media_state_dir_and_progress(
        paths,
        pool_root,
        library_id,
        cartridge_id,
        ReclaimRuntimeDirs {
            runtime_dir,
            media_state_dir,
        },
        scan_cursor,
        &mut progress,
    )
}

pub(super) struct ReclaimRuntimeDirs<'a> {
    runtime_dir: &'a Path,
    media_state_dir: &'a Path,
}

pub(super) fn reclaim_one_segment_with_media_state_dir_and_progress(
    paths: &LayoutPaths,
    pool_root: &Path,
    library_id: &str,
    cartridge_id: &str,
    dirs: ReclaimRuntimeDirs<'_>,
    scan_cursor: Option<&str>,
    progress: &mut MaintenanceProgress<'_>,
) -> Result<OfflineReclaimReport, StorageError> {
    let layout_lease = LayoutLease::acquire(pool_root, cartridge_id, dirs.runtime_dir)
        .map_err(super::layout_lease::storage_error_from_lease)?;
    layout_lease
        .verify_pool_root(pool_root)
        .map_err(super::layout_lease::storage_error_from_lease)?;
    if shared_state_reports_loaded(dirs.media_state_dir, library_id, cartridge_id)? {
        return Err(StorageError::Conflict(
            "cartridge is busy because shared media state reports it loaded".to_string(),
        ));
    }
    layout_lease
        .verify_pool_root(pool_root)
        .map_err(super::layout_lease::storage_error_from_lease)?;
    reclaim_one_segment_under_lease(paths, pool_root, dirs.runtime_dir, scan_cursor, progress)
}

fn shared_state_reports_loaded(
    dir: &Path,
    library_id: &str,
    cartridge_id: &str,
) -> Result<bool, StorageError> {
    let entries = match fs::read_dir(dir) {
        Ok(entries) => entries,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(err) => return Err(StorageError::Io(err)),
    };
    let prefix = format!("{}__", super::layout::sanitize_id(library_id));
    for entry in entries {
        let entry = entry?;
        let name = entry.file_name();
        let name = name.to_string_lossy();
        if !name.starts_with(&prefix) || !name.ends_with(".state") {
            continue;
        }
        let raw = match fs::read_to_string(entry.path()) {
            Ok(raw) => raw,
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => continue,
            Err(err) => return Err(StorageError::Io(err)),
        };
        let trimmed = raw.trim();
        if trimmed.is_empty() {
            continue;
        }
        if trimmed.contains(['\r', '\n']) {
            return Err(StorageError::Corrupt(
                "shared media state is malformed".to_string(),
            ));
        }
        let loaded = trimmed.strip_prefix("cartridge=").unwrap_or(trimmed).trim();
        if loaded.contains(['=', '/', '\\', '\0']) || loaded.contains("..") || loaded.len() > 128 {
            return Err(StorageError::Corrupt(
                "shared media state is malformed".to_string(),
            ));
        }
        if loaded == cartridge_id {
            return Ok(true);
        }
    }
    Ok(false)
}

fn reclaim_one_segment_under_lease(
    paths: &LayoutPaths,
    pool_root: &Path,
    runtime_dir: &Path,
    scan_cursor: Option<&str>,
    progress: &mut MaintenanceProgress<'_>,
) -> Result<OfflineReclaimReport, StorageError> {
    let retention = load_retention_state(&paths.root)?;
    if retention.is_some_and(|state| state.is_worm_media) {
        return Err(StorageError::Conflict(
            "cartridge is busy because WORM media cannot be reclaimed".to_string(),
        ));
    }
    if retention.is_some_and(|state| state.retention_locked) {
        return Err(StorageError::Conflict(
            "cartridge retention is locked".to_string(),
        ));
    }
    preflight_layout_metadata(paths)?;
    let metadata_bytes = metadata_temporary_bytes(paths)?;
    let _metadata_reservation =
        reserve_capacity(pool_root, runtime_dir, metadata_bytes, 1, 8, metadata_bytes)?;
    let _ = super::data_path::recover_dirty_state_with_progress(paths, progress)?;
    drop(_metadata_reservation);
    let mut index = load_segment_index(&paths.segment_index_file)?;
    let (_map_sequence, map_records) = load_blk_map_records(&paths.blk_map_file)?;
    let scan = collect_candidates(paths, &index, &map_records, scan_cursor, progress)?;
    if let Some(next_cursor) = scan.scan_cursor {
        return Ok(OfflineReclaimReport {
            processed_segments: 0,
            has_more: true,
            scan_cursor: Some(next_cursor),
            allocated_before_bytes: allocated_layout_bytes(&paths.root)?,
            allocated_after_bytes: allocated_layout_bytes(&paths.root)?,
            net_freed_bytes: 0,
        });
    }
    let Some(candidate) = scan.candidate else {
        let allocated = allocated_layout_bytes(&paths.root)?;
        return Ok(OfflineReclaimReport {
            processed_segments: 0,
            has_more: false,
            scan_cursor: None,
            allocated_before_bytes: allocated,
            allocated_after_bytes: allocated,
            net_freed_bytes: 0,
        });
    };

    let allocated_before = allocated_layout_bytes(&paths.root)?;
    let live_count = u64::try_from(candidate.live_blob_ids.len())
        .map_err(|_| StorageError::Corrupt("live blob count overflow".to_string()))?;
    let reservation = reserve_capacity(
        pool_root,
        runtime_dir,
        candidate.live_bytes,
        live_count,
        1,
        metadata_bytes,
    )?;

    progress(MaintenancePhase::Commit, 0, 0)?;
    super::data_path::mark_checkpoint_dirty(paths)?;
    let pruned = prune_metadata(paths, map_records)?;
    if candidate.live_blob_ids.is_empty() && candidate.descriptor.state != SegmentState::Active {
        if let Some(active) = index
            .descriptors
            .iter_mut()
            .find(|descriptor| descriptor.state == SegmentState::Active)
        {
            active.last_blob_id = active.last_blob_id.max(candidate.last_blob_id);
        }
        index
            .descriptors
            .retain(|desc| desc.segment_seq != candidate.descriptor.segment_seq);
        persist_segment_index(&paths.segment_index_file, &index)?;
        let source = reclaim_segment_path(paths, candidate.descriptor.segment_seq);
        if source.exists() {
            invalidate_data_segment_cache(&source);
            fs::remove_file(source)?;
            sync_directory(&paths.root)?;
        }
    } else {
        rewrite_segment_in_place(paths, &candidate, &mut index, progress)?;
    }

    preserve_blob_id_high_water(&mut index, &pruned);
    persist_segment_index(&paths.segment_index_file, &index)?;
    let _ = rebuild_lookup_from_blk_map(&paths.blk_map_file, &paths.lookup_file)?;
    rebuild_dedup_refcounts(paths, &pruned)?;
    discard_layout_caches_checked(paths)?;
    flush_pending_writes(paths)?;

    let allocated_after = allocated_layout_bytes(&paths.root)?;
    drop(reservation);
    Ok(OfflineReclaimReport {
        processed_segments: 1,
        has_more: scan.has_more_candidates,
        scan_cursor: None,
        allocated_before_bytes: allocated_before,
        allocated_after_bytes: allocated_after,
        net_freed_bytes: allocated_before.saturating_sub(allocated_after),
    })
}

fn collect_candidates(
    paths: &LayoutPaths,
    index: &super::segment_index::SegmentIndex,
    records: &[BlkMapRecord],
    raw_cursor: Option<&str>,
    progress: &mut MaintenanceProgress<'_>,
) -> Result<CandidateScan, StorageError> {
    const MAX_HEADERS_PER_ROUND: usize = 65_536;
    const MAX_DESCRIPTORS_PER_ROUND: usize = 256;
    const MAX_CURSOR_BYTES: usize = 4 * 1024;

    let snapshot = maintenance_snapshot(paths)?;
    let mut cursor = match raw_cursor {
        Some(raw) if raw.len() <= MAX_CURSOR_BYTES => {
            let parsed = serde_json::from_str::<MaintenanceScanCursor>(raw).ok();
            parsed.filter(|cursor| {
                cursor.schema_version == 2
                    && cursor.snapshot == snapshot
                    && cursor.descriptor_ordinal <= index.descriptors.len() as u64
                    && (!cursor.in_segment
                        || index
                            .descriptors
                            .get(cursor.descriptor_ordinal as usize)
                            .is_some_and(|descriptor| descriptor.segment_seq == cursor.segment_seq))
            })
        }
        Some(_) => {
            return Err(StorageError::Conflict(
                "maintenance scan cursor exceeds its limit".to_string(),
            ));
        }
        None => None,
    }
    .unwrap_or_else(|| MaintenanceScanCursor {
        schema_version: 2,
        snapshot: snapshot.clone(),
        descriptor_ordinal: 0,
        in_segment: false,
        segment_seq: 0,
        segment_sequence: 0,
        segment_checksum: 0,
        segment_file_len: 0,
        byte_offset: 0,
        segment_headers_scanned: 0,
        segment_source_bytes: 0,
        segment_live_bytes: 0,
        segment_last_blob_id: 0,
        best_candidate: None,
        eligible_candidate_count: 0,
    });

    let mut headers_this_round = 0usize;
    let mut descriptors_this_round = 0usize;
    let mut restart_cursor = false;
    while cursor.descriptor_ordinal < index.descriptors.len() as u64
        && headers_this_round < MAX_HEADERS_PER_ROUND
        && descriptors_this_round < MAX_DESCRIPTORS_PER_ROUND
    {
        let ordinal = usize::try_from(cursor.descriptor_ordinal)
            .map_err(|_| StorageError::MetadataBudgetExceeded)?;
        let descriptor = &index.descriptors[ordinal];
        let path = reclaim_segment_path(paths, descriptor.segment_seq);
        descriptors_this_round += 1;
        if descriptor.payload_bytes == 0 {
            cursor.descriptor_ordinal += 1;
            cursor.in_segment = false;
            cursor.byte_offset = 0;
            cursor.segment_headers_scanned = 0;
            cursor.segment_source_bytes = 0;
            cursor.segment_live_bytes = 0;
            cursor.segment_last_blob_id = 0;
            continue;
        }

        let live_blob_ids = active_blob_ids_for_segment(index, records, descriptor.segment_seq);
        let start_offset = cursor.in_segment.then_some(cursor.byte_offset);
        let scan = super::data_path::scan_segment_blob_headers_with_progress(
            &path,
            start_offset,
            cursor.segment_headers_scanned,
            MAX_HEADERS_PER_ROUND - headers_this_round,
            progress,
        )?;
        if cursor.in_segment
            && (scan.segment_header.sequence != cursor.segment_sequence
                || scan.segment_header.checksum != cursor.segment_checksum
                || fs::metadata(&path)?.len() != cursor.segment_file_len)
        {
            restart_cursor = true;
            break;
        }
        headers_this_round = headers_this_round.saturating_add(scan.headers_scanned as usize);
        let mut source_bytes = cursor.segment_source_bytes;
        let mut live_bytes = cursor.segment_live_bytes;
        let mut last_blob_id = cursor.segment_last_blob_id;
        for blob in &scan.blobs {
            if last_blob_id > 0 && blob.blob_id <= last_blob_id {
                return Err(StorageError::Corrupt(
                    "data blob IDs are duplicated or out of order".to_string(),
                ));
            }
            let record_bytes = 24u64
                .checked_add(u64::from(blob.stored_len))
                .ok_or_else(|| StorageError::Corrupt("blob record size overflow".to_string()))?;
            source_bytes = source_bytes
                .checked_add(record_bytes)
                .ok_or_else(|| StorageError::Corrupt("segment source size overflow".to_string()))?;
            if live_blob_ids.contains(&blob.blob_id) {
                live_bytes = live_bytes.checked_add(record_bytes).ok_or_else(|| {
                    StorageError::Corrupt("segment live size overflow".to_string())
                })?;
            }
            last_blob_id = blob.blob_id;
        }

        if scan.complete {
            let dead_bytes = source_bytes.saturating_sub(live_bytes);
            let eligible = live_bytes == 0
                || u128::from(dead_bytes) * 100
                    >= u128::from(source_bytes) * RECLAIM_MIN_DEAD_PERCENT;
            if eligible && dead_bytes > 0 {
                let summary = MaintenanceCandidateSummary {
                    segment_seq: descriptor.segment_seq,
                    source_bytes,
                    live_bytes,
                    last_blob_id,
                    source_sequence: scan.segment_header.sequence,
                    source_file_len: fs::metadata(&path)?.len(),
                };
                cursor.eligible_candidate_count = cursor.eligible_candidate_count.saturating_add(1);
                if cursor
                    .best_candidate
                    .as_ref()
                    .is_none_or(|previous| candidate_summary_precedes(&summary, previous))
                {
                    cursor.best_candidate = Some(summary);
                }
            }
            cursor.descriptor_ordinal += 1;
            cursor.in_segment = false;
            cursor.segment_seq = 0;
            cursor.segment_sequence = 0;
            cursor.segment_checksum = 0;
            cursor.segment_file_len = 0;
            cursor.byte_offset = 0;
            cursor.segment_headers_scanned = 0;
            cursor.segment_source_bytes = 0;
            cursor.segment_live_bytes = 0;
            cursor.segment_last_blob_id = 0;
        } else {
            cursor.in_segment = true;
            cursor.segment_seq = descriptor.segment_seq;
            cursor.segment_sequence = scan.segment_header.sequence;
            cursor.segment_checksum = scan.segment_header.checksum;
            cursor.segment_file_len = fs::metadata(&path)?.len();
            cursor.byte_offset = scan.next_offset;
            cursor.segment_headers_scanned = cursor
                .segment_headers_scanned
                .saturating_add(scan.headers_scanned);
            cursor.segment_source_bytes = source_bytes;
            cursor.segment_live_bytes = live_bytes;
            cursor.segment_last_blob_id = last_blob_id;
            break;
        }
    }

    if restart_cursor {
        return collect_candidates(paths, index, records, None, progress);
    }
    if cursor.descriptor_ordinal < index.descriptors.len() as u64 {
        let encoded = serde_json::to_vec(&cursor)
            .map_err(|_| StorageError::Internal("scan cursor serialization failed".to_string()))?;
        if encoded.len() > MAX_CURSOR_BYTES {
            return Err(StorageError::MetadataBudgetExceeded);
        }
        return Ok(CandidateScan {
            candidate: None,
            has_more_candidates: true,
            scan_cursor: Some(
                String::from_utf8(encoded).map_err(|_| {
                    StorageError::Internal("scan cursor encoding failed".to_string())
                })?,
            ),
        });
    }

    let candidate = cursor.best_candidate.and_then(|summary| {
        let descriptor = index
            .descriptors
            .iter()
            .find(|descriptor| descriptor.segment_seq == summary.segment_seq)?;
        Some(SegmentCandidate {
            descriptor: descriptor.clone(),
            live_blob_ids: active_blob_ids_for_segment(index, records, summary.segment_seq),
            live_bytes: summary.live_bytes,
            last_blob_id: summary.last_blob_id,
            source_sequence: summary.source_sequence,
            source_file_len: summary.source_file_len,
        })
    });
    Ok(CandidateScan {
        candidate,
        has_more_candidates: cursor.eligible_candidate_count > 1,
        scan_cursor: None,
    })
}

fn active_blob_ids_for_segment(
    index: &super::segment_index::SegmentIndex,
    records: &[BlkMapRecord],
    segment_seq: u32,
) -> HashSet<u64> {
    let legacy_zero_alias = segment_seq == 0
        && index
            .descriptors
            .iter()
            .all(|descriptor| descriptor.segment_seq != 1);
    records
        .iter()
        .filter(|record| record.state == BlkMapState::Active)
        .filter(|record| {
            record.physical_segment_id == u64::from(segment_seq)
                || (legacy_zero_alias && record.physical_segment_id == 1)
        })
        .map(|record| record.physical_offset)
        .collect()
}

fn reclaim_segment_path(paths: &LayoutPaths, segment_seq: u32) -> std::path::PathBuf {
    let indexed = data_segment_path(paths, segment_seq);
    if segment_seq == 0 && !indexed.exists() && paths.data_file.exists() {
        paths.data_file.clone()
    } else {
        indexed
    }
}

fn candidate_summary_precedes(
    left: &MaintenanceCandidateSummary,
    right: &MaintenanceCandidateSummary,
) -> bool {
    let left_fully_dead = left.live_bytes == 0;
    let right_fully_dead = right.live_bytes == 0;
    match (left_fully_dead, right_fully_dead) {
        (true, false) => return true,
        (false, true) => return false,
        _ => {}
    }
    left.source_bytes
        .saturating_sub(left.live_bytes)
        .cmp(&right.source_bytes.saturating_sub(right.live_bytes))
        .then_with(|| right.segment_seq.cmp(&left.segment_seq))
        .is_gt()
}

fn maintenance_snapshot(paths: &LayoutPaths) -> Result<String, StorageError> {
    let root_metadata = fs::metadata(&paths.root)?;
    let mut digest = Sha256::new();
    digest.update(root_metadata.dev().to_le_bytes());
    digest.update(root_metadata.ino().to_le_bytes());
    let filemarks = paths.root.join("filemarks.state");
    let usage = paths.root.join("usage.counters");
    let files = [
        (b"checkpoint".as_slice(), &paths.metadata_file),
        (b"blk-map".as_slice(), &paths.blk_map_file),
        (b"lookup".as_slice(), &paths.lookup_file),
        (b"reclaim".as_slice(), &paths.reclaim_file),
        (b"dedup".as_slice(), &paths.dedup_file),
        (b"segment-index".as_slice(), &paths.segment_index_file),
        (b"filemarks".as_slice(), &filemarks),
        (b"usage".as_slice(), &usage),
    ];
    let mut total = 0u64;
    for (label, path) in files {
        digest.update(label);
        let before = match fs::symlink_metadata(path) {
            Ok(metadata) => metadata,
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
                digest.update([0]);
                continue;
            }
            Err(err) => return Err(StorageError::Io(err)),
        };
        if before.file_type().is_symlink() || !before.is_file() {
            return Err(StorageError::Conflict(
                "unsafe maintenance metadata file".to_string(),
            ));
        }
        total = total
            .checked_add(before.len())
            .ok_or(StorageError::MetadataBudgetExceeded)?;
        if total > super::metadata::MAX_MAINTENANCE_METADATA_BYTES {
            return Err(StorageError::MetadataBudgetExceeded);
        }
        let mut file = File::open(path)?;
        let opened = file.metadata()?;
        if opened.dev() != before.dev() || opened.ino() != before.ino() {
            return Err(StorageError::Conflict(
                "maintenance metadata identity changed while opening".to_string(),
            ));
        }
        digest.update([1]);
        digest.update(before.len().to_le_bytes());
        digest.update(before.dev().to_le_bytes());
        digest.update(before.ino().to_le_bytes());
        let mut remaining = before.len();
        let mut buffer = vec![0u8; 64 * 1024];
        while remaining > 0 {
            let requested = usize::try_from(remaining.min(buffer.len() as u64))
                .map_err(|_| StorageError::MetadataBudgetExceeded)?;
            file.read_exact(&mut buffer[..requested])?;
            digest.update(&buffer[..requested]);
            remaining = remaining.saturating_sub(requested as u64);
        }
        let after = file.metadata()?;
        if after.len() != before.len() || after.dev() != before.dev() || after.ino() != before.ino()
        {
            return Err(StorageError::Conflict(
                "maintenance metadata changed during scan".to_string(),
            ));
        }
    }
    Ok(format!("{:x}", digest.finalize()))
}

fn rewrite_segment_in_place(
    paths: &LayoutPaths,
    candidate: &SegmentCandidate,
    index: &mut super::segment_index::SegmentIndex,
    progress: &mut MaintenanceProgress<'_>,
) -> Result<(), StorageError> {
    const COPY_YIELD_BYTES: u64 = 1024 * 1024;
    const COPY_YIELD_DURATION: std::time::Duration = std::time::Duration::from_millis(50);

    let source = reclaim_segment_path(paths, candidate.descriptor.segment_seq);
    let old_header = read_segment_header(&source, SegmentKind::Data)?;
    if old_header.sequence != candidate.source_sequence
        || fs::metadata(&source)?.len() != candidate.source_file_len
    {
        return Err(StorageError::Conflict(
            "data segment changed after reclaim scan".to_string(),
        ));
    }
    let stage = source.with_file_name(format!(
        "data_{:06}.reclaim-stage.seg",
        candidate.descriptor.segment_seq
    ));
    let next_sequence = old_header
        .sequence
        .checked_add(1)
        .ok_or_else(|| StorageError::Corrupt("segment sequence overflow".to_string()))?;
    let mut first_blob_id = 0u64;
    let mut last_blob_id = 0u64;
    let mut logical_live_bytes = 0u64;
    let staged_header = write_segment_file_streaming(
        &stage,
        SegmentKind::Data,
        u64::from(candidate.descriptor.segment_seq),
        next_sequence,
        |output| {
            let mut input = File::open(&source)?;
            let source_payload_offset = segment_payload_offset(&old_header);
            let mut source_fnv = 0x811C9DC5u32;
            let mut source_crc = 0u32;
            let mut legacy_count_prefix = [0u8; 8];
            let mut log_prefix = [0u8; 4];
            input.seek(SeekFrom::Start(source_payload_offset))?;
            input.read_exact(&mut log_prefix)?;
            let source_is_log = log_prefix == *DATA_LOG_PREFIX;
            let mut legacy_count = None;
            if source_is_log {
                source_fnv = super::segment::checksum32_continue(source_fnv, &log_prefix);
                source_crc = super::segment::integrity32_continue(source_crc, &log_prefix);
            } else {
                input.seek(SeekFrom::Start(source_payload_offset))?;
                input.read_exact(&mut legacy_count_prefix)?;
                source_fnv = super::segment::checksum32_continue(source_fnv, &legacy_count_prefix);
                source_crc = super::segment::integrity32_continue(source_crc, &legacy_count_prefix);
                legacy_count = Some(u64::from_le_bytes(legacy_count_prefix));
            }
            output.write_all(DATA_LOG_PREFIX)?;
            let mut buffer = vec![0u8; 1024 * 1024];
            let payload_end = source_payload_offset
                .checked_add(old_header.payload_len)
                .ok_or_else(|| StorageError::Corrupt("data segment length overflow".to_string()))?;
            let mut input_offset = if source_is_log {
                source_payload_offset + DATA_LOG_PREFIX.len() as u64
            } else {
                source_payload_offset + 8
            };
            let mut seen_headers = 0u64;
            let mut copied_since_yield = 0u64;
            let mut prior_blob_id = 0u64;
            while input_offset < payload_end
                && legacy_count.is_none_or(|count| seen_headers < count)
            {
                let mut encoded_header = [0u8; 24];
                input.read_exact(&mut encoded_header)?;
                source_fnv = super::segment::checksum32_continue(source_fnv, &encoded_header);
                source_crc = super::segment::integrity32_continue(source_crc, &encoded_header);
                let mut meta = super::data_path::decode_blob_meta_from_header(&encoded_header)?;
                if prior_blob_id > 0 && meta.blob_id <= prior_blob_id {
                    return Err(StorageError::Corrupt(
                        "data blob IDs are duplicated or out of order".to_string(),
                    ));
                }
                prior_blob_id = meta.blob_id;
                meta.payload_offset = input_offset + 24;
                meta.v2_integrity = old_header.version == 2;
                let next_offset = meta
                    .payload_offset
                    .checked_add(u64::from(meta.stored_len))
                    .ok_or_else(|| {
                        StorageError::Corrupt("data blob length overflow".to_string())
                    })?;
                if next_offset > payload_end {
                    return Err(StorageError::Corrupt(
                        "data blob exceeds segment payload".to_string(),
                    ));
                }
                let keep = candidate.live_blob_ids.contains(&meta.blob_id);
                let phase = if keep {
                    MaintenancePhase::Copy
                } else {
                    MaintenancePhase::Verify
                };
                let output_header_offset = if keep {
                    let offset = output.seek(SeekFrom::End(0))?;
                    output.write_all(&[0u8; 24])?;
                    Some(offset)
                } else {
                    None
                };
                let mut blob_crc =
                    super::segment::integrity32_continue(0, &meta.blob_id.to_le_bytes());
                blob_crc =
                    super::segment::integrity32_continue(blob_crc, &meta.logical_len.to_le_bytes());
                let mut remaining = u64::from(meta.stored_len);
                while remaining > 0 {
                    let requested = usize::try_from(remaining.min(buffer.len() as u64))
                        .map_err(|_| StorageError::Corrupt("blob length overflow".to_string()))?;
                    input.read_exact(&mut buffer[..requested])?;
                    source_fnv =
                        super::segment::checksum32_continue(source_fnv, &buffer[..requested]);
                    source_crc =
                        super::segment::integrity32_continue(source_crc, &buffer[..requested]);
                    if keep {
                        blob_crc =
                            super::segment::integrity32_continue(blob_crc, &buffer[..requested]);
                        output.write_all(&buffer[..requested])?;
                        copied_since_yield = copied_since_yield.saturating_add(requested as u64);
                    }
                    remaining = remaining.saturating_sub(requested as u64);
                    progress(phase, requested as u64, 0)?;
                    if copied_since_yield >= COPY_YIELD_BYTES {
                        // Leave the device a short window for foreground tape writes.
                        std::thread::sleep(COPY_YIELD_DURATION);
                        copied_since_yield = 0;
                    }
                }
                progress(phase, 0, 1)?;
                if keep {
                    if meta.v2_integrity && blob_crc != meta.payload_checksum {
                        return Err(StorageError::Corrupt(
                            "data blob integrity checksum mismatch".to_string(),
                        ));
                    }
                    let encoded_header = super::data_path::encode_data_blob_header(
                        meta.blob_id,
                        meta.codec,
                        meta.logical_len,
                        meta.stored_len,
                        blob_crc,
                    );
                    let end = output.seek(SeekFrom::End(0))?;
                    output.seek(SeekFrom::Start(output_header_offset.expect("kept header")))?;
                    output.write_all(&encoded_header)?;
                    output.seek(SeekFrom::Start(end))?;
                    if first_blob_id == 0 {
                        first_blob_id = meta.blob_id;
                    }
                    last_blob_id = last_blob_id.max(meta.blob_id);
                    logical_live_bytes = logical_live_bytes
                        .checked_add(u64::from(meta.logical_len))
                        .ok_or_else(|| {
                            StorageError::Corrupt("live byte count overflow".to_string())
                        })?;
                }
                input_offset = next_offset;
                seen_headers = seen_headers.saturating_add(1);
            }
            if input_offset != payload_end
                || legacy_count.is_some_and(|count| count != seen_headers)
            {
                return Err(StorageError::Corrupt(
                    "data segment blob count or length mismatch".to_string(),
                ));
            }
            let expected_checksum = old_header.checksum;
            let source_checksum_ok = match old_header.version {
                _ if expected_checksum == 0 => true,
                1 => source_fnv == expected_checksum,
                _ => source_crc == expected_checksum,
            };
            if !source_checksum_ok {
                return Err(StorageError::Corrupt(
                    "source segment integrity checksum mismatch".to_string(),
                ));
            }
            Ok(())
        },
    )?;
    fs::rename(&stage, &source)?;
    sync_directory(&paths.root)?;
    invalidate_data_segment_cache(&source);
    invalidate_segment_index_cache_checked(&paths.segment_index_file)?;

    let descriptor = index
        .descriptors
        .iter_mut()
        .find(|descriptor| descriptor.segment_seq == candidate.descriptor.segment_seq)
        .ok_or_else(|| StorageError::Corrupt("segment descriptor disappeared".to_string()))?;
    descriptor.payload_bytes = staged_header.payload_len;
    descriptor.first_blob_id = first_blob_id;
    descriptor.last_blob_id = descriptor.last_blob_id.max(candidate.last_blob_id);
    descriptor.live_bytes = u32::try_from(logical_live_bytes).unwrap_or(u32::MAX);
    Ok(())
}

fn prune_metadata(
    paths: &LayoutPaths,
    records: Vec<BlkMapRecord>,
) -> Result<Vec<BlkMapRecord>, StorageError> {
    let active = records
        .iter()
        .filter(|record| record.state == BlkMapState::Active)
        .cloned()
        .collect::<Vec<_>>();
    let max_active_blob = active
        .iter()
        .map(|record| record.physical_offset)
        .max()
        .unwrap_or(0);
    let stale_witness = records
        .iter()
        .filter(|record| record.state == BlkMapState::Stale)
        .max_by_key(|record| record.physical_offset)
        .filter(|record| record.physical_offset > max_active_blob)
        .cloned();
    let mut pruned = active;
    if let Some(witness) = stale_witness {
        pruned.push(witness);
    }
    pruned.sort_by_key(|record| (record.logical_start, record.record_id));
    persist_blk_map_records(&paths.blk_map_file, &pruned)?;
    Ok(pruned)
}

fn preserve_blob_id_high_water(
    index: &mut super::segment_index::SegmentIndex,
    records: &[BlkMapRecord],
) {
    let high_water = index
        .descriptors
        .iter()
        .map(|descriptor| descriptor.last_blob_id)
        .chain(records.iter().map(|record| record.physical_offset))
        .max()
        .unwrap_or(0);
    if let Some(active) = index
        .descriptors
        .iter_mut()
        .find(|descriptor| descriptor.state == SegmentState::Active)
    {
        active.last_blob_id = active.last_blob_id.max(high_water);
    }
}

fn rebuild_dedup_refcounts(
    paths: &LayoutPaths,
    records: &[BlkMapRecord],
) -> Result<(), StorageError> {
    let mut refcounts = HashMap::<u64, u32>::new();
    for record in records
        .iter()
        .filter(|record| record.state == BlkMapState::Active)
    {
        if record.dedup_entry_id == 0 {
            continue;
        }
        let count = refcounts.entry(record.dedup_entry_id).or_default();
        *count = count
            .checked_add(1)
            .ok_or_else(|| StorageError::Corrupt("dedup reference count overflow".to_string()))?;
    }
    let (_sequence, entries) = load_dedup_index(&paths.dedup_file)?;
    let mut rebuilt = Vec::new();
    for mut entry in entries {
        if let Some(count) = refcounts.remove(&entry.entry_id) {
            entry.ref_count = count;
            rebuilt.push(entry);
        }
    }
    if !refcounts.is_empty() {
        return Err(StorageError::Corrupt(
            "active map references missing dedup entry".to_string(),
        ));
    }
    persist_dedup_entries(&paths.dedup_file, &rebuilt)?;
    Ok(())
}

fn metadata_temporary_bytes(paths: &LayoutPaths) -> Result<u64, StorageError> {
    let filemarks_path = paths.root.join("filemarks.state");
    let usage_path = paths.root.join("usage.counters");
    let paths = [
        &paths.metadata_file,
        &paths.blk_map_file,
        &paths.lookup_file,
        &paths.reclaim_file,
        &paths.dedup_file,
        &paths.segment_index_file,
        &filemarks_path,
        &usage_path,
    ];
    paths
        .iter()
        .try_fold(4096u64, |total, path| match fs::metadata(path) {
            Ok(metadata) => total
                .checked_add(metadata.len())
                .ok_or_else(|| StorageError::Corrupt("metadata estimate overflow".to_string())),
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(total),
            Err(err) => Err(StorageError::Io(err)),
        })
}

fn preflight_layout_metadata(paths: &LayoutPaths) -> Result<(), StorageError> {
    let filemarks_path = paths.root.join("filemarks.state");
    let usage_path = paths.root.join("usage.counters");
    super::metadata::preflight_maintenance_metadata(&[
        &paths.metadata_file,
        &paths.blk_map_file,
        &paths.lookup_file,
        &paths.reclaim_file,
        &paths.dedup_file,
        &paths.segment_index_file,
        &filemarks_path,
        &usage_path,
    ])
}

pub(super) fn reserve_capacity(
    pool_root: &Path,
    runtime_dir: &Path,
    payload_bytes: u64,
    blob_count: u64,
    touched_segments: u64,
    metadata_bytes: u64,
) -> Result<FilesystemReservation, StorageError> {
    let lock = FilesystemLock::acquire(pool_root, runtime_dir)?;
    let snapshot = lock.probe()?;
    if snapshot.filesystem_id != lock.filesystem_id {
        return Err(StorageError::SpaceAdmission(
            super::space_guard::SpaceGuardError::UnknownCapacity,
        ));
    }
    let in_flight = lock.active_reservation_bytes()?;
    let peak = estimate_write_peak_bytes(
        payload_bytes,
        blob_count,
        touched_segments,
        metadata_bytes,
        snapshot.allocation_unit,
    )?;
    let total_peak = in_flight
        .checked_add(peak)
        .ok_or(StorageError::SpaceAdmission(
            super::space_guard::SpaceGuardError::ArithmeticOverflow,
        ))?;
    ensure_capacity(snapshot.available_bytes, total_peak, SPACE_RESERVE_BYTES)?;
    let reservation = lock.reserve(peak)?;
    drop(lock);
    Ok(reservation)
}

pub(crate) fn allocated_layout_bytes(root: &Path) -> Result<u64, StorageError> {
    fn walk(path: &Path, total: &mut u64) -> Result<(), StorageError> {
        let metadata = fs::symlink_metadata(path)?;
        if metadata.file_type().is_symlink() {
            return Err(StorageError::Conflict(
                "symlink found in cartridge layout".to_string(),
            ));
        }
        if metadata.is_dir() {
            for entry in fs::read_dir(path)? {
                walk(&entry?.path(), total)?;
            }
            return Ok(());
        }
        let bytes = metadata
            .blocks()
            .checked_mul(512)
            .ok_or_else(|| StorageError::Corrupt("allocated byte count overflow".to_string()))?;
        *total = total
            .checked_add(bytes)
            .ok_or_else(|| StorageError::Corrupt("allocated byte count overflow".to_string()))?;
        Ok(())
    }

    let mut total = 0;
    walk(root, &mut total)?;
    Ok(total)
}

fn sync_directory(path: &Path) -> Result<(), StorageError> {
    File::open(path)?.sync_all()?;
    Ok(())
}
