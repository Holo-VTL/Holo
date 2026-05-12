package metrics

import (
	"math"
	"sync/atomic"
	"time"
)

var APIRequestDurationBucketMicros = [...]uint64{5_000, 10_000, 25_000, 50_000, 100_000, 250_000, 500_000, 1_000_000, 2_500_000, 5_000_000, 10_000_000}

type MetricsRegistry struct {
	PublicationsActive        int64
	PublicationsTotal         int64
	AuditEventsTotal          int64
	AuditWriteFailures        int64
	AuditJournalFailed        int64
	AuditParseFailures        int64
	AuditJournalSizeBytes     int64
	AuditJournalLastWriteUnix int64
	ScsiSenseErrors           int64
	DedupHitsTotal            int64
	CompressionRatio          uint64 // stored as float64 bits
	HealthStatus              int64

	APIRequestDurationBuckets [len(APIRequestDurationBucketMicros)]uint64
	APIRequestDurationCount   uint64
	APIRequestDurationSumUsec uint64
}

func NewMetricsRegistry() *MetricsRegistry {
	return &MetricsRegistry{}
}

// TelemetrySink is an interface for the metrics registry to accept data-plane telemetry events.
type TelemetrySink interface {
	RecordDedupHit()
	RecordCompressionRatio(ratio float64)
	RecordScsiSenseError()
}

var _ TelemetrySink = (*MetricsRegistry)(nil)

func (r *MetricsRegistry) RecordDedupHit() {
	atomic.AddInt64(&r.DedupHitsTotal, 1)
}

func (r *MetricsRegistry) RecordCompressionRatio(ratio float64) {
	atomic.StoreUint64(&r.CompressionRatio, math.Float64bits(ratio))
}

func (r *MetricsRegistry) GetCompressionRatio() float64 {
	return math.Float64frombits(atomic.LoadUint64(&r.CompressionRatio))
}

func (r *MetricsRegistry) RecordScsiSenseError() {
	atomic.AddInt64(&r.ScsiSenseErrors, 1)
}

func (r *MetricsRegistry) RecordPublicationPublish() {
	atomic.AddInt64(&r.PublicationsActive, 1)
	atomic.AddInt64(&r.PublicationsTotal, 1)
}

func (r *MetricsRegistry) RecordPublicationUnpublish() {
	for {
		current := atomic.LoadInt64(&r.PublicationsActive)
		if current <= 0 {
			atomic.StoreInt64(&r.PublicationsActive, 0)
			return
		}
		if atomic.CompareAndSwapInt64(&r.PublicationsActive, current, current-1) {
			return
		}
	}
}

func (r *MetricsRegistry) RecordAuditEvent() {
	atomic.AddInt64(&r.AuditEventsTotal, 1)
}

func (r *MetricsRegistry) RecordAuditWriteFailure() {
	atomic.AddInt64(&r.AuditWriteFailures, 1)
	atomic.StoreInt64(&r.AuditJournalFailed, 1)
}

func (r *MetricsRegistry) RecordAuditJournalSuccess() {
	atomic.StoreInt64(&r.AuditJournalFailed, 0)
}

func (r *MetricsRegistry) RecordAuditJournalParseFailures(count int64) {
	if count <= 0 {
		return
	}
	atomic.AddInt64(&r.AuditParseFailures, count)
}

func (r *MetricsRegistry) RecordAuditJournalStats(sizeBytes int64, occurredAt time.Time) {
	if sizeBytes < 0 {
		sizeBytes = 0
	}
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	atomic.StoreInt64(&r.AuditJournalSizeBytes, sizeBytes)
	atomic.StoreInt64(&r.AuditJournalLastWriteUnix, occurredAt.Unix())
}

func (r *MetricsRegistry) SetAuditEventsTotal(total int64) {
	if total < 0 {
		total = 0
	}
	atomic.StoreInt64(&r.AuditEventsTotal, total)
}

func (r *MetricsRegistry) RecordAPIRequestDuration(duration time.Duration) {
	if duration < 0 {
		duration = 0
	}
	usec := uint64(duration / time.Microsecond)
	atomic.AddUint64(&r.APIRequestDurationCount, 1)
	atomic.AddUint64(&r.APIRequestDurationSumUsec, usec)
	for i, upper := range APIRequestDurationBucketMicros {
		if usec <= upper {
			atomic.AddUint64(&r.APIRequestDurationBuckets[i], 1)
			return
		}
	}
}
