package metrics

import (
	"sync/atomic"
	"testing"
	"time"
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

func TestMetricsRegistry_AuditJournalParseFailures(t *testing.T) {
	r := NewMetricsRegistry()

	r.RecordAuditJournalParseFailures(2)
	r.RecordAuditJournalParseFailures(0)
	r.RecordAuditJournalParseFailures(-1)

	if val := atomic.LoadInt64(&r.AuditParseFailures); val != 2 {
		t.Fatalf("AuditParseFailures = %v, want 2", val)
	}
}

func TestMetricsRegistry_AuditJournalStats(t *testing.T) {
	r := NewMetricsRegistry()
	occurredAt := time.Unix(100, 0).UTC()

	r.RecordAuditJournalStats(4096, occurredAt)

	if got := atomic.LoadInt64(&r.AuditJournalSizeBytes); got != 4096 {
		t.Fatalf("AuditJournalSizeBytes = %v, want 4096", got)
	}
	if got := atomic.LoadInt64(&r.AuditJournalLastWriteUnix); got != 100 {
		t.Fatalf("AuditJournalLastWriteUnix = %v, want 100", got)
	}
}

func TestMetricsRegistry_APIRequestDurationHistogram(t *testing.T) {
	r := NewMetricsRegistry()

	r.RecordAPIRequestDuration(7 * time.Millisecond)
	r.RecordAPIRequestDuration(150 * time.Millisecond)

	if got := atomic.LoadUint64(&r.APIRequestDurationCount); got != 2 {
		t.Fatalf("APIRequestDurationCount = %v, want 2", got)
	}
	if got := atomic.LoadUint64(&r.APIRequestDurationSumUsec); got != 157_000 {
		t.Fatalf("APIRequestDurationSumUsec = %v, want 157000", got)
	}
	if got := atomic.LoadUint64(&r.APIRequestDurationBuckets[1]); got != 1 {
		t.Fatalf("10ms bucket = %v, want 1", got)
	}
	if got := atomic.LoadUint64(&r.APIRequestDurationBuckets[5]); got != 1 {
		t.Fatalf("250ms bucket = %v, want 1", got)
	}
}
