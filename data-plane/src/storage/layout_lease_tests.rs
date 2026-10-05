use std::fs;
use std::process::Command;

use super::layout_lease::{layout_lease_key, LayoutLease, LayoutLeaseError};

#[test]
fn layout_lease_key_matches_go_sha256_golden_vector() {
    assert_eq!(
        layout_lease_key(2049, 123456, "VTL000001"),
        "80740740baf2b7e51e0efa9d98382f76ecaa360969bdcff495e76784eec2b78c"
    );
}

#[test]
fn layout_lease_excludes_other_openers_until_last_clone_drops() {
    let root = std::env::temp_dir().join(format!("holo-layout-lock-root-{}", std::process::id()));
    let runtime = std::env::temp_dir().join(format!("holo-layout-lock-run-{}", std::process::id()));
    fs::create_dir_all(&root).expect("create root");
    fs::create_dir_all(&runtime).expect("create runtime");

    let first = LayoutLease::acquire(&root, "cart-a", &runtime).expect("acquire layout lease");
    let shared = first.clone();
    assert!(matches!(
        LayoutLease::acquire(&root, "cart-a", &runtime),
        Err(LayoutLeaseError::Busy)
    ));
    drop(first);
    assert!(matches!(
        LayoutLease::acquire(&root, "cart-a", &runtime),
        Err(LayoutLeaseError::Busy)
    ));
    drop(shared);
    LayoutLease::acquire(&root, "cart-a", &runtime)
        .expect("lease releases after the final clone drops");

    let _ = fs::remove_dir_all(root);
    let _ = fs::remove_dir_all(runtime);
}

#[test]
fn layout_leases_share_library_aliases_but_separate_root_and_cartridge() {
    let base = std::env::temp_dir().join(format!("holo-layout-identities-{}", std::process::id()));
    let root_a = base.join("pool-a");
    let root_b = base.join("pool-b");
    let runtime = base.join("run");
    fs::create_dir_all(&root_a).expect("create pool a");
    fs::create_dir_all(&root_b).expect("create pool b");

    let _pool_a = LayoutLease::acquire(&root_a, "cart-a", &runtime).expect("pool a");
    let _pool_b = LayoutLease::acquire(&root_b, "cart-a", &runtime).expect("pool b");
    assert!(matches!(
        LayoutLease::acquire(&root_a, "cart-a", &runtime),
        Err(LayoutLeaseError::Busy)
    ));
    let _cartridge_b = LayoutLease::acquire(&root_a, "cart-b", &runtime).expect("cart b");

    let _ = fs::remove_dir_all(base);
}

#[cfg(unix)]
#[test]
fn layout_lease_rejects_a_symlink_lock_directory() {
    use std::os::unix::fs::symlink;

    let base = std::env::temp_dir().join(format!("holo-layout-symlink-{}", std::process::id()));
    let root = base.join("pool");
    let runtime = base.join("run");
    let elsewhere = base.join("elsewhere");
    fs::create_dir_all(&root).expect("create pool");
    fs::create_dir_all(&elsewhere).expect("create target");
    fs::create_dir_all(&runtime).expect("create runtime");
    symlink(&elsewhere, runtime.join("storage-locks")).expect("create lock directory symlink");

    assert!(LayoutLease::acquire(&root, "cart-a", &runtime).is_err());
    let _ = fs::remove_dir_all(base);
}

#[cfg(unix)]
#[test]
fn layout_lease_aliases_share_and_replaced_root_fails_verification() {
    use std::os::unix::fs::symlink;

    let base = std::env::temp_dir().join(format!("holo-layout-root-alias-{}", std::process::id()));
    let root = base.join("pool");
    let alias = base.join("pool-alias");
    let runtime = base.join("run");
    fs::create_dir_all(&root).expect("create root");
    fs::create_dir_all(&runtime).expect("create runtime");
    symlink(&root, &alias).expect("create root alias");

    let lease = LayoutLease::acquire(&root, "cart-a", &runtime).expect("acquire lease");
    assert!(matches!(
        LayoutLease::acquire(&alias, "cart-a", &runtime),
        Err(LayoutLeaseError::Busy)
    ));
    fs::rename(&root, base.join("pool-old")).expect("rename leased root");
    fs::create_dir_all(&root).expect("create replacement root");
    assert!(matches!(
        lease.verify_pool_root(&root),
        Err(LayoutLeaseError::IdentityConflict)
    ));

    let _ = fs::remove_dir_all(base);
}

#[test]
fn layout_lease_child_process_observes_shared_lock() {
    let root = match std::env::var_os("HOLO_LAYOUT_LEASE_CHILD_ROOT") {
        Some(root) => std::path::PathBuf::from(root),
        None => return,
    };
    let runtime = std::path::PathBuf::from(
        std::env::var_os("HOLO_LAYOUT_LEASE_CHILD_RUNTIME").expect("child runtime dir"),
    );
    let result = LayoutLease::acquire(&root, "cart-a", &runtime);
    assert!(matches!(result, Err(LayoutLeaseError::Busy)));
}

#[test]
fn layout_lease_serializes_alias_across_processes() {
    let base = std::env::temp_dir().join(format!("holo-layout-process-{}", std::process::id()));
    let root = base.join("pool");
    let alias = base.join("pool-alias");
    let runtime = base.join("run");
    fs::create_dir_all(&root).expect("create root");
    fs::create_dir_all(&runtime).expect("create runtime");
    #[cfg(unix)]
    std::os::unix::fs::symlink(&root, &alias).expect("create root alias");

    let _lease = LayoutLease::acquire(&root, "cart-a", &runtime).expect("acquire parent lease");
    let output = Command::new(std::env::current_exe().expect("test binary"))
        .args([
            "--exact",
            "storage::layout_lease_tests::layout_lease_child_process_observes_shared_lock",
            "--nocapture",
        ])
        .env("HOLO_LAYOUT_LEASE_CHILD_ROOT", alias)
        .env("HOLO_LAYOUT_LEASE_CHILD_RUNTIME", &runtime)
        .output()
        .expect("run child process");
    assert!(
        output.status.success(),
        "child process did not observe the shared lease: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    let _ = fs::remove_dir_all(base);
}
