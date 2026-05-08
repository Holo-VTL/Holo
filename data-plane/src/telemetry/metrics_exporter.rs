use crate::telemetry::events::TelemetryEvent;

pub fn export(name: &str, value: f64) -> TelemetryEvent {
    TelemetryEvent::new("data-plane", name, value)
}
