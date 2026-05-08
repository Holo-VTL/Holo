use serde::Serialize;

#[derive(Debug, Clone, Serialize)]
pub struct TelemetryEvent {
    pub component: String,
    pub name: String,
    pub value: f64,
    pub severity: String,
    pub timestamp_unix: i64,
}

impl TelemetryEvent {
    pub fn new(component: impl Into<String>, name: impl Into<String>, value: f64) -> Self {
        Self {
            component: component.into(),
            name: name.into(),
            value,
            severity: "info".to_string(),
            timestamp_unix: chrono::Utc::now().timestamp(),
        }
    }
}

pub fn storage_integrity_event(
    name: impl Into<String>,
    severity: impl Into<String>,
) -> TelemetryEvent {
    let mut event = TelemetryEvent::new("storage", name, 1.0);
    event.severity = severity.into();
    event
}

pub fn storage_rollover_event(rollover_count: u64) -> TelemetryEvent {
    TelemetryEvent::new("storage", "map_rollover", rollover_count as f64)
}

pub fn dedup_hit_event() -> TelemetryEvent {
    TelemetryEvent::new("storage", "dedup_hit", 1.0)
}

pub fn dedup_collision_event() -> TelemetryEvent {
    let mut event = TelemetryEvent::new("storage", "dedup_collision", 1.0);
    event.severity = "warning".to_string();
    event
}

pub fn compression_ratio_event(ratio: f64) -> TelemetryEvent {
    TelemetryEvent::new("storage", "compression_ratio", ratio)
}

pub fn recovery_event(repaired_items: u64) -> TelemetryEvent {
    let mut event = TelemetryEvent::new("storage", "recovery_repair", repaired_items as f64);
    event.severity = "warning".to_string();
    event
}

pub fn scsi_identity_event(name: impl Into<String>) -> TelemetryEvent {
    TelemetryEvent::new("scsi", name, 1.0)
}

pub fn scsi_unsupported_vpd_event(page_code: u8) -> TelemetryEvent {
    let mut event = TelemetryEvent::new("scsi", format!("unsupported_vpd_0x{page_code:02X}"), 1.0);
    event.severity = "warning".to_string();
    event
}

pub fn scsi_sense_event(sense_key: u8, asc: u8, ascq: u8) -> TelemetryEvent {
    let mut event = TelemetryEvent::new(
        "scsi",
        format!("sense_{sense_key:02X}_{asc:02X}_{ascq:02X}"),
        1.0,
    );
    event.severity = "warning".to_string();
    event
}

pub fn scsi_command_lifecycle_event(name: impl Into<String>) -> TelemetryEvent {
    TelemetryEvent::new("scsi", name, 1.0)
}

pub fn scsi_position_error_event(kind: impl Into<String>) -> TelemetryEvent {
    let mut event = TelemetryEvent::new("scsi", kind, 1.0);
    event.severity = "warning".to_string();
    event
}

pub fn scsi_worm_block_event() -> TelemetryEvent {
    let mut event = TelemetryEvent::new("scsi", "worm_write_protect_block", 1.0);
    event.severity = "warning".to_string();
    event
}

pub fn scsi_pr_lifecycle_event(name: impl Into<String>) -> TelemetryEvent {
    TelemetryEvent::new("scsi", name, 1.0)
}

pub fn scsi_pr_conflict_event() -> TelemetryEvent {
    let mut event = TelemetryEvent::new("scsi", "pr_reservation_conflict", 1.0);
    event.severity = "warning".to_string();
    event
}

pub fn scsi_space_branch_event(branch: impl Into<String>) -> TelemetryEvent {
    TelemetryEvent::new("scsi", format!("space_branch_{}", branch.into()), 1.0)
}

pub fn scsi_unsupported_mode_page_event(page_code: u8) -> TelemetryEvent {
    let mut event = TelemetryEvent::new("scsi", format!("unsupported_mode_0x{page_code:02X}"), 1.0);
    event.severity = "warning".to_string();
    event
}

pub fn scsi_unsupported_log_page_event(page_code: u8) -> TelemetryEvent {
    let mut event = TelemetryEvent::new("scsi", format!("unsupported_log_0x{page_code:02X}"), 1.0);
    event.severity = "warning".to_string();
    event
}
