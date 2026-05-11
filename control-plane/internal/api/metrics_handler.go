package api

import (
	"bytes"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/metrics"
)

type MetricsHandler struct {
	registry        *metrics.MetricsRegistry
	storageRootBase string
}

func NewMetricsHandler(registry *metrics.MetricsRegistry, storageRootBase ...string) *MetricsHandler {
	rootBase := ""
	if len(storageRootBase) > 0 {
		rootBase = storageRootBase[0]
	}
	return &MetricsHandler{registry: registry, storageRootBase: rootBase}
}

func (h *MetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		respondError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(PrometheusText(h.registry) + StorageLayoutPrometheusText(h.storageRootBase)))
}

func PrometheusText(registry *metrics.MetricsRegistry) string {
	if registry == nil {
		return ""
	}
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "# HELP holo_publications_active Number of active iSCSI target publications\n")
	fmt.Fprintf(&buf, "# TYPE holo_publications_active gauge\n")
	fmt.Fprintf(&buf, "holo_publications_active %d\n", atomic.LoadInt64(&registry.PublicationsActive))

	fmt.Fprintf(&buf, "# HELP holo_publications_total Total target publications created historically\n")
	fmt.Fprintf(&buf, "# TYPE holo_publications_total counter\n")
	fmt.Fprintf(&buf, "holo_publications_total %d\n", atomic.LoadInt64(&registry.PublicationsTotal))

	fmt.Fprintf(&buf, "# HELP holo_audit_events_total Total number of audit events recorded\n")
	fmt.Fprintf(&buf, "# TYPE holo_audit_events_total counter\n")
	fmt.Fprintf(&buf, "holo_audit_events_total %d\n", atomic.LoadInt64(&registry.AuditEventsTotal))

	fmt.Fprintf(&buf, "# HELP holo_audit_write_failures_total Total failed audit writes to persistent journal\n")
	fmt.Fprintf(&buf, "# TYPE holo_audit_write_failures_total counter\n")
	fmt.Fprintf(&buf, "holo_audit_write_failures_total %d\n", atomic.LoadInt64(&registry.AuditWriteFailures))

	fmt.Fprintf(&buf, "# HELP holo_audit_journal_failed Persistent audit journal failure state (1=failed, 0=ok)\n")
	fmt.Fprintf(&buf, "# TYPE holo_audit_journal_failed gauge\n")
	fmt.Fprintf(&buf, "holo_audit_journal_failed %d\n", atomic.LoadInt64(&registry.AuditJournalFailed))

	fmt.Fprintf(&buf, "# HELP holo_audit_journal_parse_errors_total Total malformed audit journal rows skipped during replay\n")
	fmt.Fprintf(&buf, "# TYPE holo_audit_journal_parse_errors_total counter\n")
	fmt.Fprintf(&buf, "holo_audit_journal_parse_errors_total %d\n", atomic.LoadInt64(&registry.AuditParseFailures))

	fmt.Fprintf(&buf, "# HELP holo_audit_journal_size_bytes Current audit journal size in bytes\n")
	fmt.Fprintf(&buf, "# TYPE holo_audit_journal_size_bytes gauge\n")
	fmt.Fprintf(&buf, "holo_audit_journal_size_bytes %d\n", atomic.LoadInt64(&registry.AuditJournalSizeBytes))

	fmt.Fprintf(&buf, "# HELP holo_audit_journal_lag_seconds Seconds since the last successful audit journal write\n")
	fmt.Fprintf(&buf, "# TYPE holo_audit_journal_lag_seconds gauge\n")
	lastWrite := atomic.LoadInt64(&registry.AuditJournalLastWriteUnix)
	lag := int64(0)
	if lastWrite > 0 {
		lag = time.Now().UTC().Unix() - lastWrite
		if lag < 0 {
			lag = 0
		}
	}
	fmt.Fprintf(&buf, "holo_audit_journal_lag_seconds %d\n", lag)

	fmt.Fprintf(&buf, "# HELP holo_scsi_sense_errors_total Total SCSI CHECK CONDITION sense errors\n")
	fmt.Fprintf(&buf, "# TYPE holo_scsi_sense_errors_total counter\n")
	fmt.Fprintf(&buf, "holo_scsi_sense_errors_total %d\n", atomic.LoadInt64(&registry.ScsiSenseErrors))

	fmt.Fprintf(&buf, "# HELP holo_dedup_hits_total Total storage deduplication lookup hits\n")
	fmt.Fprintf(&buf, "# TYPE holo_dedup_hits_total counter\n")
	fmt.Fprintf(&buf, "holo_dedup_hits_total %d\n", atomic.LoadInt64(&registry.DedupHitsTotal))

	fmt.Fprintf(&buf, "# HELP holo_compression_ratio_avg Latest observed compression ratio (gauge)\n")
	fmt.Fprintf(&buf, "# TYPE holo_compression_ratio_avg gauge\n")
	fmt.Fprintf(&buf, "holo_compression_ratio_avg %f\n", registry.GetCompressionRatio())

	fmt.Fprintf(&buf, "# HELP holo_health_status Aggregated health check status (1=ok, 0=down)\n")
	fmt.Fprintf(&buf, "# TYPE holo_health_status gauge\n")
	fmt.Fprintf(&buf, "holo_health_status %d\n", atomic.LoadInt64(&registry.HealthStatus))

	fmt.Fprintf(&buf, "# HELP holo_api_request_duration_seconds HTTP API request duration in seconds\n")
	fmt.Fprintf(&buf, "# TYPE holo_api_request_duration_seconds histogram\n")
	var cumulative uint64
	for i, upper := range metrics.APIRequestDurationBucketMicros {
		cumulative += atomic.LoadUint64(&registry.APIRequestDurationBuckets[i])
		fmt.Fprintf(&buf, "holo_api_request_duration_seconds_bucket{le=\"%.3f\"} %d\n", float64(upper)/1_000_000, cumulative)
	}
	count := atomic.LoadUint64(&registry.APIRequestDurationCount)
	fmt.Fprintf(&buf, "holo_api_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", count)
	fmt.Fprintf(&buf, "holo_api_request_duration_seconds_sum %.6f\n", float64(atomic.LoadUint64(&registry.APIRequestDurationSumUsec))/1_000_000)
	fmt.Fprintf(&buf, "holo_api_request_duration_seconds_count %d\n", count)

	return buf.String()
}
