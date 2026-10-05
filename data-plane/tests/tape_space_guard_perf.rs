use std::fs;
use std::path::PathBuf;
use std::sync::{Arc, Barrier};
use std::time::Instant;

use data_plane::scsi_tape::commands_core::{execute, CoreCommand};
use data_plane::scsi_tape::state::TapeState;

const WRITE_COUNT: usize = 8;
const BLOCK_BYTES: usize = 8 * 1024 * 1024;
const ROUNDS: usize = 5;

#[test]
#[ignore = "bounded comparison benchmark; run with tests/performance/tape_space_guard.sh"]
fn tape_write_path_benchmark() {
    let base = PathBuf::from(
        std::env::var_os("HOLO_TAPE_BENCH_ROOT").expect("benchmark harness must set root"),
    );
    assert!(!base.exists(), "benchmark root must be new and disposable");
    std::env::set_var("HOLO_TAPE_COMPRESSION_ENABLED", "0");

    let mut samples = Vec::with_capacity(ROUNDS);
    for round in 0..ROUNDS {
        let root = base.join(format!("round-{round}"));
        let storage_root = root.join("storage");
        let runtime_dir = root.join("run");
        let media_state_dir = root.join("media-state");
        fs::create_dir_all(&storage_root).expect("create isolated storage root");
        fs::create_dir_all(&runtime_dir).expect("create isolated runtime directory");
        fs::create_dir_all(&media_state_dir).expect("create isolated media state directory");
        std::env::set_var("HOLO_STORAGE_ROOT", &storage_root);
        std::env::set_var("HOLO_RUN_DIR", &runtime_dir);
        std::env::set_var("HOLO_MEDIA_STATE_DIR", &media_state_dir);
        std::env::set_var("HOLO_MEDIA_STATE_KEY", "library-bench__drive-bench");

        let mut state = TapeState::new(format!("drive-bench-{round}"));
        execute(
            &mut state,
            CoreCommand::Load {
                cartridge_id: format!("cart-bench-{round}"),
            },
        )
        .expect("load benchmark cartridge");
        execute(&mut state, CoreCommand::SetBlockModeVariable).expect("select variable mode");
        let started = Instant::now();
        for index in 0..WRITE_COUNT {
            execute(
                &mut state,
                CoreCommand::WriteData {
                    payload: benchmark_payload(index),
                },
            )
            .expect("write benchmark block");
        }
        let elapsed = started.elapsed();
        execute(&mut state, CoreCommand::Unload).expect("unload benchmark cartridge");
        let bytes = WRITE_COUNT * BLOCK_BYTES;
        samples.push(elapsed.as_nanos());
        println!(
            "TAPE_SPACE_GUARD_BENCH round={round} ops={WRITE_COUNT} bytes={bytes} elapsed_ns={}",
            elapsed.as_nanos()
        );
        drop(state);
        fs::remove_dir_all(root).expect("remove this round's isolated data");
    }
    println!("TAPE_SPACE_GUARD_BENCH_SAMPLES_NS={samples:?}");
}

#[test]
#[ignore = "bounded dual-drive comparison benchmark; run with tests/performance/tape_space_guard.sh"]
fn tape_dual_write_path_benchmark() {
    let base = PathBuf::from(
        std::env::var_os("HOLO_TAPE_BENCH_ROOT").expect("benchmark harness must set root"),
    );
    assert!(!base.exists(), "benchmark root must be new and disposable");
    std::env::set_var("HOLO_TAPE_COMPRESSION_ENABLED", "0");

    for round in 0..ROUNDS {
        let root = base.join(format!("round-{round}"));
        let storage_root = root.join("storage");
        let runtime_dir = root.join("run");
        let media_state_dir = root.join("media-state");
        fs::create_dir_all(&storage_root).expect("create isolated storage root");
        fs::create_dir_all(&runtime_dir).expect("create isolated runtime directory");
        fs::create_dir_all(&media_state_dir).expect("create isolated media state directory");
        std::env::set_var("HOLO_STORAGE_ROOT", &storage_root);
        std::env::set_var("HOLO_RUN_DIR", &runtime_dir);
        std::env::set_var("HOLO_MEDIA_STATE_DIR", &media_state_dir);
        std::env::set_var("HOLO_MEDIA_STATE_KEY", "library-bench__drive-bench");

        let gate = Arc::new(Barrier::new(3));
        let elapsed = std::thread::scope(|scope| {
            let first_gate = Arc::clone(&gate);
            let first = scope
                .spawn(move || benchmark_drive("drive-bench-1", "cart-bench-1", first_gate, 0));
            let second_gate = Arc::clone(&gate);
            let second = scope.spawn(move || {
                benchmark_drive("drive-bench-2", "cart-bench-2", second_gate, WRITE_COUNT)
            });
            gate.wait();
            let started = Instant::now();
            first.join().expect("first drive benchmark");
            second.join().expect("second drive benchmark");
            started.elapsed()
        });
        let bytes = 2 * WRITE_COUNT * BLOCK_BYTES;
        println!(
            "TAPE_SPACE_GUARD_DUAL_BENCH round={round} ops={} bytes={bytes} elapsed_ns={}",
            2 * WRITE_COUNT,
            elapsed.as_nanos()
        );
        fs::remove_dir_all(root).expect("remove this round's isolated data");
    }
}

fn benchmark_drive(drive_id: &str, cartridge_id: &str, gate: Arc<Barrier>, payload_offset: usize) {
    let mut state = TapeState::new(drive_id);
    execute(
        &mut state,
        CoreCommand::Load {
            cartridge_id: cartridge_id.to_string(),
        },
    )
    .expect("load benchmark cartridge");
    execute(&mut state, CoreCommand::SetBlockModeVariable).expect("select variable mode");
    gate.wait();
    for index in 0..WRITE_COUNT {
        execute(
            &mut state,
            CoreCommand::WriteData {
                payload: benchmark_payload(payload_offset + index),
            },
        )
        .expect("write benchmark block");
    }
    execute(&mut state, CoreCommand::Unload).expect("unload benchmark cartridge");
}

fn benchmark_payload(index: usize) -> Vec<u8> {
    let mut value = 0x9e37_79b9_7f4a_7c15u64 ^ index as u64;
    (0..BLOCK_BYTES)
        .map(|_| {
            value ^= value << 13;
            value ^= value >> 7;
            value ^= value << 17;
            value as u8
        })
        .collect()
}
