use std::collections::{HashMap, HashSet};
use std::fs;

use super::blk_map::{load_blk_map_records, persist_blk_map_records, BlkMapRecord, BlkMapState};
use super::compression::{compress_payload, decompress_payload, CompressionCodec};
use super::data_path::{
    append_data_blob, flush_pending_writes, invalidate_data_segment_cache, load_blob_by_location,
    mark_checkpoint_clean, mark_checkpoint_dirty, sync_layout_segments,
};
use super::dedup::{load_dedup_index, persist_dedup_entries};
use super::layout::LayoutPaths;
use super::map_lookup::rebuild_lookup_from_blk_map;
use super::metadata::StorageError;
use super::reclaim::{upsert_reclaim_candidates, ReclaimReason};
use super::segment_index::{
    data_segment_path, load_segment_index, persist_segment_index, remove_segment_descriptor,
    SegmentState,
};

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CompactionReport {
    pub source_seq: u32,
    pub destination_seq: u32,
    pub blobs_moved: usize,
    pub bytes_moved: u64,
    pub bytes_reclaimed: u64,
    pub codec_upgrades: usize,
}

pub fn compact_segment(
    paths: &LayoutPaths,
    source_seq: u32,
    target_codec: CompressionCodec,
) -> Result<CompactionReport, StorageError> {
    let mut index = load_segment_index(&paths.segment_index_file)?;
    let source_descriptor = index
        .descriptors
        .iter()
        .find(|descriptor| descriptor.segment_seq == source_seq)
        .ok_or_else(|| StorageError::NotFound("source segment not found".to_string()))?;
    if source_descriptor.state == SegmentState::Active {
        return Err(StorageError::Conflict(
            "cannot compact active data segment".to_string(),
        ));
    }
    let source_payload_bytes = source_descriptor.payload_bytes;

    let _ = flush_pending_writes(paths)?;
    let _ = mark_checkpoint_dirty(paths)?;
    if let Some(descriptor) = index
        .descriptors
        .iter_mut()
        .find(|descriptor| descriptor.segment_seq == source_seq)
    {
        descriptor.state = SegmentState::Compacting;
    }
    persist_segment_index(&paths.segment_index_file, &index)?;

    let (_seq, records) = load_blk_map_records(&paths.blk_map_file)?;
    let source_records = records
        .iter()
        .filter(|record| {
            record.state == BlkMapState::Active && record.physical_segment_id == source_seq as u64
        })
        .cloned()
        .collect::<Vec<_>>();

    if source_records.is_empty() {
        sync_layout_segments(paths)?;
        remove_source_segment(paths, &mut index, source_seq)?;
        let _ = mark_checkpoint_clean(paths)?;
        return Ok(CompactionReport {
            source_seq,
            destination_seq: index.active_descriptor()?.segment_seq,
            blobs_moved: 0,
            bytes_moved: 0,
            bytes_reclaimed: source_payload_bytes,
            codec_upgrades: 0,
        });
    }

    let mut moved_by_blob = HashMap::<(u64, u64), (u64, u64, CompressionCodec, u32)>::new();
    let old_record_ids = source_records
        .iter()
        .map(|record| record.record_id)
        .collect::<Vec<_>>();
    let mut bytes_moved = 0u64;
    let mut codec_upgrades = 0usize;
    let mut destination_seq = index.active_descriptor()?.segment_seq;
    let mut moved_records = Vec::with_capacity(source_records.len());

    for record in &source_records {
        let blob_key = (record.physical_segment_id, record.physical_offset);
        let moved = if let Some(existing) = moved_by_blob.get(&blob_key) {
            *existing
        } else {
            let blob =
                load_blob_by_location(paths, record.physical_segment_id, record.physical_offset)?;
            let payload = decompress_payload(blob.codec, &blob.bytes, blob.logical_len as usize)?;
            let (selected_codec, encoded, _compressed) = compress_payload(target_codec, &payload)?;
            if selected_codec != blob.codec {
                codec_upgrades += 1;
            }
            let (new_segment_id, new_blob_id) = append_data_blob(
                paths,
                selected_codec,
                blob.payload_checksum,
                blob.logical_len,
                &encoded,
                true,
            )?;
            let stored_len = u32::try_from(encoded.len())
                .map_err(|_| StorageError::Conflict("compacted payload too large".to_string()))?;
            destination_seq = new_segment_id as u32;
            bytes_moved = bytes_moved.saturating_add(stored_len as u64);
            let moved = (new_segment_id, new_blob_id, selected_codec, stored_len);
            moved_by_blob.insert(blob_key, moved);
            moved
        };

        moved_records.push(BlkMapRecord {
            record_id: 0,
            logical_start: record.logical_start,
            logical_len: record.logical_len,
            physical_segment_id: moved.0,
            physical_offset: moved.1,
            filemark_count: record.filemark_count,
            state: BlkMapState::Active,
            dedup_entry_id: record.dedup_entry_id,
            compression: moved.2,
            compressed_len: moved.3,
            payload_checksum: record.payload_checksum,
        });
    }

    persist_compacted_blk_map(&paths.blk_map_file, records, &old_record_ids, moved_records)?;
    update_moved_dedup_entries(paths, &source_records, &moved_by_blob)?;
    let _ = rebuild_lookup_from_blk_map(&paths.blk_map_file, &paths.lookup_file)?;
    let _ = upsert_reclaim_candidates(
        &paths.lookup_file,
        &paths.reclaim_file,
        &old_record_ids,
        ReclaimReason::Compacted,
    )?;

    sync_layout_segments(paths)?;
    index = load_segment_index(&paths.segment_index_file)?;
    remove_source_segment(paths, &mut index, source_seq)?;
    let _ = mark_checkpoint_clean(paths)?;

    Ok(CompactionReport {
        source_seq,
        destination_seq,
        blobs_moved: moved_by_blob.len(),
        bytes_moved,
        bytes_reclaimed: source_payload_bytes,
        codec_upgrades,
    })
}

fn update_moved_dedup_entries(
    paths: &LayoutPaths,
    records: &[BlkMapRecord],
    moved_by_blob: &HashMap<(u64, u64), (u64, u64, CompressionCodec, u32)>,
) -> Result<(), StorageError> {
    let (_seq, mut entries) = load_dedup_index(&paths.dedup_file)?;
    let mut changed = false;
    for record in records {
        if record.dedup_entry_id == 0 {
            continue;
        }
        let Some(moved) = moved_by_blob.get(&(record.physical_segment_id, record.physical_offset))
        else {
            continue;
        };
        if let Some(entry) = entries
            .iter_mut()
            .find(|entry| entry.entry_id == record.dedup_entry_id)
        {
            entry.stored_blob_id = moved.1;
            entry.compression = moved.2;
            entry.stored_len = moved.3;
            changed = true;
        }
    }
    if changed {
        persist_dedup_entries(&paths.dedup_file, &entries)?;
    }
    Ok(())
}

fn persist_compacted_blk_map(
    path: &std::path::Path,
    mut records: Vec<BlkMapRecord>,
    old_record_ids: &[u64],
    mut moved_records: Vec<BlkMapRecord>,
) -> Result<(), StorageError> {
    let mut next_record_id = records
        .iter()
        .map(|record| record.record_id)
        .max()
        .unwrap_or(0)
        .saturating_add(1);
    // O(1) lookup per record instead of O(M) Vec scan; matches the fix in
    // mark_blk_map_stale_batch.
    let stale_ids: HashSet<u64> = old_record_ids.iter().copied().collect();
    for record in &mut records {
        if stale_ids.contains(&record.record_id) {
            record.state = BlkMapState::Stale;
        }
    }
    for record in &mut moved_records {
        record.record_id = next_record_id;
        next_record_id = next_record_id.saturating_add(1);
    }
    records.extend(moved_records);
    records.sort_by_key(|record| (record.logical_start, record.record_id));
    persist_blk_map_records(path, &records)
}

fn remove_source_segment(
    paths: &LayoutPaths,
    index: &mut super::segment_index::SegmentIndex,
    source_seq: u32,
) -> Result<(), StorageError> {
    if index
        .descriptors
        .iter()
        .any(|descriptor| descriptor.segment_seq == source_seq)
    {
        remove_segment_descriptor(index, source_seq);
        persist_segment_index(&paths.segment_index_file, index)?;
    }
    let source_path = data_segment_path(paths, source_seq);
    if source_path.exists() {
        invalidate_data_segment_cache(&source_path);
        fs::remove_file(source_path)?;
        fs::File::open(&paths.root)?.sync_all()?;
    }
    Ok(())
}
