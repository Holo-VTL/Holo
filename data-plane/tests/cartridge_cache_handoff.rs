use data_plane::storage::{
    discard_layout_caches, discard_layout_caches_checked, initialize_layout, read_logical_block,
    reset_layout_for_overwrite, run_unmap, write_logical_block, LayoutPaths, WriteOptions,
};
use std::{
    env, fs,
    path::Path,
    process::Command,
    time::{SystemTime, UNIX_EPOCH},
};

fn spawn_role(root: &Path, role: &str, checked: bool, reset: bool) {
    let output = Command::new(env::current_exe().expect("test executable"))
        .args([
            "--exact",
            "trusted_cache_handoff_reads_rewritten_media",
            "--nocapture",
        ])
        .env("HOLO_CACHE_HANDOFF_ROLE", role)
        .env("HOLO_CACHE_HANDOFF_ROOT", root)
        .env(
            "HOLO_CACHE_HANDOFF_CHECKED",
            if checked { "1" } else { "0" },
        )
        .env("HOLO_STORAGE_TRUST_HOT_CACHE", "1")
        .env("HOLO_CACHE_HANDOFF_RESET", if reset { "1" } else { "0" })
        .output()
        .expect("run separate cache owner");
    assert!(
        output.status.success(),
        "{role} failed:\n{}\n{}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
}

fn discard(paths: &LayoutPaths, checked: bool) {
    if checked {
        discard_layout_caches_checked(paths).expect("release cartridge caches");
    } else {
        discard_layout_caches(paths);
    }
}

#[test]
fn trusted_cache_handoff_reads_rewritten_media() {
    let role = env::var("HOLO_CACHE_HANDOFF_ROLE").unwrap_or_default();
    if role.is_empty() {
        let mut failures = 0;
        for checked in [true, false] {
            for reset in [true, false] {
                let nonce = SystemTime::now()
                    .duration_since(UNIX_EPOCH)
                    .expect("clock")
                    .as_nanos();
                let root = env::temp_dir().join(format!("holo-cache-handoff-{nonce}-{checked}"));
                fs::create_dir_all(&root).expect("create isolated cartridge root");
                let result =
                    std::panic::catch_unwind(|| spawn_role(&root, "reader", checked, reset));
                fs::remove_dir_all(root).expect("remove isolated cartridge root");
                failures += usize::from(result.is_err());
            }
        }
        assert_eq!(failures, 0, "cache handoff variants failed");
        return;
    }
    let root = env::var("HOLO_CACHE_HANDOFF_ROOT").expect("isolated root");
    let checked = env::var("HOLO_CACHE_HANDOFF_CHECKED").expect("discard variant") == "1";
    let reset = env::var("HOLO_CACHE_HANDOFF_RESET").expect("rewrite variant") == "1";
    let paths = LayoutPaths::for_cartridge(Path::new(&root), "library", "tape");
    initialize_layout(&paths).expect("mount cartridge");
    let options = WriteOptions {
        force_sync: true,
        ..WriteOptions::default()
    };
    if role == "writer" {
        if reset {
            reset_layout_for_overwrite(&paths).expect("another drive reformats cartridge");
        } else {
            run_unmap(&paths, 0, 4).expect("another drive replaces the old record");
        }
        write_logical_block(&paths, 0, b"BBBB", 0, options, None)
            .expect("write replacement record");
        write_logical_block(&paths, 4, b"CCCC", 0, options, None).expect("write additional record");
        discard(&paths, checked);
        return;
    }
    assert_eq!(role, "reader");
    write_logical_block(&paths, 0, b"AAAA", 0, options, None).expect("inventory record");
    assert_eq!(
        read_logical_block(&paths, 0)
            .expect("warm cached read")
            .expect("record")
            .payload,
        b"AAAA"
    );
    discard(&paths, checked);
    spawn_role(Path::new(&root), "writer", checked, reset);
    initialize_layout(&paths).expect("remount cartridge after other drive");
    assert_eq!(
        read_logical_block(&paths, 0)
            .expect("read rewritten record after handoff")
            .expect("record")
            .payload,
        b"BBBB"
    );
    assert_eq!(
        read_logical_block(&paths, 4)
            .expect("read added record after handoff")
            .expect("record")
            .payload,
        b"CCCC"
    );
    discard(&paths, checked);
}
