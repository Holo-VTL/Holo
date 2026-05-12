use std::collections::{HashMap, VecDeque};
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard, OnceLock};
#[cfg(test)]
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread::{self, JoinHandle};

use crate::media::mount_bridge;
use crate::scsi_tape::error::TapeError;
use crate::scsi_tape::reservation::{
    clear_keys, ensure_reservation_access, preempt_key, register_and_move_key,
    register_ignore_existing, register_key, release_key, reserve_key, snapshot,
};
use crate::scsi_tape::state::{BlockMode, MountState, TapeState};
use crate::storage::{
    discard_layout_caches, flush_pending_writes, initialize_layout, persist_filemarks,
    persist_retention_state, read_logical_block, reset_layout_for_overwrite, run_unmap,
    write_logical_block, CompressionCodec, StorageError, WriteOptions,
};
use crate::worm::enforcer::{enforce_write, RetentionState};

const DEFAULT_INITIATOR: &str = "initiator-default";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum EraseMode {
    Short,
    Long,
}

struct ReadPrefetchJob {
    position: u64,
    handle: JoinHandle<Result<Option<crate::storage::LogicalReadResult>, StorageError>>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct ReadPrefetchDegradation {
    invalidation_failures: u64,
    bypass_reads: bool,
}

#[cfg(test)]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(super) struct ReadPrefetchDegradationStatus {
    pub invalidation_failures: u64,
    pub bypass_reads: bool,
}

fn read_prefetch_jobs() -> &'static Mutex<HashMap<PathBuf, VecDeque<ReadPrefetchJob>>> {
    static JOBS: OnceLock<Mutex<HashMap<PathBuf, VecDeque<ReadPrefetchJob>>>> = OnceLock::new();
    JOBS.get_or_init(|| Mutex::new(HashMap::new()))
}

fn read_prefetch_degradation() -> &'static Mutex<HashMap<PathBuf, ReadPrefetchDegradation>> {
    static STATE: OnceLock<Mutex<HashMap<PathBuf, ReadPrefetchDegradation>>> = OnceLock::new();
    STATE.get_or_init(|| Mutex::new(HashMap::new()))
}

#[cfg(test)]
fn fail_next_read_prefetch_invalidation_flag() -> &'static AtomicBool {
    static FAIL_NEXT: AtomicBool = AtomicBool::new(false);
    &FAIL_NEXT
}

fn lock_read_prefetch_jobs(
) -> Result<MutexGuard<'static, HashMap<PathBuf, VecDeque<ReadPrefetchJob>>>, StorageError> {
    read_prefetch_jobs()
        .lock()
        .map_err(|_| StorageError::Conflict("read prefetch cache poisoned".to_string()))
}

fn lock_read_prefetch_degradation(
) -> Result<MutexGuard<'static, HashMap<PathBuf, ReadPrefetchDegradation>>, StorageError> {
    read_prefetch_degradation()
        .lock()
        .map_err(|_| StorageError::Conflict("read prefetch degradation state poisoned".to_string()))
}

fn read_prefetch_enabled() -> bool {
    std::env::var("HOLO_READ_PREFETCH")
        .ok()
        .map(|raw| {
            !matches!(
                raw.trim().to_ascii_lowercase().as_str(),
                "0" | "false" | "no" | "off"
            )
        })
        .unwrap_or(false)
}

fn read_prefetch_depth() -> usize {
    std::env::var("HOLO_READ_PREFETCH_DEPTH")
        .ok()
        .and_then(|raw| raw.trim().parse::<usize>().ok())
        .filter(|value| (1..=8).contains(value))
        .unwrap_or(2)
}

fn tape_dedup_enabled() -> bool {
    // The TCMU adapter starts one handler process per publication, so this is
    // intentionally process-scoped instead of block-scoped.
    static ENABLED: OnceLock<bool> = OnceLock::new();
    *ENABLED.get_or_init(|| {
        std::env::var("HOLO_TAPE_DEDUP_ENABLED")
            .ok()
            .map(|raw| {
                !matches!(
                    raw.trim().to_ascii_lowercase().as_str(),
                    "0" | "false" | "no" | "off"
                )
            })
            .unwrap_or(true)
    })
}

fn tape_payload_checksum_enabled() -> bool {
    static ENABLED: OnceLock<bool> = OnceLock::new();
    *ENABLED.get_or_init(|| {
        std::env::var("HOLO_TAPE_PAYLOAD_CHECKSUM_ENABLED")
            .ok()
            .map(|raw| {
                !matches!(
                    raw.trim().to_ascii_lowercase().as_str(),
                    "0" | "false" | "no" | "off"
                )
            })
            .unwrap_or(true)
    })
}

fn prefetch_key(layout: &crate::storage::LayoutPaths) -> PathBuf {
    layout.root.clone()
}

fn discard_read_prefetch_job(job: ReadPrefetchJob) {
    let _ = job.handle.join();
}

fn discard_read_prefetch_jobs_for_key(layout_root: &Path) {
    let jobs = match lock_read_prefetch_jobs() {
        Ok(mut guard) => guard.remove(layout_root),
        Err(err) => {
            eprintln!("[prefetch] failed to discard read prefetch cache: {err}");
            None
        }
    };
    if let Some(jobs) = jobs {
        for job in jobs {
            discard_read_prefetch_job(job);
        }
    }
}

fn record_read_prefetch_invalidation_failure(layout_root: &Path, err: StorageError) {
    let count = match lock_read_prefetch_degradation() {
        Ok(mut guard) => {
            let state = guard
                .entry(layout_root.to_path_buf())
                .or_insert(ReadPrefetchDegradation {
                    invalidation_failures: 0,
                    bypass_reads: true,
                });
            state.invalidation_failures = state.invalidation_failures.saturating_add(1);
            state.bypass_reads = true;
            state.invalidation_failures
        }
        Err(lock_err) => {
            eprintln!("[prefetch] failed to record read prefetch degradation: {lock_err}");
            0
        }
    };
    eprintln!(
        "[prefetch] read prefetch invalidation degraded layout={} failures={} error={err}",
        layout_root.display(),
        count
    );
}

fn read_prefetch_bypassed(layout_root: &Path) -> bool {
    match lock_read_prefetch_degradation() {
        Ok(guard) => guard
            .get(layout_root)
            .map(|state| state.bypass_reads)
            .unwrap_or(false),
        Err(err) => {
            eprintln!("[prefetch] failed to read prefetch degradation state: {err}");
            true
        }
    }
}

fn clear_read_prefetch_degradation(layout_root: &Path) {
    if let Ok(mut guard) = lock_read_prefetch_degradation() {
        guard.remove(layout_root);
    }
}

#[cfg(test)]
pub(super) fn fail_next_read_prefetch_invalidation_for_test() {
    fail_next_read_prefetch_invalidation_flag().store(true, Ordering::SeqCst);
}

#[cfg(test)]
pub(super) fn read_prefetch_degradation_status_for_test(
    layout_root: &Path,
) -> Option<ReadPrefetchDegradationStatus> {
    lock_read_prefetch_degradation()
        .ok()
        .and_then(|guard| guard.get(layout_root).copied())
        .map(|state| ReadPrefetchDegradationStatus {
            invalidation_failures: state.invalidation_failures,
            bypass_reads: state.bypass_reads,
        })
}

#[cfg(test)]
pub(super) fn clear_read_prefetch_degradation_for_test(layout_root: &Path) {
    clear_read_prefetch_degradation(layout_root);
}

fn invalidate_read_prefetch(layout_root: &Path) {
    #[cfg(test)]
    if fail_next_read_prefetch_invalidation_flag().swap(false, Ordering::SeqCst) {
        record_read_prefetch_invalidation_failure(
            layout_root,
            StorageError::Conflict("injected read prefetch invalidation failure".to_string()),
        );
        return;
    }

    let jobs = match lock_read_prefetch_jobs() {
        Ok(mut guard) => guard.remove(layout_root),
        Err(err) => {
            record_read_prefetch_invalidation_failure(layout_root, err);
            None
        }
    };
    if let Some(jobs) = jobs {
        for job in jobs {
            discard_read_prefetch_job(job);
        }
    }
}

fn invalidate_state_read_prefetch(state: &TapeState) {
    if let Some(layout) = state.active_layout.as_ref() {
        invalidate_read_prefetch(&layout.root);
    }
}

fn tape_write_options(state: &TapeState) -> WriteOptions {
    let dedup_enabled = tape_dedup_enabled();
    let payload_checksum_enabled = tape_payload_checksum_enabled();
    if state.data_compression_enabled {
        WriteOptions {
            dedup_enabled,
            payload_checksum_enabled,
            ..WriteOptions::default()
        }
    } else {
        WriteOptions {
            dedup_enabled,
            preferred_codec: CompressionCodec::None,
            force_sync: false,
            payload_checksum_enabled,
        }
    }
}

fn take_read_prefetch(
    layout: &crate::storage::LayoutPaths,
    position: u64,
) -> Result<Option<crate::storage::LogicalReadResult>, StorageError> {
    if !read_prefetch_enabled() {
        return Ok(None);
    }

    let key = prefetch_key(layout);
    if read_prefetch_bypassed(&key) {
        discard_read_prefetch_jobs_for_key(&key);
        return Ok(None);
    }
    let mut stale_jobs = Vec::new();
    let job = {
        let mut guard = lock_read_prefetch_jobs()?;
        let mut job = None;
        let mut remove_queue = false;
        if let Some(queue) = guard.get_mut(&key) {
            while queue
                .front()
                .map(|queued| queued.position < position)
                .unwrap_or(false)
            {
                if let Some(stale) = queue.pop_front() {
                    stale_jobs.push(stale);
                }
            }
            match queue.front() {
                Some(queued) if queued.position == position => {
                    job = queue.pop_front();
                    remove_queue = queue.is_empty();
                }
                Some(_) => {
                    remove_queue = true;
                }
                None => {
                    remove_queue = true;
                }
            }
        }
        if remove_queue {
            if let Some(stale) = guard.remove(&key) {
                stale_jobs.extend(stale);
            }
        }
        job
    };
    for stale in stale_jobs {
        discard_read_prefetch_job(stale);
    }

    let Some(job) = job else {
        return Ok(None);
    };
    job.handle
        .join()
        .map_err(|_| StorageError::Conflict("read prefetch worker panicked".to_string()))?
}

fn schedule_read_prefetch(
    layout: &crate::storage::LayoutPaths,
    position: u64,
    eod_position: u64,
    block_size: u64,
) {
    if !read_prefetch_enabled() || position >= eod_position || block_size == 0 {
        return;
    }

    let key = prefetch_key(layout);
    if read_prefetch_bypassed(&key) {
        discard_read_prefetch_jobs_for_key(&key);
        return;
    }

    let depth = read_prefetch_depth();
    let mut wanted = Vec::with_capacity(depth);
    let mut next_position = position;
    for _ in 0..depth {
        if next_position >= eod_position {
            break;
        }
        wanted.push(next_position);
        next_position = next_position.saturating_add(block_size);
    }
    if wanted.is_empty() {
        return;
    }

    let mut stale_jobs = Vec::new();
    let mut guard = match lock_read_prefetch_jobs() {
        Ok(guard) => guard,
        Err(err) => {
            eprintln!("[prefetch] failed to schedule read prefetch: {err}");
            return;
        }
    };
    let queue = guard.entry(key).or_default();
    let mut idx = 0;
    while idx < queue.len() {
        if wanted.contains(&queue[idx].position) {
            idx += 1;
        } else if let Some(stale) = queue.remove(idx) {
            stale_jobs.push(stale);
        }
    }

    for position in wanted {
        if queue.iter().any(|job| job.position == position) {
            continue;
        }
        let layout = layout.clone();
        let handle = thread::spawn(move || read_logical_block(&layout, position));
        queue.push_back(ReadPrefetchJob { position, handle });
    }
    queue.make_contiguous().sort_by_key(|job| job.position);
    drop(guard);

    for stale in stale_jobs {
        discard_read_prefetch_job(stale);
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SpaceBranch {
    Blocks,
    Filemarks,
    EndOfData,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PositionReport {
    pub current_position: u64,
    pub eod_position: u64,
    pub file_number: u64,
    pub set_number: u64,
    pub filemark_count: u32,
    pub next_filemark: Option<u64>,
    pub early_warning: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReservationReport {
    pub generation: u32,
    pub registration_count: usize,
    pub active_owner: Option<String>,
    pub active_key: Option<u64>,
}

pub fn default_initiator() -> &'static str {
    DEFAULT_INITIATOR
}

pub fn configured_initiator() -> &'static str {
    static INITIATOR: OnceLock<String> = OnceLock::new();
    INITIATOR
        .get_or_init(|| {
            std::env::var("HOLO_INITIATOR_IQN")
                .ok()
                .map(|raw| raw.trim().to_string())
                .filter(|value| !value.is_empty())
                .unwrap_or_else(|| DEFAULT_INITIATOR.to_string())
        })
        .as_str()
}

pub fn load_media(state: &mut TapeState, cartridge_id: &str) -> Result<(), TapeError> {
    invalidate_state_read_prefetch(state);
    mount_bridge::attach_cartridge(state, cartridge_id)
}

pub fn unload_media(state: &mut TapeState) -> Result<(), TapeError> {
    invalidate_state_read_prefetch(state);
    let active_layout = state.active_layout.clone();
    if let Some(layout) = active_layout.as_ref() {
        flush_pending_writes(layout)?;
    }
    state.unmount();
    if let Some(layout) = active_layout.as_ref() {
        discard_layout_caches(layout);
        clear_read_prefetch_degradation(&layout.root);
    }
    Ok(())
}

pub fn rewind_media(state: &mut TapeState) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    invalidate_state_read_prefetch(state);
    if let Some(layout) = state.active_layout.as_ref() {
        flush_pending_writes(layout)?;
    }
    state.current_position = 0;
    Ok(())
}

pub fn set_worm_policy(
    state: &mut TapeState,
    is_worm_media: bool,
    retention_locked: bool,
) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    if retention_locked && !is_worm_media {
        return Err(TapeError::InvalidArgument(
            "retention lock requires worm media".to_string(),
        ));
    }
    if state.retention_policy.retention_locked && !retention_locked {
        return Err(TapeError::WormWriteProtected);
    }

    state.retention_policy.is_worm_media = is_worm_media;
    state.retention_policy.retention_locked = retention_locked;
    if let Some(layout) = state.active_layout.as_ref() {
        persist_retention_state(&layout.root, is_worm_media, retention_locked)?;
    }
    Ok(())
}

pub fn register_reservation(
    state: &mut TapeState,
    initiator: Option<&str>,
    key: u64,
) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    register_key(state, initiator.unwrap_or(DEFAULT_INITIATOR), key)
}

pub fn register_ignore_reservation(
    state: &mut TapeState,
    initiator: Option<&str>,
    service_key: u64,
) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    register_ignore_existing(state, initiator.unwrap_or(DEFAULT_INITIATOR), service_key)
}

pub fn reserve_reservation(
    state: &mut TapeState,
    initiator: Option<&str>,
    key: u64,
) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    reserve_key(state, initiator.unwrap_or(DEFAULT_INITIATOR), key)
}

pub fn release_reservation(
    state: &mut TapeState,
    initiator: Option<&str>,
    key: u64,
) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    release_key(state, initiator.unwrap_or(DEFAULT_INITIATOR), key)
}

pub fn clear_reservation(
    state: &mut TapeState,
    initiator: Option<&str>,
    key: u64,
) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    clear_keys(state, initiator.unwrap_or(DEFAULT_INITIATOR), key)
}

pub fn preempt_reservation(
    state: &mut TapeState,
    initiator: Option<&str>,
    key: u64,
    service_key: u64,
) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    preempt_key(
        state,
        initiator.unwrap_or(DEFAULT_INITIATOR),
        key,
        service_key,
    )
}

pub fn register_and_move_reservation(
    state: &mut TapeState,
    initiator: Option<&str>,
    key: u64,
    service_key: u64,
    target_initiator: &str,
    unregister_source: bool,
) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    register_and_move_key(
        state,
        initiator.unwrap_or(DEFAULT_INITIATOR),
        key,
        service_key,
        target_initiator,
        unregister_source,
    )
}

pub fn read_reservation(state: &TapeState) -> Result<ReservationReport, TapeError> {
    if state.mount_state == MountState::Empty {
        return Err(TapeError::NotReady("no media loaded".to_string()));
    }

    let reservation_snapshot = snapshot(state);
    Ok(ReservationReport {
        generation: reservation_snapshot.generation,
        registration_count: reservation_snapshot.registration_count,
        active_owner: reservation_snapshot.active_owner,
        active_key: reservation_snapshot.active_key,
    })
}

pub fn erase_media(
    state: &mut TapeState,
    mode: EraseMode,
    initiator: Option<&str>,
) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    invalidate_state_read_prefetch(state);
    enforce_protected_access(state, initiator.unwrap_or(DEFAULT_INITIATOR))?;
    enforce_worm_erase_allowed(state)?;

    let cartridge_id = state
        .cartridge_id
        .clone()
        .ok_or_else(|| TapeError::NotReady("missing cartridge id".to_string()))?;
    let layout = state
        .active_layout
        .as_ref()
        .ok_or_else(|| TapeError::NotReady("active layout missing".to_string()))?
        .clone();
    let preserved_partition_runtime = state.partition_runtime.clone();
    let preserved_capacity_bytes = state.cartridge_capacity_bytes;
    let preserved_retention_policy = state.retention_policy.clone();

    match mode {
        EraseMode::Short => reset_layout_for_overwrite(&layout)?,
        EraseMode::Long => {
            discard_layout_caches(&layout);
            if let Err(err) = fs::remove_dir_all(&layout.root) {
                if err.kind() != std::io::ErrorKind::NotFound {
                    return Err(StorageError::Io(err).into());
                }
            }
            initialize_layout(&layout)?;
            reset_layout_for_overwrite(&layout)?;
        }
    }
    state.mount(cartridge_id, layout);
    state.partition_runtime = preserved_partition_runtime;
    state.cartridge_capacity_bytes = preserved_capacity_bytes;
    state.retention_policy = preserved_retention_policy;
    if let Some(layout) = state.active_layout.as_ref() {
        persist_filemarks(&layout.root, &state.filemarks)?;
        persist_retention_state(
            &layout.root,
            state.retention_policy.is_worm_media,
            state.retention_policy.retention_locked,
        )?;
    }
    Ok(())
}

pub fn set_block_mode_fixed(state: &mut TapeState, block_size: u32) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    if block_size == 0 {
        return Err(TapeError::InvalidArgument(
            "fixed block size must be > 0".to_string(),
        ));
    }

    state.block_mode.mode = BlockMode::Fixed;
    state.block_mode.fixed_block_size = block_size;
    Ok(())
}

pub fn set_block_mode_variable(state: &mut TapeState) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    state.block_mode.mode = BlockMode::Variable;
    state.block_mode.fixed_block_size = 0;
    Ok(())
}

pub fn write_data(
    state: &mut TapeState,
    payload: &[u8],
    initiator: Option<&str>,
) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    invalidate_state_read_prefetch(state);
    let payload_len = validate_payload_len(payload.len())?;
    if payload.is_empty() {
        if state.block_mode.mode == BlockMode::Variable {
            return Ok(());
        }
        return Err(TapeError::InvalidArgument(
            "write payload cannot be empty".to_string(),
        ));
    }
    enforce_protected_access(state, initiator.unwrap_or(DEFAULT_INITIATOR))?;
    enforce_worm_append_only(state)?;

    if state.block_mode.mode == BlockMode::Fixed {
        let expected = state.block_mode.fixed_block_size;
        let actual = payload_len;
        if expected != actual {
            return Err(TapeError::FixedBlockSizeMismatch { expected, actual });
        }
    }

    let layout = state
        .active_layout
        .as_ref()
        .ok_or_else(|| TapeError::NotReady("active layout missing".to_string()))?
        .clone();
    truncate_tail_before_write(state, &layout)?;
    ensure_capacity_available(state, payload.len() as u64)?;

    write_logical_block(
        &layout,
        state.current_position,
        payload,
        validate_payload_len(state.filemarks.len())?,
        tape_write_options(state),
        None,
    )?;

    record_block(state, state.current_position, payload_len);
    state.current_position += payload.len() as u64;
    if state.current_position > state.eod_position {
        state.eod_position = state.current_position;
    }
    state.command_counters.write_ops = state.command_counters.write_ops.saturating_add(1);
    state
        .record_write_usage(payload.len() as u64)
        .map_err(StorageError::from)?;
    Ok(())
}

fn truncate_tail_before_write(
    state: &mut TapeState,
    layout: &crate::storage::LayoutPaths,
) -> Result<(), TapeError> {
    if state.current_position >= state.eod_position {
        return Ok(());
    }

    if state.current_position == 0 {
        reset_layout_for_overwrite(layout)?;
        state.filemarks.clear();
        state.block_lengths.clear();
        state.block_starts.clear();
        state.eod_position = 0;
        persist_filemarks(&layout.root, &state.filemarks)?;
        return Ok(());
    }

    let mut start = state.current_position;
    let mut remaining = state.eod_position.saturating_sub(state.current_position);
    while remaining > 0 {
        let span = remaining.min(u32::MAX as u64) as u32;
        if span == 0 {
            break;
        }
        run_unmap(layout, start, span)?;
        start = start.saturating_add(span as u64);
        remaining = remaining.saturating_sub(span as u64);
    }

    let truncate_point = state.current_position;
    state.filemarks.retain(|mark| *mark < truncate_point);
    state.block_lengths.retain(|block_start, block_len| {
        block_start.saturating_add(*block_len as u64) <= truncate_point
    });
    state
        .block_starts
        .retain(|block_start| state.block_lengths.contains_key(block_start));

    persist_filemarks(&layout.root, &state.filemarks)?;
    state.eod_position = state.current_position;
    Ok(())
}

pub fn read_data(state: &mut TapeState) -> Result<Vec<u8>, TapeError> {
    ensure_loaded(state)?;

    let layout = state
        .active_layout
        .as_ref()
        .ok_or_else(|| TapeError::NotReady("active layout missing".to_string()))?;

    let read = read_logical_block(layout, state.current_position)?.ok_or_else(|| {
        TapeError::NotFound(format!(
            "no block mapped at logical position {}",
            state.current_position
        ))
    })?;

    if state.block_mode.mode == BlockMode::Fixed {
        let expected = state.block_mode.fixed_block_size;
        let actual = read.logical_len;
        if expected != actual {
            return Err(TapeError::FixedBlockSizeMismatch { expected, actual });
        }
    }

    state.current_position += read.logical_len as u64;
    state.command_counters.read_ops = state.command_counters.read_ops.saturating_add(1);
    state
        .record_read_usage(read.logical_len as u64)
        .map_err(StorageError::from)?;
    Ok(read.payload)
}

pub fn write_fixed_blocks(
    state: &mut TapeState,
    payload: &[u8],
    block_size: usize,
    initiator: Option<&str>,
) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    invalidate_state_read_prefetch(state);
    if payload.is_empty() || block_size == 0 || !payload.len().is_multiple_of(block_size) {
        return Err(TapeError::InvalidArgument(
            "fixed-block write payload must contain complete blocks".to_string(),
        ));
    }
    let fixed_block_size = validate_payload_len(block_size)?;
    if state.block_mode.mode != BlockMode::Fixed
        || state.block_mode.fixed_block_size != fixed_block_size
    {
        return Err(TapeError::FixedBlockSizeMismatch {
            expected: state.block_mode.fixed_block_size,
            actual: fixed_block_size,
        });
    }
    enforce_protected_access(state, initiator.unwrap_or(DEFAULT_INITIATOR))?;
    enforce_worm_append_only(state)?;

    let layout = state
        .active_layout
        .as_ref()
        .ok_or_else(|| TapeError::NotReady("active layout missing".to_string()))?
        .clone();
    truncate_tail_before_write(state, &layout)?;
    ensure_capacity_available(state, payload.len() as u64)?;

    let filemark_count = validate_payload_len(state.filemarks.len())?;
    for chunk in payload.chunks(block_size) {
        write_logical_block(
            &layout,
            state.current_position,
            chunk,
            filemark_count,
            tape_write_options(state),
            None,
        )?;
        record_block(state, state.current_position, fixed_block_size);
        state.current_position = state.current_position.saturating_add(chunk.len() as u64);
        if state.current_position > state.eod_position {
            state.eod_position = state.current_position;
        }
        state.command_counters.write_ops = state.command_counters.write_ops.saturating_add(1);
        state
            .record_write_usage(chunk.len() as u64)
            .map_err(StorageError::from)?;
    }
    Ok(())
}

pub fn read_fixed_blocks(
    state: &mut TapeState,
    block_size: usize,
    transfer_blocks: usize,
) -> Result<Vec<u8>, TapeError> {
    ensure_loaded(state)?;
    let fixed_block_size = validate_payload_len(block_size)?;
    if transfer_blocks == 0 || block_size == 0 {
        return Err(TapeError::InvalidArgument(
            "fixed-block read requires a non-empty transfer".to_string(),
        ));
    }
    if state.block_mode.mode != BlockMode::Fixed
        || state.block_mode.fixed_block_size != fixed_block_size
    {
        return Err(TapeError::FixedBlockSizeMismatch {
            expected: state.block_mode.fixed_block_size,
            actual: fixed_block_size,
        });
    }

    let layout = state
        .active_layout
        .as_ref()
        .ok_or_else(|| TapeError::NotReady("active layout missing".to_string()))?
        .clone();
    if transfer_blocks == 1 {
        let position = state.current_position;
        let read = match take_read_prefetch(&layout, position)? {
            Some(read) => read,
            None => read_logical_block(&layout, position)?.ok_or_else(|| {
                TapeError::NotFound(format!("no block mapped at logical position {position}"))
            })?,
        };
        if read.logical_start != position {
            invalidate_read_prefetch(&layout.root);
            return Err(TapeError::NotFound(format!(
                "prefetched block mapped at logical position {}, expected {}",
                read.logical_start, position
            )));
        }
        if read.logical_len != fixed_block_size {
            return Err(TapeError::FixedBlockSizeMismatch {
                expected: fixed_block_size,
                actual: read.logical_len,
            });
        }
        state.current_position = state
            .current_position
            .saturating_add(read.logical_len as u64);
        state.command_counters.read_ops = state.command_counters.read_ops.saturating_add(1);
        state
            .record_read_usage(read.logical_len as u64)
            .map_err(StorageError::from)?;
        schedule_read_prefetch(
            &layout,
            state.current_position,
            state.eod_position,
            fixed_block_size as u64,
        );
        return Ok(read.payload);
    }

    let mut out = Vec::with_capacity(transfer_blocks.saturating_mul(block_size));
    for _ in 0..transfer_blocks {
        let position = state.current_position;
        let read = match take_read_prefetch(&layout, position)? {
            Some(read) => read,
            None => read_logical_block(&layout, position)?.ok_or_else(|| {
                TapeError::NotFound(format!("no block mapped at logical position {}", position))
            })?,
        };
        if read.logical_start != position {
            invalidate_read_prefetch(&layout.root);
            return Err(TapeError::NotFound(format!(
                "prefetched block mapped at logical position {}, expected {}",
                read.logical_start, position
            )));
        }
        if read.logical_len != fixed_block_size {
            return Err(TapeError::FixedBlockSizeMismatch {
                expected: fixed_block_size,
                actual: read.logical_len,
            });
        }
        state.current_position = state
            .current_position
            .saturating_add(read.logical_len as u64);
        state.command_counters.read_ops = state.command_counters.read_ops.saturating_add(1);
        state
            .record_read_usage(read.logical_len as u64)
            .map_err(StorageError::from)?;
        out.extend_from_slice(&read.payload);
        schedule_read_prefetch(
            &layout,
            state.current_position,
            state.eod_position,
            fixed_block_size as u64,
        );
    }
    Ok(out)
}

pub fn write_filemarks(
    state: &mut TapeState,
    count: u32,
    initiator: Option<&str>,
) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    invalidate_state_read_prefetch(state);
    if count == 0 {
        state.command_counters.filemark_ops = state.command_counters.filemark_ops.saturating_add(1);
        state.record_filemark_usage(0).map_err(StorageError::from)?;
        return Ok(());
    }
    enforce_protected_access(state, initiator.unwrap_or(DEFAULT_INITIATOR))?;
    enforce_worm_append_only(state)?;

    let step = logical_position_step(state);
    ensure_capacity_available(state, step.saturating_mul(count as u64))?;
    for _ in 0..count {
        state.filemarks.push(state.current_position);
        state.current_position = state.current_position.saturating_add(step);
    }
    if state.current_position > state.eod_position {
        state.eod_position = state.current_position;
    }
    if let Some(layout) = state.active_layout.as_ref() {
        flush_pending_writes(layout)?;
        persist_filemarks(&layout.root, &state.filemarks)?;
    }
    state.command_counters.filemark_ops = state.command_counters.filemark_ops.saturating_add(1);
    state
        .record_filemark_usage(count as u64)
        .map_err(StorageError::from)?;
    Ok(())
}

pub fn space(state: &mut TapeState, branch: SpaceBranch, count: i64) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    invalidate_state_read_prefetch(state);
    state.command_counters.space_ops = state.command_counters.space_ops.saturating_add(1);

    match branch {
        SpaceBranch::Blocks => {
            if count == 0 {
                return Ok(());
            }
            if state.block_starts.is_empty() {
                return if count > 0 {
                    Err(TapeError::NotFound(
                        "SPACE blocks encountered EOD before any recorded block".to_string(),
                    ))
                } else {
                    Err(TapeError::OutOfRange("SPACE blocks before BOT".to_string()))
                };
            }
            let mut position = state.current_position;
            if count > 0 {
                for _ in 0..count {
                    position = next_point(&state.block_starts, position).ok_or_else(|| {
                        TapeError::NotFound("SPACE blocks encountered EOD".to_string())
                    })?;
                }
            } else {
                for _ in 0..(-count) {
                    position = previous_point(&state.block_starts, position).ok_or_else(|| {
                        TapeError::OutOfRange("SPACE blocks before BOT".to_string())
                    })?;
                }
            }
            state.current_position = position;
            Ok(())
        }
        SpaceBranch::Filemarks => {
            if count == 0 {
                return Ok(());
            }
            if state.filemarks.is_empty() {
                return Err(TapeError::OutOfRange(
                    "SPACE filemarks has no filemarks".to_string(),
                ));
            }
            let marks = sorted_points(&state.filemarks);
            let step = logical_position_step(state);
            let mut position = state.current_position;
            if count > 0 {
                for _ in 0..count {
                    let mark = marks
                        .iter()
                        .copied()
                        .find(|point| *point >= position)
                        .ok_or_else(|| {
                            TapeError::OutOfRange("SPACE filemarks beyond EOD".to_string())
                        })?;
                    position = mark.saturating_add(step);
                }
            } else {
                for _ in 0..(-count) {
                    let mark = marks
                        .iter()
                        .copied()
                        .rev()
                        .find(|point| *point < position)
                        .ok_or_else(|| {
                            TapeError::OutOfRange("SPACE filemarks before BOT".to_string())
                        })?;
                    position = mark;
                }
            }
            state.current_position = position;
            Ok(())
        }
        SpaceBranch::EndOfData => match count {
            0 | 1 => {
                state.current_position = state.eod_position;
                Ok(())
            }
            -1 => {
                state.current_position = 0;
                Ok(())
            }
            _ => Err(TapeError::OutOfRange(format!(
                "SPACE EOD count {count} out of supported range"
            ))),
        },
    }
}

pub fn mode_sense(state: &mut TapeState, page_code: u8) -> Result<Vec<u8>, TapeError> {
    ensure_loaded(state)?;
    state.command_counters.mode_sense_ops = state.command_counters.mode_sense_ops.saturating_add(1);

    match page_code {
        0x00 => Ok(vec![0x00, 0x02, 0x10, 0x11]),
        0x10 => {
            let mode_byte = if state.block_mode.mode == BlockMode::Fixed {
                0x01
            } else {
                0x00
            };
            let mut payload = vec![0x10, 0x06, mode_byte, 0x00];
            payload.extend_from_slice(&state.block_mode.fixed_block_size.to_be_bytes());
            Ok(payload)
        }
        0x11 => {
            let mut payload = vec![0x11, 0x11];
            payload.extend_from_slice(&state.current_position.to_be_bytes());
            payload.extend_from_slice(&state.eod_position.to_be_bytes());
            payload.push(if early_warning_active(state) { 1 } else { 0 });
            Ok(payload)
        }
        _ => {
            state.command_counters.unsupported_mode_pages = state
                .command_counters
                .unsupported_mode_pages
                .saturating_add(1);
            Err(TapeError::UnsupportedModePage(page_code))
        }
    }
}

pub fn log_sense(state: &mut TapeState, page_code: u8) -> Result<Vec<u8>, TapeError> {
    ensure_loaded(state)?;
    state.command_counters.log_sense_ops = state.command_counters.log_sense_ops.saturating_add(1);

    match page_code {
        0x00 => Ok(vec![0x00, 0x01, 0x31]),
        0x31 => {
            let mut payload = vec![0x31, 0x40];
            payload.extend_from_slice(&state.command_counters.write_ops.to_be_bytes());
            payload.extend_from_slice(&state.command_counters.read_ops.to_be_bytes());
            payload.extend_from_slice(&state.command_counters.filemark_ops.to_be_bytes());
            payload.extend_from_slice(&state.command_counters.space_ops.to_be_bytes());
            payload.extend_from_slice(&state.command_counters.mode_sense_ops.to_be_bytes());
            payload.extend_from_slice(&state.command_counters.log_sense_ops.to_be_bytes());
            payload.extend_from_slice(&state.command_counters.unsupported_mode_pages.to_be_bytes());
            payload.extend_from_slice(&state.command_counters.unsupported_log_pages.to_be_bytes());
            Ok(payload)
        }
        _ => {
            state.command_counters.unsupported_log_pages = state
                .command_counters
                .unsupported_log_pages
                .saturating_add(1);
            Err(TapeError::UnsupportedLogPage(page_code))
        }
    }
}

pub fn locate(state: &mut TapeState, target_position: u64) -> Result<(), TapeError> {
    ensure_loaded(state)?;
    invalidate_state_read_prefetch(state);
    if target_position > state.eod_position {
        return Err(TapeError::OutOfRange(format!(
            "locate target {} beyond eod {}",
            target_position, state.eod_position
        )));
    }
    if !is_addressable_position(state, target_position) {
        return Err(TapeError::OutOfRange(format!(
            "locate target {} is not an addressable position",
            target_position
        )));
    }

    state.current_position = target_position;
    Ok(())
}

pub fn read_position(state: &TapeState) -> Result<PositionReport, TapeError> {
    if state.mount_state == MountState::Empty {
        return Err(TapeError::NotReady("no media loaded".to_string()));
    }

    let next_filemark = state
        .filemarks
        .iter()
        .copied()
        .filter(|mark| *mark >= state.current_position)
        .min();
    let file_number = if state.filemarks.is_empty() {
        0
    } else {
        state
            .filemarks
            .iter()
            .copied()
            .filter(|mark| *mark < state.current_position)
            .count() as u64
    };

    Ok(PositionReport {
        current_position: state.current_position,
        eod_position: state.eod_position,
        file_number,
        set_number: 0,
        filemark_count: validate_payload_len(state.filemarks.len())?,
        next_filemark,
        early_warning: early_warning_active(state),
    })
}

pub(crate) fn validate_payload_len(len: usize) -> Result<u32, TapeError> {
    u32::try_from(len).map_err(|_| {
        TapeError::InvalidArgument(format!(
            "payload too large for u32 length field: {len} bytes"
        ))
    })
}

fn ensure_loaded(state: &TapeState) -> Result<(), TapeError> {
    if state.mount_state == MountState::Empty {
        return Err(TapeError::NotReady("no media loaded".to_string()));
    }
    Ok(())
}

fn enforce_protected_access(state: &TapeState, initiator: &str) -> Result<(), TapeError> {
    ensure_reservation_access(state, initiator)?;
    if state.retention_policy.is_worm_media && state.retention_policy.retention_locked {
        enforce_write(RetentionState::Locked)?;
    }
    Ok(())
}

fn enforce_worm_append_only(state: &TapeState) -> Result<(), TapeError> {
    if state.retention_policy.is_worm_media && state.current_position < state.eod_position {
        return Err(TapeError::WormWriteProtected);
    }
    Ok(())
}

fn enforce_worm_erase_allowed(state: &TapeState) -> Result<(), TapeError> {
    if state.retention_policy.is_worm_media
        && (state.eod_position > 0 || !state.block_starts.is_empty() || !state.filemarks.is_empty())
    {
        return Err(TapeError::WormWriteProtected);
    }
    Ok(())
}

fn record_block(state: &mut TapeState, start: u64, length: u32) {
    if !state.block_lengths.contains_key(&start) {
        state.block_starts.push(start);
        state.block_starts.sort_unstable();
    }
    state.block_lengths.insert(start, length);
}

fn next_point(points: &[u64], current: u64) -> Option<u64> {
    points.iter().copied().find(|point| *point > current)
}

fn previous_point(points: &[u64], current: u64) -> Option<u64> {
    points.iter().copied().rev().find(|point| *point < current)
}

fn sorted_points(points: &[u64]) -> Vec<u64> {
    let mut sorted = points.to_vec();
    sorted.sort_unstable();
    sorted
}

fn logical_position_step(state: &TapeState) -> u64 {
    if state.block_mode.mode == BlockMode::Fixed && state.block_mode.fixed_block_size > 0 {
        state.block_mode.fixed_block_size as u64
    } else {
        1
    }
}

fn active_partition_capacity_limit(state: &TapeState) -> Option<u64> {
    let partition_count = usize::from(state.partition_runtime.addl_partitions_defined)
        .saturating_add(1)
        .clamp(1, 4);
    let active_idx = usize::from(state.partition_runtime.active_partition).min(partition_count - 1);
    let configured = state.partition_runtime.partition_sizes_bytes[active_idx];
    match (state.cartridge_capacity_bytes, configured) {
        (Some(capacity), 0) => Some(capacity),
        (Some(capacity), configured) => Some(capacity.min(configured)),
        (None, 0) => None,
        (None, configured) => Some(configured),
    }
}

fn ensure_capacity_available(state: &TapeState, bytes_to_advance: u64) -> Result<(), TapeError> {
    let Some(capacity_limit) = active_partition_capacity_limit(state) else {
        return Ok(());
    };
    let projected = state.current_position.saturating_add(bytes_to_advance);
    if projected > capacity_limit {
        return Err(TapeError::VolumeOverflow);
    }
    Ok(())
}

fn early_warning_active(state: &TapeState) -> bool {
    let partition_count = usize::from(state.partition_runtime.addl_partitions_defined)
        .saturating_add(1)
        .clamp(1, 4);
    let active_idx = usize::from(state.partition_runtime.active_partition).min(partition_count - 1);
    let capacity = state.partition_runtime.partition_sizes_bytes[active_idx];
    if capacity == 0 {
        return false;
    }
    let window = state.early_warning_window.max(1);
    state.current_position >= capacity.saturating_sub(window)
}

fn is_addressable_position(state: &TapeState, target_position: u64) -> bool {
    if target_position > state.eod_position {
        return false;
    }

    if state.block_mode.mode == BlockMode::Variable {
        return true;
    }

    if target_position == 0
        || target_position == state.eod_position
        || state.block_lengths.contains_key(&target_position)
        || state.filemarks.contains(&target_position)
    {
        return true;
    }

    if state.block_mode.fixed_block_size > 0 {
        let block = state.block_mode.fixed_block_size as u64;
        return target_position.is_multiple_of(block);
    }
    false
}
