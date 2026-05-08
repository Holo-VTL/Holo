package metrics

import (
	"math"
	"sync/atomic"
)

type MetricsRegistry struct {
	PublicationsActive int64
	PublicationsTotal  int64
	AuditEventsTotal   int64
	AuditWriteFailures int64
	AuditJournalFailed int64
	ScsiSenseErrors    int64
	DedupHitsTotal     int64
	CompressionRatio   uint64 // stored as float64 bits
	HealthStatus       int64
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

func (r *MetricsRegistry) SetAuditEventsTotal(total int64) {
	if total < 0 {
		total = 0
	}
	atomic.StoreInt64(&r.AuditEventsTotal, total)
}
