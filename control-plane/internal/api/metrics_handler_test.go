package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/metrics"
)

func TestMetricsHandler_ResponseFormat(t *testing.T) {
	registry := metrics.NewMetricsRegistry()

	atomic.AddInt64(&registry.PublicationsActive, 5)
	atomic.AddInt64(&registry.ScsiSenseErrors, 42)
	registry.RecordCompressionRatio(2.35)
	registry.RecordAPIRequestDuration(7 * time.Millisecond)
	registry.RecordAuditJournalStats(128, time.Now().UTC().Add(-30*time.Second))

	handler := NewMetricsHandler(registry)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	out := rr.Body.String()

	if !strings.Contains(out, "holo_publications_active 5") {
		t.Errorf("Missing holo_publications_active")
	}
	if !strings.Contains(out, "holo_scsi_sense_errors_total 42") {
		t.Errorf("Missing holo_scsi_sense_errors_total")
	}
	if !strings.Contains(out, "holo_compression_ratio_avg 2.350000") {
		t.Errorf("Missing holo_compression_ratio_avg %s", out)
	}
	if !strings.Contains(out, "holo_audit_write_failures_total 0") {
		t.Errorf("Missing holo_audit_write_failures_total %s", out)
	}
	if !strings.Contains(out, "holo_audit_journal_failed 0") {
		t.Errorf("Missing holo_audit_journal_failed %s", out)
	}
	if !strings.Contains(out, "holo_audit_journal_parse_errors_total 0") {
		t.Errorf("Missing holo_audit_journal_parse_errors_total %s", out)
	}
	if !strings.Contains(out, "holo_audit_journal_size_bytes 128") {
		t.Errorf("Missing holo_audit_journal_size_bytes %s", out)
	}
	if !strings.Contains(out, "holo_audit_journal_lag_seconds") {
		t.Errorf("Missing holo_audit_journal_lag_seconds %s", out)
	}
	if !strings.Contains(out, "holo_api_request_duration_seconds_bucket") {
		t.Errorf("Missing holo_api_request_duration_seconds histogram %s", out)
	}
	if !strings.Contains(out, "holo_api_request_duration_seconds_count 1") {
		t.Errorf("Missing holo_api_request_duration_seconds_count %s", out)
	}
	if !strings.Contains(out, "holo_storage_segment_files 0") {
		t.Errorf("Missing holo_storage_segment_files %s", out)
	}
	if !strings.Contains(out, `holo_scsi_commands_total{bucket="read"} 0`) {
		t.Errorf("Missing holo_scsi_commands_total %s", out)
	}
}

func TestMetricsHandler_RejectsUnsupportedMethod(t *testing.T) {
	handler := NewMetricsHandler(metrics.NewMetricsRegistry())
	req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestMetricsRegistry_PublishIncrements(t *testing.T) {
	registry := metrics.NewMetricsRegistry()
	registry.RecordPublicationPublish()

	if atomic.LoadInt64(&registry.PublicationsActive) != 1 {
		t.Errorf("active publications expected 1, got %d", atomic.LoadInt64(&registry.PublicationsActive))
	}
	if atomic.LoadInt64(&registry.PublicationsTotal) != 1 {
		t.Errorf("total publications expected 1, got %d", atomic.LoadInt64(&registry.PublicationsTotal))
	}

	registry.RecordPublicationUnpublish()

	if atomic.LoadInt64(&registry.PublicationsActive) != 0 {
		t.Errorf("active publications expected 0, got %d", atomic.LoadInt64(&registry.PublicationsActive))
	}
	if atomic.LoadInt64(&registry.PublicationsTotal) != 1 {
		t.Errorf("total publications expected 1 (should not decrement), got %d", atomic.LoadInt64(&registry.PublicationsTotal))
	}
}

func TestMetricsRegistry_UnpublishDoesNotGoNegative(t *testing.T) {
	registry := metrics.NewMetricsRegistry()
	registry.RecordPublicationUnpublish()
	registry.RecordPublicationUnpublish()

	if atomic.LoadInt64(&registry.PublicationsActive) != 0 {
		t.Errorf("active publications expected 0 floor, got %d", atomic.LoadInt64(&registry.PublicationsActive))
	}
}
