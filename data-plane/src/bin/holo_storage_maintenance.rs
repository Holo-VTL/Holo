use std::env;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use serde::Serialize;

use data_plane::storage::layout::sanitize_id;
use data_plane::storage::maintenance_progress::MaintenancePhase;
use data_plane::storage::offline_reclaim::reclaim_one_segment_with_cursor_and_progress;
use data_plane::storage::space_guard::SpaceGuardError;
use data_plane::storage::{resolve_layout_paths, StorageError};

#[derive(Debug)]
struct Args {
    pool_root: PathBuf,
    pool_id: String,
    library_id: String,
    cartridge_id: String,
    runtime_dir: PathBuf,
    scan_cursor: Option<String>,
}

#[derive(Serialize)]
struct Report {
    schema_version: u8,
    pool_id: String,
    library_id: String,
    cartridge_id: String,
    status: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    reason: Option<&'static str>,
    processed_segments: u32,
    has_more: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    scan_cursor: Option<String>,
    allocated_before_bytes: u64,
    allocated_after_bytes: u64,
    net_freed_bytes: u64,
    duration_ms: u64,
}

#[derive(Serialize)]
struct ProgressLine {
    schema_version: u8,
    phase: &'static str,
    verified_bytes: u64,
    verified_records: u64,
}

struct ProgressReporter {
    cancelled: Arc<AtomicBool>,
    verified_bytes: u64,
    verified_records: u64,
    last_emit: Instant,
    last_phase: Option<MaintenancePhase>,
}

impl ProgressReporter {
    fn new(cancelled: Arc<AtomicBool>) -> Self {
        Self {
            cancelled,
            verified_bytes: 0,
            verified_records: 0,
            last_emit: Instant::now() - Duration::from_secs(10),
            last_phase: None,
        }
    }

    fn update(
        &mut self,
        phase: MaintenancePhase,
        bytes: u64,
        records: u64,
    ) -> Result<(), StorageError> {
        if self.cancelled.load(Ordering::Relaxed) {
            return Err(StorageError::Cancelled);
        }
        self.verified_bytes = self
            .verified_bytes
            .checked_add(bytes)
            .ok_or_else(|| StorageError::Internal("progress byte count overflow".to_string()))?;
        self.verified_records = self
            .verified_records
            .checked_add(records)
            .ok_or_else(|| StorageError::Internal("progress record count overflow".to_string()))?;
        let phase_changed = self.last_phase != Some(phase);
        if phase_changed || self.last_emit.elapsed() >= Duration::from_secs(5) {
            let line = ProgressLine {
                schema_version: 2,
                phase: phase.as_str(),
                verified_bytes: self.verified_bytes,
                verified_records: self.verified_records,
            };
            let mut encoded = serde_json::to_vec(&line)
                .map_err(|_| StorageError::Internal("progress serialization failed".to_string()))?;
            if encoded.len() + "HOLO_PROGRESS ".len() + 1 > 1024 {
                return Err(StorageError::Internal(
                    "progress line exceeded limit".to_string(),
                ));
            }
            encoded.extend_from_slice(b"\n");
            let mut stderr = std::io::stderr().lock();
            use std::io::Write;
            stderr
                .write_all(b"HOLO_PROGRESS ")
                .and_then(|()| stderr.write_all(&encoded))
                .map_err(StorageError::Io)?;
            self.last_emit = Instant::now();
            self.last_phase = Some(phase);
        }
        Ok(())
    }
}

fn main() -> ExitCode {
    match run() {
        Ok(code) => code,
        Err(err) => {
            eprintln!("[storage-maintenance] {err}");
            ExitCode::from(2)
        }
    }
}

fn run() -> Result<ExitCode, String> {
    let args = parse_args(env::args().skip(1).collect())?;
    if !valid_id(&args.pool_id) || !valid_id(&args.library_id) || !valid_id(&args.cartridge_id) {
        return Err("invalid management ID".to_string());
    }
    if args
        .scan_cursor
        .as_ref()
        .is_some_and(|cursor| cursor.len() > 4 * 1024)
    {
        return Err("scan cursor exceeds its limit".to_string());
    }
    env::set_var("HOLO_RUN_DIR", &args.runtime_dir);

    let expected_root = env::var_os("HOLO_STORAGE_POOL_ROOT_BASE")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("/var/lib/holo/storage-pools"))
        .join(sanitize_id(&args.pool_id));
    let canonical_expected = match fs::canonicalize(&expected_root) {
        Ok(root) => root,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
            emit_report(
                &args,
                "deferred",
                Some("pool_unavailable"),
                0,
                false,
                None,
                0,
                0,
                0,
                0,
            )?;
            return Ok(ExitCode::SUCCESS);
        }
        Err(_) => {
            emit_report(
                &args,
                "failed",
                Some("io_error"),
                0,
                false,
                None,
                0,
                0,
                0,
                0,
            )?;
            return Ok(ExitCode::from(1));
        }
    };
    let canonical_given = fs::canonicalize(&args.pool_root)
        .map_err(|_| "requested pool root is unavailable".to_string())?;
    if canonical_expected != canonical_given || !canonical_given.is_dir() {
        return Err("pool root does not match the approved storage pool".to_string());
    }
    match media_state_contains_cartridge(&args.library_id, &args.cartridge_id) {
        Ok(true) => {
            emit_report(&args, "deferred", Some("busy"), 0, false, None, 0, 0, 0, 0)?;
            return Ok(ExitCode::SUCCESS);
        }
        Ok(false) => {}
        Err(_) => {
            emit_report(
                &args,
                "failed",
                Some("io_error"),
                0,
                false,
                None,
                0,
                0,
                0,
                0,
            )?;
            return Ok(ExitCode::from(1));
        }
    }

    let started = Instant::now();
    let media_key = format!("{}__maintenance", sanitize_id(&args.library_id));
    env::set_var("HOLO_MEDIA_STATE_KEY", media_key);
    env::set_var("HOLO_LAYOUT_LIBRARY_ID", &args.library_id);
    env::set_var("HOLO_STORAGE_ROOT", &canonical_given);
    let paths = match resolve_layout_paths(
        &canonical_given,
        &args.library_id,
        "maintenance",
        &args.cartridge_id,
    ) {
        Ok(paths) => paths,
        Err(StorageError::Conflict(reason)) => {
            let reason = match reason.as_str() {
                "ambiguous_layout" => "ambiguous_layout",
                "identity_conflict" => "identity_conflict",
                _ => "identity_conflict",
            };
            emit_report(&args, "deferred", Some(reason), 0, false, None, 0, 0, 0, 0)?;
            return Ok(ExitCode::SUCCESS);
        }
        Err(_) => {
            emit_report(
                &args,
                "failed",
                Some("io_error"),
                0,
                false,
                None,
                0,
                0,
                0,
                0,
            )?;
            return Ok(ExitCode::from(1));
        }
    };
    if !paths.root.exists() {
        emit_report(
            &args,
            "completed",
            None,
            0,
            false,
            None,
            0,
            0,
            0,
            elapsed_ms(started),
        )?;
        return Ok(ExitCode::SUCCESS);
    }

    let cancelled = Arc::new(AtomicBool::new(false));
    signal_hook::flag::register(signal_hook::consts::SIGTERM, Arc::clone(&cancelled))
        .map_err(|_| "cannot register cancellation handler".to_string())?;
    signal_hook::flag::register(signal_hook::consts::SIGINT, Arc::clone(&cancelled))
        .map_err(|_| "cannot register cancellation handler".to_string())?;
    let mut reporter = ProgressReporter::new(cancelled);
    let mut progress = |phase, bytes, records| reporter.update(phase, bytes, records);
    match reclaim_one_segment_with_cursor_and_progress(
        &paths,
        &canonical_given,
        &args.library_id,
        &args.cartridge_id,
        &args.runtime_dir,
        args.scan_cursor.as_deref(),
        &mut progress,
    ) {
        Ok(report) => {
            emit_report(
                &args,
                "completed",
                None,
                report.processed_segments,
                report.has_more,
                report.scan_cursor,
                report.allocated_before_bytes,
                report.allocated_after_bytes,
                report.net_freed_bytes,
                elapsed_ms(started),
            )?;
            Ok(ExitCode::SUCCESS)
        }
        Err(err) => {
            let (status, reason, has_more) = classify_error(&err);
            emit_report(
                &args,
                status,
                Some(reason),
                0,
                has_more,
                None,
                0,
                0,
                0,
                elapsed_ms(started),
            )?;
            if status == "failed" || status == "cancelled" {
                Ok(ExitCode::from(1))
            } else {
                Ok(ExitCode::SUCCESS)
            }
        }
    }
}

fn parse_args(args: Vec<String>) -> Result<Args, String> {
    let mut values = std::collections::HashMap::new();
    let mut iter = args.into_iter();
    while let Some(flag) = iter.next() {
        let key = flag
            .strip_prefix("--")
            .ok_or_else(|| "expected a named option".to_string())?;
        let value = iter
            .next()
            .ok_or_else(|| format!("missing value for --{key}"))?;
        if !matches!(
            key,
            "pool-root" | "pool-id" | "library-id" | "cartridge-id" | "runtime-dir" | "scan-cursor"
        ) {
            return Err(format!("unknown option --{key}"));
        }
        if values.insert(key.to_string(), value).is_some() {
            return Err(format!("duplicate option --{key}"));
        }
    }
    let take = |key: &str| {
        values
            .get(key)
            .filter(|value| !value.trim().is_empty())
            .map(String::as_str)
            .ok_or_else(|| format!("missing --{key}"))
    };
    let pool_root = PathBuf::from(take("pool-root")?);
    let runtime_dir = PathBuf::from(take("runtime-dir")?);
    if !pool_root.is_absolute() || !runtime_dir.is_absolute() {
        return Err("pool-root and runtime-dir must be absolute paths".to_string());
    }
    if values
        .get("scan-cursor")
        .is_some_and(|cursor| cursor.len() > 4 * 1024)
    {
        return Err("scan cursor exceeds its limit".to_string());
    }
    Ok(Args {
        pool_root,
        pool_id: take("pool-id")?.to_string(),
        library_id: take("library-id")?.to_string(),
        cartridge_id: take("cartridge-id")?.to_string(),
        runtime_dir,
        scan_cursor: values.get("scan-cursor").cloned(),
    })
}

fn valid_id(value: &str) -> bool {
    !value.trim().is_empty()
        && value.len() <= 128
        && !value.contains(['/', '\\', '\0'])
        && !value.contains("..")
}

fn media_state_contains_cartridge(library_id: &str, cartridge_id: &str) -> Result<bool, String> {
    let dir = env::var_os("HOLO_MEDIA_STATE_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("/run/holo/media-state"));
    let entries = match fs::read_dir(&dir) {
        Ok(entries) => entries,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(_err) => return Err("shared media state cannot be read".to_string()),
    };
    let prefix = format!("{}__", sanitize_id(library_id));
    for entry in entries {
        let entry = entry.map_err(|_| "shared media state cannot be read".to_string())?;
        let name = entry.file_name();
        let name = name.to_string_lossy();
        if !name.starts_with(&prefix) || !name.ends_with(".state") {
            continue;
        }
        let loaded = read_media_state(&entry.path())?;
        if loaded.as_deref() == Some(cartridge_id) {
            return Ok(true);
        }
    }
    Ok(false)
}

fn read_media_state(path: &Path) -> Result<Option<String>, String> {
    let raw = match fs::read_to_string(path) {
        Ok(raw) => raw,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err("shared media state cannot be read".to_string()),
    };
    let value = raw.trim();
    if value.is_empty() {
        return Ok(None);
    }
    if value.contains(['\r', '\n']) {
        return Err("shared media state format is invalid".to_string());
    }
    let cartridge = value.strip_prefix("cartridge=").unwrap_or(value).trim();
    if cartridge.is_empty() {
        return Ok(None);
    }
    if cartridge.contains(['=', '/', '\\', '\0'])
        || cartridge.contains("..")
        || cartridge.len() > 128
    {
        return Err("shared media state format is invalid".to_string());
    }
    Ok(Some(cartridge.to_string()))
}

fn classify_error(err: &data_plane::storage::StorageError) -> (&'static str, &'static str, bool) {
    use data_plane::storage::StorageError;
    match err {
        StorageError::Conflict(message) if message.contains("WORM") => ("deferred", "busy", false),
        StorageError::Conflict(message) if message.contains("retention") => {
            ("deferred", "busy", false)
        }
        StorageError::Conflict(message) if message.contains("busy") => ("deferred", "busy", false),
        StorageError::Conflict(message) if message.contains("ambiguous_layout") => {
            ("deferred", "ambiguous_layout", true)
        }
        StorageError::Conflict(message) if message.contains("identity_conflict") => {
            ("deferred", "identity_conflict", true)
        }
        StorageError::Conflict(message) if message.contains("changed after reclaim scan") => {
            ("failed", "no_progress", true)
        }
        StorageError::MetadataBudgetExceeded => ("deferred", "metadata_budget_exceeded", true),
        StorageError::SpaceExhausted(_) => ("deferred", "space_unavailable", true),
        StorageError::SpaceAdmission(SpaceGuardError::Busy) => ("deferred", "busy", true),
        StorageError::SpaceAdmission(SpaceGuardError::InsufficientSpace { .. }) => {
            ("deferred", "space_unavailable", true)
        }
        StorageError::SpaceAdmission(SpaceGuardError::UnknownCapacity) => {
            ("deferred", "space_unavailable", true)
        }
        StorageError::SpaceAdmission(_) => ("failed", "io_error", true),
        StorageError::Cancelled => ("cancelled", "cancelled", true),
        StorageError::Corrupt(_) | StorageError::VersionMismatch { .. } => {
            ("failed", "integrity_error", true)
        }
        _ => ("failed", "io_error", true),
    }
}

#[allow(clippy::too_many_arguments)]
fn emit_report(
    args: &Args,
    status: &'static str,
    reason: Option<&'static str>,
    processed_segments: u32,
    has_more: bool,
    scan_cursor: Option<String>,
    allocated_before_bytes: u64,
    allocated_after_bytes: u64,
    net_freed_bytes: u64,
    duration_ms: u64,
) -> Result<(), String> {
    let report = Report {
        schema_version: 2,
        pool_id: args.pool_id.clone(),
        library_id: args.library_id.clone(),
        cartridge_id: args.cartridge_id.clone(),
        status,
        reason,
        processed_segments,
        has_more,
        scan_cursor,
        allocated_before_bytes,
        allocated_after_bytes,
        net_freed_bytes,
        duration_ms,
    };
    validate_report(&report)?;
    let encoded =
        serde_json::to_vec(&report).map_err(|_| "report serialization failed".to_string())?;
    if encoded.len() > 64 * 1024 {
        return Err("maintenance report exceeded output limit".to_string());
    }
    println!(
        "{}",
        String::from_utf8(encoded).map_err(|_| "report encoding failed".to_string())?
    );
    Ok(())
}

fn validate_report(report: &Report) -> Result<(), String> {
    if report.processed_segments > 1 {
        return Err("processed segment count exceeds one".to_string());
    }
    let reason_valid = match report.status {
        "completed" => report.reason.is_none(),
        "deferred" => report.reason.is_some_and(|reason| {
            matches!(
                reason,
                "busy"
                    | "pool_unavailable"
                    | "identity_conflict"
                    | "ambiguous_layout"
                    | "metadata_budget_exceeded"
                    | "space_unavailable"
            )
        }),
        "failed" => report
            .reason
            .is_some_and(|reason| matches!(reason, "integrity_error" | "io_error" | "no_progress")),
        "cancelled" => report.reason == Some("cancelled"),
        _ => false,
    };
    if !reason_valid {
        return Err("maintenance status and reason are inconsistent".to_string());
    }
    if report.status != "completed"
        && (report.processed_segments != 0
            || report.net_freed_bytes != 0
            || report.scan_cursor.is_some())
    {
        return Err("non-completed result reports committed work".to_string());
    }
    if report
        .scan_cursor
        .as_ref()
        .is_some_and(|cursor| cursor.len() > 4 * 1024 || !report.has_more)
    {
        return Err("maintenance cursor is invalid".to_string());
    }
    let expected_freed = report
        .allocated_before_bytes
        .saturating_sub(report.allocated_after_bytes);
    if report.net_freed_bytes != expected_freed {
        return Err("maintenance released-byte counters are inconsistent".to_string());
    }
    Ok(())
}

fn elapsed_ms(started: Instant) -> u64 {
    u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX)
}

#[cfg(test)]
mod tests {
    use super::{classify_error, parse_args, validate_report, Report};
    use data_plane::storage::{space_guard::SpaceGuardError, StorageError};

    fn valid_report() -> Report {
        Report {
            schema_version: 2,
            pool_id: "pool-a".to_string(),
            library_id: "library-a".to_string(),
            cartridge_id: "cart-a".to_string(),
            status: "completed",
            reason: None,
            processed_segments: 0,
            has_more: false,
            scan_cursor: None,
            allocated_before_bytes: 10,
            allocated_after_bytes: 10,
            net_freed_bytes: 0,
            duration_ms: 1,
        }
    }

    #[test]
    fn report_rejects_cursor_without_more_work_and_invalid_reason_pairs() {
        let mut report = valid_report();
        report.scan_cursor = Some("{}".to_string());
        assert!(validate_report(&report).is_err());
        let mut report = valid_report();
        report.status = "failed";
        report.reason = Some("busy");
        assert!(validate_report(&report).is_err());
        let mut report = valid_report();
        report.status = "deferred";
        report.reason = Some("metadata_budget_exceeded");
        report.has_more = true;
        assert!(validate_report(&report).is_ok());
    }

    #[test]
    fn parse_args_rejects_oversized_cursor_and_accepts_bounded_cursor() {
        let args = vec![
            "--pool-root".to_string(),
            "/tmp/pool".to_string(),
            "--pool-id".to_string(),
            "pool-a".to_string(),
            "--library-id".to_string(),
            "library-a".to_string(),
            "--cartridge-id".to_string(),
            "cart-a".to_string(),
            "--runtime-dir".to_string(),
            "/tmp/run".to_string(),
            "--scan-cursor".to_string(),
            "{}".to_string(),
        ];
        let parsed = parse_args(args.clone()).expect("bounded cursor should parse");
        assert_eq!(parsed.scan_cursor.as_deref(), Some("{}"));
        let mut oversized = args;
        *oversized.last_mut().expect("cursor argument") = "x".repeat(4097);
        assert!(parse_args(oversized).is_err());
    }

    #[test]
    fn typed_storage_errors_map_to_fixed_worker_reasons() {
        assert_eq!(
            classify_error(&StorageError::MetadataBudgetExceeded),
            ("deferred", "metadata_budget_exceeded", true)
        );
        assert_eq!(
            classify_error(&StorageError::Cancelled),
            ("cancelled", "cancelled", true)
        );
        assert_eq!(
            classify_error(&StorageError::SpaceAdmission(
                SpaceGuardError::InsufficientSpace {
                    available: 0,
                    required: 1
                }
            )),
            ("deferred", "space_unavailable", true)
        );
    }

    #[test]
    fn report_schema_is_v2_and_cursor_is_optional() {
        let report = valid_report();
        let encoded = serde_json::to_value(report).expect("serialize report");
        assert_eq!(encoded["schema_version"], 2);
        assert!(encoded.get("scan_cursor").is_none());
    }
}
