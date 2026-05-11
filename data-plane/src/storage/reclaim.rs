use std::path::Path;

use super::blk_map::mark_blk_map_stale;
use super::layout::SegmentKind;
use super::map_lookup::load_lookup_records;
use super::metadata::{checked_usize_from_u64, StorageError};
use super::segment::{read_segment_file, write_segment_file};

const RECLAIM_RECORD_SIZE: usize = 25;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[repr(u8)]
pub enum ReclaimReason {
    Superseded = 1,
    Truncated = 2,
    Compacted = 3,
}

impl ReclaimReason {
    fn from_u8(v: u8) -> Result<Self, StorageError> {
        match v {
            1 => Ok(Self::Superseded),
            2 => Ok(Self::Truncated),
            3 => Ok(Self::Compacted),
            _ => Err(StorageError::Corrupt("invalid reclaim reason".to_string())),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReclaimCandidate {
    pub candidate_id: u64,
    pub blk_map_record_id: u64,
    pub reason: ReclaimReason,
    pub safe_to_reclaim: bool,
}

impl ReclaimCandidate {
    fn encode(&self) -> Vec<u8> {
        let mut out = Vec::with_capacity(RECLAIM_RECORD_SIZE);
        out.extend_from_slice(&self.candidate_id.to_le_bytes());
        out.extend_from_slice(&self.blk_map_record_id.to_le_bytes());
        out.push(self.reason as u8);
        out.push(u8::from(self.safe_to_reclaim));
        out.extend_from_slice(&[0u8; 7]);
        out
    }

    fn decode(buf: &[u8]) -> Result<Self, StorageError> {
        if buf.len() < RECLAIM_RECORD_SIZE {
            return Err(StorageError::Corrupt(
                "reclaim record too short".to_string(),
            ));
        }
        Ok(Self {
            candidate_id: u64::from_le_bytes(
                buf[0..8]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("reclaim parse failed".to_string()))?,
            ),
            blk_map_record_id: u64::from_le_bytes(
                buf[8..16]
                    .try_into()
                    .map_err(|_| StorageError::Corrupt("reclaim parse failed".to_string()))?,
            ),
            reason: ReclaimReason::from_u8(buf[16])?,
            safe_to_reclaim: buf[17] == 1,
        })
    }
}

pub fn load_reclaim_candidates(path: &Path) -> Result<Vec<ReclaimCandidate>, StorageError> {
    let (_header, payload) = read_segment_file(path, SegmentKind::Reclaim)?;
    if payload.is_empty() {
        return Ok(Vec::new());
    }
    if payload.len() < 8 {
        return Err(StorageError::Corrupt(
            "reclaim payload too short".to_string(),
        ));
    }

    let raw_count = u64::from_le_bytes(
        payload[0..8]
            .try_into()
            .map_err(|_| StorageError::Corrupt("reclaim count parse failed".to_string()))?,
    );
    let count = checked_usize_from_u64(raw_count, "reclaim count")?;
    if payload.len().saturating_sub(8) / RECLAIM_RECORD_SIZE < count {
        return Err(StorageError::Corrupt(
            "reclaim payload truncated".to_string(),
        ));
    }
    let mut offset = 8;
    let mut out = Vec::with_capacity(count);
    for _ in 0..count {
        if payload.len() < offset + RECLAIM_RECORD_SIZE {
            return Err(StorageError::Corrupt(
                "reclaim payload truncated".to_string(),
            ));
        }
        out.push(ReclaimCandidate::decode(
            &payload[offset..offset + RECLAIM_RECORD_SIZE],
        )?);
        offset += RECLAIM_RECORD_SIZE;
    }
    Ok(out)
}

pub fn upsert_reclaim_candidate(
    blk_map_path: &Path,
    lookup_path: &Path,
    reclaim_path: &Path,
    blk_map_record_id: u64,
    reason: ReclaimReason,
) -> Result<ReclaimCandidate, StorageError> {
    let _ = mark_blk_map_stale(blk_map_path, blk_map_record_id)?;

    let (_seq, lookups) = load_lookup_records(lookup_path)?;
    let still_referenced = lookups.iter().any(|entry| {
        blk_map_record_id >= entry.blk_map_ref_start && blk_map_record_id <= entry.blk_map_ref_end
    });

    let mut candidates = load_reclaim_candidates(reclaim_path)?;
    if let Some(existing) = candidates
        .iter_mut()
        .find(|c| c.blk_map_record_id == blk_map_record_id)
    {
        existing.reason = reason;
        existing.safe_to_reclaim = !still_referenced;
    } else {
        candidates.push(ReclaimCandidate {
            candidate_id: candidates.last().map(|c| c.candidate_id + 1).unwrap_or(1),
            blk_map_record_id,
            reason,
            safe_to_reclaim: !still_referenced,
        });
    }

    persist_candidates(reclaim_path, &candidates)?;
    candidates
        .into_iter()
        .find(|c| c.blk_map_record_id == blk_map_record_id)
        .ok_or_else(|| StorageError::Corrupt("candidate missing after persist".to_string()))
}

pub fn upsert_reclaim_candidates(
    lookup_path: &Path,
    reclaim_path: &Path,
    blk_map_record_ids: &[u64],
    reason: ReclaimReason,
) -> Result<Vec<ReclaimCandidate>, StorageError> {
    if blk_map_record_ids.is_empty() {
        return Ok(Vec::new());
    }

    let (_seq, lookups) = load_lookup_records(lookup_path)?;
    let mut candidates = load_reclaim_candidates(reclaim_path)?;
    let mut updated = Vec::new();

    for record_id in blk_map_record_ids {
        let still_referenced = lookups.iter().any(|entry| {
            *record_id >= entry.blk_map_ref_start && *record_id <= entry.blk_map_ref_end
        });
        if let Some(existing) = candidates
            .iter_mut()
            .find(|c| c.blk_map_record_id == *record_id)
        {
            existing.reason = reason;
            existing.safe_to_reclaim = !still_referenced;
            updated.push(existing.clone());
        } else {
            let candidate = ReclaimCandidate {
                candidate_id: candidates.last().map(|c| c.candidate_id + 1).unwrap_or(1),
                blk_map_record_id: *record_id,
                reason,
                safe_to_reclaim: !still_referenced,
            };
            candidates.push(candidate.clone());
            updated.push(candidate);
        }
    }

    persist_candidates(reclaim_path, &candidates)?;
    Ok(updated)
}

pub fn refresh_reclaim_safety(
    lookup_path: &Path,
    reclaim_path: &Path,
) -> Result<usize, StorageError> {
    let (_seq, lookups) = load_lookup_records(lookup_path)?;
    let mut candidates = load_reclaim_candidates(reclaim_path)?;

    let mut updated = 0usize;
    for candidate in &mut candidates {
        let referenced = lookups.iter().any(|entry| {
            candidate.blk_map_record_id >= entry.blk_map_ref_start
                && candidate.blk_map_record_id <= entry.blk_map_ref_end
        });
        let next = !referenced;
        if candidate.safe_to_reclaim != next {
            candidate.safe_to_reclaim = next;
            updated += 1;
        }
    }

    persist_candidates(reclaim_path, &candidates)?;
    Ok(updated)
}

fn persist_candidates(path: &Path, candidates: &[ReclaimCandidate]) -> Result<(), StorageError> {
    let mut payload = Vec::new();
    payload.extend_from_slice(&(candidates.len() as u64).to_le_bytes());
    for c in candidates {
        payload.extend_from_slice(&c.encode());
    }
    write_segment_file(
        path,
        SegmentKind::Reclaim,
        4,
        candidates.len() as u64,
        &payload,
    )
}
