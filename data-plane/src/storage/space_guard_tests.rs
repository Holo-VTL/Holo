use std::fs;

use super::space_guard::{
    ensure_capacity, estimate_write_peak_bytes, round_up_to_allocation_unit, FilesystemLock,
    FilesystemSnapshot, SpaceGuardError,
};

#[test]
fn allocation_rounding_uses_checked_arithmetic() {
    assert_eq!(round_up_to_allocation_unit(0, 4096).unwrap(), 0);
    assert_eq!(round_up_to_allocation_unit(1, 4096).unwrap(), 4096);
    assert_eq!(round_up_to_allocation_unit(4096, 4096).unwrap(), 4096);
    assert_eq!(round_up_to_allocation_unit(4097, 4096).unwrap(), 8192);
    assert!(round_up_to_allocation_unit(1, 0).is_err());
    assert!(round_up_to_allocation_unit(u64::MAX, 4096).is_err());
}

#[test]
fn write_peak_estimate_accounts_for_payload_records_segments_and_metadata() {
    let got = estimate_write_peak_bytes(4097, 2, 1, 1, 4096).unwrap();
    assert_eq!(got, 8192 + 350 + 100 + 4096);
}

#[test]
fn write_peak_estimate_rejects_overflow_and_unknown_allocation_unit() {
    assert!(estimate_write_peak_bytes(u64::MAX, 1, 1, 1, 4096).is_err());
    assert!(estimate_write_peak_bytes(1, u64::MAX, 1, 0, 4096).is_err());
    assert!(estimate_write_peak_bytes(1, 1, 1, 0, 0).is_err());
}

#[test]
fn admission_requires_payload_estimate_plus_fixed_reserve() {
    let reserve = 64 * 1024 * 1024;
    let peak = 16 * 1024 * 1024;
    ensure_capacity(peak + reserve, peak, reserve).expect("exact boundary is accepted");
    assert!(matches!(
        ensure_capacity(peak + reserve - 1, peak, reserve),
        Err(SpaceGuardError::InsufficientSpace { .. })
    ));
    assert!(ensure_capacity(u64::MAX, u64::MAX, 1).is_err());
}

#[test]
fn filesystem_probe_reads_the_open_root_device_and_available_space() {
    let root = std::env::temp_dir();
    let snapshot = FilesystemSnapshot::probe(&root).expect("probe temp filesystem");
    assert!(snapshot.filesystem_id > 0);
    assert!(snapshot.allocation_unit > 0);
    assert!(snapshot.available_bytes > 0);
}

#[test]
fn pool_root_aliases_on_one_filesystem_share_the_same_exclusive_lock() {
    let base = std::env::temp_dir().join(format!("holo-space-lock-{}", std::process::id()));
    let root_a = base.join("pool-a");
    let root_b = base.join("pool-b");
    let runtime = base.join("run");
    fs::create_dir_all(&root_a).expect("create pool a");
    fs::create_dir_all(&root_b).expect("create pool b");

    let first = FilesystemLock::acquire(&root_a, &runtime).expect("lock pool a filesystem");
    assert!(matches!(
        FilesystemLock::acquire(&root_b, &runtime),
        Err(SpaceGuardError::Busy)
    ));
    drop(first);
    FilesystemLock::acquire(&root_b, &runtime).expect("filesystem lock releases");

    let _ = fs::remove_dir_all(base);
}

#[test]
fn active_write_reservations_are_shared_across_filesystem_lock_holders() {
    let base = std::env::temp_dir().join(format!("holo-space-reservation-{}", std::process::id()));
    let root = base.join("pool");
    let runtime = base.join("run");
    fs::create_dir_all(&root).expect("create pool");

    let reservation = {
        let lock = FilesystemLock::acquire(&root, &runtime).expect("lock filesystem for reserve");
        let reservation = lock.reserve(123_456).expect("create write reservation");
        assert_eq!(
            lock.active_reservation_bytes().expect("read reservation"),
            123_456
        );
        reservation
    };
    let lock = FilesystemLock::acquire(&root, &runtime).expect("reacquire filesystem lock");
    assert_eq!(
        lock.active_reservation_bytes()
            .expect("read in-flight reservation"),
        123_456
    );
    drop(lock);
    drop(reservation);

    let lock = FilesystemLock::acquire(&root, &runtime).expect("lock filesystem after completion");
    assert_eq!(
        lock.active_reservation_bytes()
            .expect("read released reservations"),
        0
    );
    drop(lock);
    let _ = fs::remove_dir_all(base);
}

#[test]
fn concurrent_reservations_are_summed_and_orphan_reservations_are_reclaimed() {
    let base = std::env::temp_dir().join(format!(
        "holo-space-concurrent-reservation-{}",
        std::process::id()
    ));
    let root = base.join("pool");
    let runtime = base.join("run");
    fs::create_dir_all(&root).expect("create pool");
    let first = {
        let lock = FilesystemLock::acquire(&root, &runtime).expect("lock for first reserve");
        lock.reserve(100_000).expect("first reservation")
    };
    let second = super::offline_reclaim::reserve_capacity(&root, &runtime, 1024 * 1024, 1, 1, 4096)
        .expect("second writer can reserve available space concurrently");
    let lock = FilesystemLock::acquire(&root, &runtime).expect("read concurrent reservations");
    assert!(
        lock.active_reservation_bytes().expect("sum reservations") > 100_000,
        "admission must account for both reservations"
    );
    let orphan_path = runtime
        .join("storage-locks")
        .join(format!("fs-{}-orphan-1.reserve", lock.filesystem_id));
    fs::write(&orphan_path, 987u64.to_le_bytes()).expect("write orphan reservation");
    assert!(
        lock.active_reservation_bytes()
            .expect("reclaim unlocked orphan")
            > 100_000
    );
    assert!(!orphan_path.exists(), "unlocked orphan token is removed");
    drop(lock);
    drop(second);
    drop(first);
    let lock = FilesystemLock::acquire(&root, &runtime).expect("reacquire after reservations");
    assert_eq!(
        lock.active_reservation_bytes()
            .expect("no live reservations remain"),
        0
    );
    drop(lock);
    let _ = fs::remove_dir_all(base);
}

#[test]
fn maintenance_capacity_reservation_releases_the_shared_filesystem_lock() {
    let base = std::env::temp_dir().join(format!(
        "holo-space-maintenance-short-lock-{}",
        std::process::id()
    ));
    let root = base.join("pool");
    let runtime = base.join("run");
    fs::create_dir_all(&root).expect("create pool");

    let reservation =
        super::offline_reclaim::reserve_capacity(&root, &runtime, 1024 * 1024, 1, 1, 4096)
            .expect("reserve maintenance space");
    let lock = FilesystemLock::acquire(&root, &runtime)
        .expect("shared filesystem lock is released after admission");
    assert!(
        lock.active_reservation_bytes()
            .expect("read active reservation")
            > 0
    );
    drop(lock);
    drop(reservation);

    let lock = FilesystemLock::acquire(&root, &runtime).expect("reacquire after release");
    assert_eq!(
        lock.active_reservation_bytes()
            .expect("read reservations after release"),
        0
    );
    drop(lock);
    let _ = fs::remove_dir_all(base);
}
