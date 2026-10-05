use crate::scsi_tape::error::TapeError;
use crate::scsi_tape::state::{MountState, TapeState};
use crate::storage::layout_lease::LayoutLease;
use crate::storage::{
    bootstrap_for_mount_with_lease, load_blk_map_records, load_filemarks, load_retention_state,
    recover_dirty_state, storage_root_dir_for_cartridge, BlkMapState,
};

pub fn attach_cartridge(state: &mut TapeState, cartridge_id: &str) -> Result<(), TapeError> {
    if state.mount_state != MountState::Empty {
        return Err(TapeError::InvalidTransition);
    }
    let root = storage_root_dir_for_cartridge(cartridge_id);
    let library_id = std::env::var("HOLO_LAYOUT_LIBRARY_ID")
        .ok()
        .filter(|library| !library.trim().is_empty())
        .unwrap_or_else(|| state.drive_id.clone());
    let lease = LayoutLease::acquire(
        &root,
        cartridge_id,
        &crate::storage::layout_lease::storage_runtime_dir(),
    )
    .map_err(crate::storage::layout_lease::storage_error_from_lease)?;
    let snapshot =
        bootstrap_for_mount_with_lease(&library_id, &state.drive_id, cartridge_id, &lease)?;
    let mut staged = state.clone();
    staged.mount(cartridge_id.to_string(), snapshot.paths);
    staged.layout_lease = Some(lease);
    hydrate_runtime_from_layout(&mut staged)?;
    *state = staged;
    Ok(())
}

pub fn detach_cartridge(state: &mut TapeState) -> Result<(), TapeError> {
    crate::scsi_tape::command_chain::invalidate_state_read_prefetch(state);
    if let Some(layout) = state.active_layout.clone() {
        if !state.requires_recovery {
            crate::storage::flush_pending_writes(&layout)?;
            state
                .persist_usage_counters()
                .map_err(crate::storage::StorageError::Io)?;
        }
        crate::storage::discard_layout_caches_checked(&layout)?;
    }
    state.unmount();
    Ok(())
}

fn hydrate_runtime_from_layout(state: &mut TapeState) -> Result<(), TapeError> {
    let Some(layout) = state.active_layout.as_ref() else {
        return Ok(());
    };
    let _ = recover_dirty_state(layout)?;
    let (_, records) = load_blk_map_records(&layout.blk_map_file)?;
    let mut active_records: Vec<_> = records
        .into_iter()
        .filter(|record| record.state == BlkMapState::Active)
        .collect();
    active_records.sort_by_key(|record| record.logical_start);

    state.current_position = 0;
    state.eod_position = 0;
    state.block_starts.clear();
    state.block_lengths.clear();
    state.filemarks.clear();

    let filemarks = load_filemarks(&layout.root)?;
    if !filemarks.is_empty() {
        state.filemarks = filemarks;
        state.filemarks.sort_unstable();
    }

    if let Some(retention) = load_retention_state(&layout.root)? {
        state.retention_policy.is_worm_media = retention.is_worm_media;
        state.retention_policy.retention_locked = retention.retention_locked;
    }

    let mut observed_filemarks = 0u32;
    let filemark_step = runtime_filemark_step(state);

    for record in active_records {
        if state.filemarks.is_empty() {
            let missing_filemarks = record.filemark_count.saturating_sub(observed_filemarks);
            if missing_filemarks > 0 {
                let span = (missing_filemarks as u64).saturating_mul(filemark_step);
                let mut mark = record.logical_start.saturating_sub(span);
                for _ in 0..missing_filemarks {
                    state.filemarks.push(mark);
                    mark = mark.saturating_add(filemark_step);
                }
                observed_filemarks = record.filemark_count;
            }
        }
        if !state.block_lengths.contains_key(&record.logical_start) {
            state.block_starts.push(record.logical_start);
        }
        state
            .block_lengths
            .insert(record.logical_start, record.logical_len);
        state.eod_position = state.eod_position.max(record.logical_end());
    }
    if let Some(last_filemark) = state.filemarks.iter().copied().max() {
        state.eod_position = state
            .eod_position
            .max(last_filemark.saturating_add(filemark_step));
    }
    state.block_starts.sort_unstable();
    Ok(())
}

fn runtime_filemark_step(state: &TapeState) -> u64 {
    if state.block_mode.mode == crate::scsi_tape::state::BlockMode::Fixed
        && state.block_mode.fixed_block_size > 0
    {
        state.block_mode.fixed_block_size as u64
    } else {
        1
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::storage::{
        bootstrap_for_mount, load_filemarks, persist_filemarks, persist_retention_state,
        write_logical_block, WriteOptions,
    };

    #[test]
    fn attach_cartridge_rebuilds_eod_and_block_index() {
        let drive_id = format!("drive-hydrate-{}", std::process::id());
        let cartridge_id = "CAR-HYDRATE-001";
        let snapshot = bootstrap_for_mount(&drive_id, &drive_id, cartridge_id).expect("bootstrap");
        write_logical_block(
            &snapshot.paths,
            0,
            b"header-block",
            0,
            WriteOptions::default(),
            None,
        )
        .expect("write block 0");
        write_logical_block(
            &snapshot.paths,
            262_144,
            b"next-block",
            0,
            WriteOptions::default(),
            None,
        )
        .expect("write block 1");

        let mut state = TapeState::new(&drive_id);
        attach_cartridge(&mut state, cartridge_id).expect("attach");

        assert_eq!(state.mount_state, MountState::Loaded);
        assert_eq!(state.current_position, 0);
        assert!(state.eod_position >= 262_144 + b"next-block".len() as u64);
        assert_eq!(state.block_starts, vec![0, 262_144]);
        assert_eq!(state.block_lengths.get(&0).copied(), Some(12));
        assert_eq!(state.block_lengths.get(&262_144).copied(), Some(10));
    }

    #[test]
    fn attach_cartridge_rebuilds_filemark_positions_from_blk_map() {
        let drive_id = format!("drive-hydrate-filemarks-{}", std::process::id());
        let cartridge_id = "CAR-HYDRATE-FM-001";
        let snapshot = bootstrap_for_mount(&drive_id, &drive_id, cartridge_id).expect("bootstrap");
        let payload_a = vec![0x41u8; 262_144];
        let payload_b = vec![0x42u8; 262_144];

        write_logical_block(
            &snapshot.paths,
            0,
            &payload_a,
            0,
            WriteOptions::default(),
            None,
        )
        .expect("write first block");
        write_logical_block(
            &snapshot.paths,
            524_288,
            &payload_b,
            1,
            WriteOptions::default(),
            None,
        )
        .expect("write second block with one filemark before");

        let mut state = TapeState::new(&drive_id);
        attach_cartridge(&mut state, cartridge_id).expect("attach");

        assert_eq!(state.filemarks, vec![262_144]);
        assert_eq!(state.block_starts, vec![0, 524_288]);
        assert_eq!(state.eod_position, 786_432);
    }

    #[test]
    fn attach_cartridge_prefers_persisted_filemarks_and_retention() {
        let drive_id = format!("drive-hydrate-runtime-state-{}", std::process::id());
        let cartridge_id = "CAR-HYDRATE-STATE-001";
        let snapshot = bootstrap_for_mount(&drive_id, &drive_id, cartridge_id).expect("bootstrap");
        persist_filemarks(&snapshot.paths.root, &[9, 42]).expect("persist filemarks");
        persist_retention_state(&snapshot.paths.root, true, true).expect("persist retention");

        let mut state = TapeState::new(&drive_id);
        attach_cartridge(&mut state, cartridge_id).expect("attach");

        assert_eq!(state.filemarks, vec![9, 42]);
        assert!(state.retention_policy.is_worm_media);
        assert!(state.retention_policy.retention_locked);
        let filemarks = load_filemarks(&snapshot.paths.root).expect("load filemarks");
        assert_eq!(filemarks, vec![9, 42]);
    }

    #[test]
    fn attach_cartridge_restores_eod_after_trailing_filemark() {
        let drive_id = format!("drive-hydrate-trailing-filemark-{}", std::process::id());
        let cartridge_id = "CAR-HYDRATE-TRAILING-FM-001";
        let snapshot = bootstrap_for_mount(&drive_id, &drive_id, cartridge_id).expect("bootstrap");
        let payload = vec![0x45u8; 262_144];

        write_logical_block(
            &snapshot.paths,
            0,
            &payload,
            0,
            WriteOptions::default(),
            None,
        )
        .expect("write last catalog block");
        persist_filemarks(&snapshot.paths.root, &[262_144]).expect("persist trailing filemark");

        let mut state = TapeState::new(&drive_id);
        attach_cartridge(&mut state, cartridge_id).expect("attach");

        assert_eq!(state.block_starts, vec![0]);
        assert_eq!(state.filemarks, vec![262_144]);
        assert_eq!(state.eod_position, 524_288);
    }
}
