package metrics

import (
	"sync/atomic"
	"testing"
)

func TestMetricsRegistry_Increment(t *testing.T) {
	r := NewMetricsRegistry()

	atomic.AddInt64(&r.PublicationsActive, 1)
	atomic.AddInt64(&r.PublicationsActive, 1)
	if val := atomic.LoadInt64(&r.PublicationsActive); val != 2 {
		t.Errorf("PublicationsActive = %v, want 2", val)
	}

	r.RecordDedupHit()
	r.RecordDedupHit()
	r.RecordDedupHit()
	if val := atomic.LoadInt64(&r.DedupHitsTotal); val != 3 {
		t.Errorf("DedupHitsTotal = %v, want 3", val)
	}

	r.RecordScsiSenseError()
	if val := atomic.LoadInt64(&r.ScsiSenseErrors); val != 1 {
		t.Errorf("ScsiSenseErrors = %v, want 1", val)
	}
}

func TestMetricsRegistry_Gauge(t *testing.T) {
	r := NewMetricsRegistry()

	atomic.StoreInt64(&r.HealthStatus, 1)
	if val := atomic.LoadInt64(&r.HealthStatus); val != 1 {
		t.Errorf("HealthStatus = %v, want 1", val)
	}

	r.RecordCompressionRatio(1.54)
	r.RecordCompressionRatio(2.10)
	if val := r.GetCompressionRatio(); val != 2.10 {
		t.Errorf("CompressionRatio = %v, want 2.10", val)
	}
}

func TestMetricsRegistry_AuditJournalFailureState(t *testing.T) {
	r := NewMetricsRegistry()

	r.RecordAuditWriteFailure()
	if val := atomic.LoadInt64(&r.AuditWriteFailures); val != 1 {
		t.Errorf("AuditWriteFailures = %v, want 1", val)
	}
	if val := atomic.LoadInt64(&r.AuditJournalFailed); val != 1 {
		t.Errorf("AuditJournalFailed = %v, want 1", val)
	}

	r.RecordAuditJournalSuccess()
	if val := atomic.LoadInt64(&r.AuditJournalFailed); val != 0 {
		t.Errorf("AuditJournalFailed after success = %v, want 0", val)
	}
}
