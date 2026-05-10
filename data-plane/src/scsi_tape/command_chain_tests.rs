use super::command_chain::{
    clear_read_prefetch_degradation_for_test, fail_next_read_prefetch_invalidation_for_test,
    read_fixed_blocks, read_prefetch_degradation_status_for_test, rewind_media,
    validate_payload_len, write_fixed_blocks, EraseMode,
};
use super::commands_core::{execute, execute_with_sense, CoreCommand, CoreResponse};
use super::error::TapeError;
use super::state::TapeState;
use crate::storage::{
    current_checkpoint, current_dedup_refcounts, data_segment_path, read_logical_block,
    write_segment_file, CheckpointFlags, CompressionCodec, SegmentKind,
};
use std::fs;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;

static NEXT_STATE_ID: AtomicU64 = AtomicU64::new(1);
static ENV_LOCK: Mutex<()> = Mutex::new(());

fn new_state(case: &str) -> TapeState {
    let id = NEXT_STATE_ID.fetch_add(1, Ordering::Relaxed);
    TapeState::new(format!("drive-{case}-{id}"))
}

fn cleanup(state: &TapeState) {
    if let Some(layout) = &state.active_layout {
        let _ = std::fs::remove_dir_all(&layout.root);
    }
}

#[test]
fn media_lifecycle_chain_load_rewind_erase_unload() {
    let mut state = new_state("lifecycle");
    let cartridge_id = format!("cart-{}", state.drive_id);

    execute(&mut state, CoreCommand::Load { cartridge_id }).expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("variable mode");
    assert!(state.active_layout.is_some());

    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"abcdef".to_vec(),
        },
    )
    .expect("write should pass");
    assert_eq!(state.current_position, 6);

    execute(&mut state, CoreCommand::Rewind).expect("rewind should pass");
    assert_eq!(state.current_position, 0);

    execute(
        &mut state,
        CoreCommand::Erase {
            mode: EraseMode::Short,
        },
    )
    .expect("erase should pass");
    assert_eq!(state.current_position, 0);
    assert_eq!(state.eod_position, 0);
    assert!(state.filemarks.is_empty());

    execute(&mut state, CoreCommand::Unload).expect("unload should pass");
    assert!(state.active_layout.is_none());

    cleanup(&state);
}

#[test]
fn erase_clears_cached_layout_before_rewrite_at_bot() {
    let mut state = new_state("erase-rewrite-bot");

    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-erase-rewrite-bot".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeFixed { block_size: 4 }).expect("fixed mode");

    write_fixed_blocks(&mut state, b"ABCD", 4, None).expect("initial write should pass");
    rewind_media(&mut state).expect("rewind should pass");

    execute(
        &mut state,
        CoreCommand::Erase {
            mode: EraseMode::Short,
        },
    )
    .expect("erase should pass");
    execute(&mut state, CoreCommand::SetBlockModeFixed { block_size: 4 }).expect("fixed mode");

    write_fixed_blocks(&mut state, b"WXYZ", 4, None).expect("rewrite after erase should pass");
    rewind_media(&mut state).expect("rewind should pass");

    let read_back = read_fixed_blocks(&mut state, 4, 1).expect("read after erase rewrite");
    assert_eq!(read_back, b"WXYZ");

    cleanup(&state);
}

#[test]
fn fixed_block_read_prefetch_preserves_sequential_payloads() {
    let _guard = ENV_LOCK.lock().expect("env lock");
    std::env::set_var("HOLO_READ_PREFETCH", "1");
    std::env::set_var("HOLO_READ_PREFETCH_DEPTH", "2");

    let mut state = new_state("read-prefetch");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-read-prefetch".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeFixed { block_size: 4 }).expect("fixed mode");

    write_fixed_blocks(&mut state, b"aaaabbbbcccc", 4, None).expect("write fixed blocks");
    rewind_media(&mut state).expect("rewind should pass");

    let first = read_fixed_blocks(&mut state, 4, 1).expect("read first block");
    let second = read_fixed_blocks(&mut state, 4, 1).expect("read second block");
    let third = read_fixed_blocks(&mut state, 4, 1).expect("read third block");
    assert_eq!(first, b"aaaa");
    assert_eq!(second, b"bbbb");
    assert_eq!(third, b"cccc");

    std::env::remove_var("HOLO_READ_PREFETCH");
    std::env::remove_var("HOLO_READ_PREFETCH_DEPTH");
    cleanup(&state);
}

#[test]
fn fixed_block_write_succeeds_when_prefetch_invalidation_degrades() {
    let _guard = ENV_LOCK.lock().expect("env lock");
    std::env::set_var("HOLO_READ_PREFETCH", "1");
    std::env::set_var("HOLO_READ_PREFETCH_DEPTH", "2");

    let mut state = new_state("prefetch-invalidation-success");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-prefetch-invalidation-success".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeFixed { block_size: 4 }).expect("fixed mode");

    write_fixed_blocks(&mut state, b"aaaabbbb", 4, None).expect("initial write fixed blocks");
    rewind_media(&mut state).expect("rewind should pass");
    let first = read_fixed_blocks(&mut state, 4, 1).expect("read first block");
    assert_eq!(first, b"aaaa");

    let layout = state.active_layout.clone().expect("layout should be active");
    fail_next_read_prefetch_invalidation_for_test();
    write_fixed_blocks(&mut state, b"ZZZZ", 4, None)
        .expect("committed write should not fail on prefetch invalidation");

    let status = read_prefetch_degradation_status_for_test(&layout.root)
        .expect("prefetch degradation should be recorded");
    assert_eq!(status.invalidation_failures, 1);
    assert!(status.bypass_reads);

    clear_read_prefetch_degradation_for_test(&layout.root);
    std::env::remove_var("HOLO_READ_PREFETCH");
    std::env::remove_var("HOLO_READ_PREFETCH_DEPTH");
    cleanup(&state);
}

#[test]
fn prefetch_degradation_bypasses_stale_cached_read() {
    let _guard = ENV_LOCK.lock().expect("env lock");
    std::env::set_var("HOLO_READ_PREFETCH", "1");
    std::env::set_var("HOLO_READ_PREFETCH_DEPTH", "2");

    let mut state = new_state("prefetch-bypass-stale");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-prefetch-bypass-stale".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeFixed { block_size: 4 }).expect("fixed mode");

    write_fixed_blocks(&mut state, b"aaaabbbb", 4, None).expect("initial write fixed blocks");
    rewind_media(&mut state).expect("rewind should pass");
    let first = read_fixed_blocks(&mut state, 4, 1).expect("read first block");
    assert_eq!(first, b"aaaa");

    let layout = state.active_layout.clone().expect("layout should be active");
    fail_next_read_prefetch_invalidation_for_test();
    write_fixed_blocks(&mut state, b"ZZZZ", 4, None).expect("overwrite second block");

    state.current_position = 4;
    let read_back = read_fixed_blocks(&mut state, 4, 1)
        .expect("degraded prefetch should bypass stale queued read");
    assert_eq!(read_back, b"ZZZZ");

    clear_read_prefetch_degradation_for_test(&layout.root);
    std::env::remove_var("HOLO_READ_PREFETCH");
    std::env::remove_var("HOLO_READ_PREFETCH_DEPTH");
    cleanup(&state);
}

#[test]
fn rewind_flushes_pending_throughput_writes() {
    let mut state = new_state("rewind-flush");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-rewind-flush".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeFixed { block_size: 4 }).expect("fixed mode");

    write_fixed_blocks(&mut state, b"aaaa", 4, None).expect("write fixed block");
    let layout = state
        .active_layout
        .clone()
        .expect("layout should be active");
    let before = current_checkpoint(&layout).expect("checkpoint before rewind");
    assert_eq!(before.flags, CheckpointFlags::Dirty);

    rewind_media(&mut state).expect("rewind should flush pending writes");

    let after = current_checkpoint(&layout).expect("checkpoint after rewind");
    assert_eq!(after.flags, CheckpointFlags::Clean);
    cleanup(&state);
}

#[test]
fn tape_writes_use_lz4_and_dedup_when_dce_enabled() {
    let mut state = new_state("dce-compress-dedup");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-dce-compress-dedup".to_string(),
        },
    )
    .expect("load should pass");
    execute(
        &mut state,
        CoreCommand::SetBlockModeFixed { block_size: 1024 },
    )
    .expect("fixed mode");

    let payload = vec![0x41; 2048];
    write_fixed_blocks(&mut state, &payload, 1024, None).expect("write fixed blocks");

    let layout = state
        .active_layout
        .clone()
        .expect("layout should be active");
    let first = read_logical_block(&layout, 0)
        .expect("read first")
        .expect("first block should exist");
    let second = read_logical_block(&layout, 1024)
        .expect("read second")
        .expect("second block should exist");
    assert_eq!(first.codec_used, CompressionCodec::Lz4);
    assert_eq!(second.codec_used, CompressionCodec::Lz4);
    assert_eq!(first.dedup_entry_id, second.dedup_entry_id);
    assert_eq!(
        current_dedup_refcounts(&layout).expect("dedup refs"),
        vec![(1, 2)]
    );

    cleanup(&state);
}

#[test]
fn tape_writes_keep_compression_when_algorithm_select_is_clear() {
    let mut state = new_state("sdca-clear-compress");
    state.select_data_compression_algorithm = false;
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-sdca-clear-compress".to_string(),
        },
    )
    .expect("load should pass");
    assert!(state.data_compression_enabled);
    assert!(!state.select_data_compression_algorithm);
    execute(
        &mut state,
        CoreCommand::SetBlockModeFixed { block_size: 1024 },
    )
    .expect("fixed mode");

    write_fixed_blocks(&mut state, &[0x42; 1024], 1024, None).expect("write fixed block");

    let layout = state
        .active_layout
        .clone()
        .expect("layout should be active");
    let block = read_logical_block(&layout, 0)
        .expect("read block")
        .expect("block should exist");
    assert_eq!(block.codec_used, CompressionCodec::Lz4);

    cleanup(&state);
}

#[test]
fn tape_writes_disable_compression_but_keep_dedup_when_dce_disabled() {
    let mut state = new_state("dce-off-dedup");
    state.data_compression_enabled = false;
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-dce-off-dedup".to_string(),
        },
    )
    .expect("load should pass");
    assert!(!state.data_compression_enabled);
    execute(
        &mut state,
        CoreCommand::SetBlockModeFixed { block_size: 1024 },
    )
    .expect("fixed mode");

    let payload = vec![0x41; 2048];
    write_fixed_blocks(&mut state, &payload, 1024, None).expect("write fixed blocks");

    let layout = state
        .active_layout
        .clone()
        .expect("layout should be active");
    let first = read_logical_block(&layout, 0)
        .expect("read first")
        .expect("first block should exist");
    let second = read_logical_block(&layout, 1024)
        .expect("read second")
        .expect("second block should exist");
    assert_eq!(first.codec_used, CompressionCodec::None);
    assert_eq!(second.codec_used, CompressionCodec::None);
    assert_eq!(first.dedup_entry_id, second.dedup_entry_id);
    assert_eq!(
        current_dedup_refcounts(&layout).expect("dedup refs"),
        vec![(1, 2)]
    );

    cleanup(&state);
}

#[test]
fn erase_preserves_custom_capacity_and_partition_layout() {
    let mut state = new_state("erase-preserve-capacity");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-erase-preserve-capacity".to_string(),
        },
    )
    .expect("load should pass");

    let custom_capacity = 800 * 1024 * 1024;
    state.cartridge_capacity_bytes = Some(custom_capacity);
    state.partition_runtime.partition_sizes_bytes[0] = custom_capacity;
    state.partition_runtime.partition_size_units[0] = 800;
    state.partition_runtime.partition_units = 20;
    state.retention_policy.is_worm_media = false;
    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("variable mode");

    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"abcd".to_vec(),
        },
    )
    .expect("write before erase should pass");
    execute(
        &mut state,
        CoreCommand::Erase {
            mode: EraseMode::Short,
        },
    )
    .expect("erase should pass");

    assert_eq!(state.cartridge_capacity_bytes, Some(custom_capacity));
    assert_eq!(
        state.partition_runtime.partition_sizes_bytes[0],
        custom_capacity
    );
    assert_eq!(state.partition_runtime.partition_size_units[0], 800);
    assert_eq!(state.partition_runtime.partition_units, 20);
    assert!(!state.retention_policy.is_worm_media);
    assert_eq!(state.current_position, 0);
    assert_eq!(state.eod_position, 0);

    cleanup(&state);
}

#[test]
fn long_erase_purges_layout_artifacts_before_reset() {
    let mut state = new_state("long-erase-purge");

    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-long-erase-purge".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("variable mode");
    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"old-data".to_vec(),
        },
    )
    .expect("write before erase");

    let layout = state.active_layout.clone().expect("layout");
    let orphan = layout.root.join("orphan-before-long-erase.bin");
    fs::write(&orphan, b"stale").expect("write orphan");

    execute(
        &mut state,
        CoreCommand::Erase {
            mode: EraseMode::Long,
        },
    )
    .expect("long erase should pass");

    assert_eq!(state.current_position, 0);
    assert_eq!(state.eod_position, 0);
    assert!(!orphan.exists(), "long erase should purge stale artifacts");
    assert!(
        layout.metadata_file.exists(),
        "layout is recreated after purge"
    );

    cleanup(&state);
}

#[test]
fn variable_write_rejects_volume_overflow_for_custom_capacity() {
    let mut state = new_state("overflow-variable");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-overflow-variable".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("variable mode");

    let custom_capacity = 8u64;
    state.cartridge_capacity_bytes = Some(custom_capacity);
    state.partition_runtime.partition_sizes_bytes[0] = custom_capacity;

    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"1234567".to_vec(),
        },
    )
    .expect("write within capacity should pass");

    let err = execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"89".to_vec(),
        },
    )
    .expect_err("overflow write should fail");
    assert!(matches!(err, TapeError::VolumeOverflow));
    assert_eq!(state.current_position, 7);
    assert_eq!(state.eod_position, 7);

    cleanup(&state);
}

#[test]
fn filemark_write_rejects_volume_overflow_for_custom_capacity() {
    let mut state = new_state("overflow-filemark");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-overflow-filemark".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeFixed { block_size: 4 }).expect("fixed mode");

    let custom_capacity = 8u64;
    state.cartridge_capacity_bytes = Some(custom_capacity);
    state.partition_runtime.partition_sizes_bytes[0] = custom_capacity;
    state.current_position = custom_capacity;
    state.eod_position = custom_capacity;

    let err = execute(&mut state, CoreCommand::WriteFilemarks { count: 1 })
        .expect_err("overflow filemark should fail");
    assert!(matches!(err, TapeError::VolumeOverflow));
    assert_eq!(state.current_position, custom_capacity);
    assert_eq!(state.eod_position, custom_capacity);

    cleanup(&state);
}

#[test]
fn supports_positioned_write_read_filemarks_locate_and_read_position() {
    let mut state = new_state("positioned");

    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-positioned".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("variable mode");

    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"blockA".to_vec(),
        },
    )
    .expect("write block A should pass");
    execute(&mut state, CoreCommand::WriteFilemarks { count: 1 })
        .expect("write filemark should pass");
    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: b"BLOB".to_vec(),
        },
    )
    .expect("write block B should pass");

    let position =
        execute(&mut state, CoreCommand::ReadPosition).expect("read position should pass");
    match position {
        CoreResponse::Position(report) => {
            assert_eq!(report.current_position, 11);
            assert_eq!(report.eod_position, 11);
            assert_eq!(report.file_number, 1);
            assert_eq!(report.set_number, 0);
            assert_eq!(report.filemark_count, 1);
        }
        _ => panic!("unexpected response type"),
    }

    execute(&mut state, CoreCommand::Locate { logical_block: 0 })
        .expect("locate block A should pass");
    let first = execute(&mut state, CoreCommand::ReadData).expect("read block A should pass");
    match first {
        CoreResponse::Data(bytes) => assert_eq!(bytes, b"blockA"),
        _ => panic!("unexpected response type"),
    }

    execute(&mut state, CoreCommand::Locate { logical_block: 7 })
        .expect("locate block B should pass");
    let second = execute(&mut state, CoreCommand::ReadData).expect("read block B should pass");
    match second {
        CoreResponse::Data(bytes) => assert_eq!(bytes, b"BLOB"),
        _ => panic!("unexpected response type"),
    }

    cleanup(&state);
}

#[test]
fn maps_no_media_and_out_of_range_errors_deterministically() {
    let mut state = new_state("errors");

    let no_media = execute_with_sense(&mut state, CoreCommand::ReadData)
        .expect_err("read without media should fail");
    assert_eq!(no_media.sense_key, 0x02);

    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-errors".to_string(),
        },
    )
    .expect("load should pass");

    let out_of_range = execute_with_sense(&mut state, CoreCommand::Locate { logical_block: 2 })
        .expect_err("locate beyond eod should fail");
    assert_eq!(out_of_range.sense_key, 0x05);

    execute(&mut state, CoreCommand::Locate { logical_block: 0 })
        .expect("locate to zero should pass");
    let read_miss = execute_with_sense(&mut state, CoreCommand::ReadData)
        .expect_err("read unmapped should fail");
    assert_eq!(read_miss.sense_key, 0x08);
    assert_eq!(read_miss.asc, 0x00);
    assert_eq!(read_miss.ascq, 0x05);

    cleanup(&state);
}

#[test]
fn fixed_block_filemarks_advance_by_block_size() {
    let mut state = new_state("fixed-filemark-step");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-fixed-filemark-step".to_string(),
        },
    )
    .expect("load should pass");

    let block = vec![0xAAu8; 262_144];
    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: block.clone(),
        },
    )
    .expect("first write should pass");
    execute(&mut state, CoreCommand::WriteFilemarks { count: 1 })
        .expect("write filemark should pass");
    execute(&mut state, CoreCommand::WriteData { payload: block })
        .expect("second write should pass");

    assert_eq!(state.filemarks, vec![262_144]);
    assert_eq!(state.block_starts, vec![0, 524_288]);
    assert_eq!(state.current_position, 786_432);
    assert_eq!(state.eod_position, 786_432);

    execute(&mut state, CoreCommand::Locate { logical_block: 0 }).expect("locate first block");
    execute(&mut state, CoreCommand::SpaceFilemarks { count: 1 }).expect("space +1 filemark");
    assert_eq!(state.current_position, 524_288);

    cleanup(&state);
}

#[test]
fn rewrite_from_bot_truncates_previous_tail_in_fixed_mode() {
    let mut state = new_state("rewrite-bot-truncate");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-rewrite-bot-truncate".to_string(),
        },
    )
    .expect("load should pass");

    let block_a = vec![0xAAu8; 262_144];
    let block_b = vec![0xBBu8; 262_144];

    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: block_a.clone(),
        },
    )
    .expect("first write should pass");
    assert_eq!(state.current_position, 262_144);
    assert_eq!(state.eod_position, 262_144);

    execute(&mut state, CoreCommand::Rewind).expect("rewind should pass");
    assert_eq!(state.current_position, 0);
    assert_eq!(state.eod_position, 262_144);

    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: block_b.clone(),
        },
    )
    .expect("rewrite at BOT should pass and truncate old tail");
    assert_eq!(state.current_position, 262_144);
    assert_eq!(state.eod_position, 262_144);
    assert_eq!(state.block_starts, vec![0]);

    execute(&mut state, CoreCommand::Locate { logical_block: 0 }).expect("locate rewritten block");
    let read = execute(&mut state, CoreCommand::ReadData).expect("read rewritten block");
    match read {
        CoreResponse::Data(bytes) => assert_eq!(bytes, block_b),
        _ => panic!("unexpected response type"),
    }

    cleanup(&state);
}

#[test]
fn rewrite_from_bot_resets_physical_segments() {
    let mut state = new_state("rewrite-bot-reset-segments");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-rewrite-bot-reset-segments".to_string(),
        },
    )
    .expect("load should pass");

    let block_a = vec![0xAAu8; 262_144];
    let block_b = vec![0xBBu8; 262_144];

    for index in 0..4 {
        let payload = if index == 0 {
            block_a.clone()
        } else {
            vec![0xA0u8 + index; 262_144]
        };
        execute(&mut state, CoreCommand::WriteData { payload }).expect("initial write should pass");
    }
    let layout = state
        .active_layout
        .clone()
        .expect("layout should be active");
    let primary_segment = data_segment_path(&layout, 0);
    assert!(primary_segment.exists());
    let first_size = fs::metadata(&primary_segment)
        .expect("initial data segment metadata")
        .len();
    let stale_segment = data_segment_path(&layout, 1);
    write_segment_file(&stale_segment, SegmentKind::Data, 1, 1, &[0xCC; 4096])
        .expect("stale rotated segment");
    assert!(stale_segment.exists());

    execute(&mut state, CoreCommand::Locate { logical_block: 0 }).expect("locate original block");
    let original = execute(&mut state, CoreCommand::ReadData).expect("read original block");
    match original {
        CoreResponse::Data(bytes) => assert_eq!(bytes, block_a),
        _ => panic!("unexpected response type"),
    }

    execute(&mut state, CoreCommand::Rewind).expect("rewind should pass");
    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: block_b.clone(),
        },
    )
    .expect("rewrite should pass");

    assert!(primary_segment.exists());
    let rewritten_size = fs::metadata(&primary_segment)
        .expect("rewritten data segment metadata")
        .len();
    assert!(rewritten_size < first_size);
    assert!(!stale_segment.exists());
    assert_eq!(state.current_position, 262_144);
    assert_eq!(state.eod_position, 262_144);

    execute(&mut state, CoreCommand::Locate { logical_block: 0 }).expect("locate rewritten block");
    let read = execute(&mut state, CoreCommand::ReadData).expect("read rewritten block");
    match read {
        CoreResponse::Data(bytes) => assert_eq!(bytes, block_b),
        _ => panic!("unexpected response type"),
    }

    cleanup(&state);
}

#[test]
fn write_filemarks_zero_is_noop_success() {
    let mut state = new_state("wmf-zero");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-wmf-zero".to_string(),
        },
    )
    .expect("load should pass");

    execute(&mut state, CoreCommand::WriteFilemarks { count: 0 })
        .expect("write filemarks(0) should be accepted as no-op");
    assert!(state.filemarks.is_empty());
    assert_eq!(state.current_position, 0);

    cleanup(&state);
}

#[test]
fn variable_mode_empty_write_is_noop_success() {
    let mut state = new_state("variable-empty-write");
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: "cart-variable-empty".to_string(),
        },
    )
    .expect("load should pass");
    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("switch variable mode");

    execute(
        &mut state,
        CoreCommand::WriteData {
            payload: Vec::new(),
        },
    )
    .expect("variable-mode empty write should succeed");
    assert_eq!(state.current_position, 0);
    assert_eq!(state.eod_position, 0);

    cleanup(&state);
}

#[test]
fn payload_len_validation_rejects_values_over_u32_max() {
    let err = validate_payload_len(usize::MAX).expect_err("usize::MAX must be rejected");
    assert!(
        matches!(err, super::error::TapeError::InvalidArgument(_)),
        "unexpected error type: {err:?}"
    );
}
