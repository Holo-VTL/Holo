package audit

import (
	"context"
	"sync"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/metrics"
)

const memoryWriterLimit = 10000

type Event struct {
	EventID    string         `json:"eventId"`
	Actor      string         `json:"actor"`
	Action     string         `json:"action"`
	ObjectType string         `json:"objectType"`
	ObjectID   string         `json:"objectId"`
	Result     string         `json:"result"`
	Details    map[string]any `json:"details,omitempty"`
	OccurredAt time.Time      `json:"occurredAt"`
}

type Writer interface {
	Write(ctx context.Context, event Event) error
}

type MemoryWriter struct {
	mu     sync.RWMutex
	events []Event
	next   int
}

func NewMemoryWriter() *MemoryWriter {
	return &MemoryWriter{events: make([]Event, 0, memoryWriterLimit)}
}

func (w *MemoryWriter) Write(_ context.Context, event Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.events) < memoryWriterLimit {
		w.events = append(w.events, event)
		return nil
	}
	w.events[w.next] = event
	w.next = (w.next + 1) % memoryWriterLimit
	return nil
}

func (w *MemoryWriter) Events() []Event {
	w.mu.RLock()
	defer w.mu.RUnlock()
	cp := make([]Event, len(w.events))
	if len(w.events) < memoryWriterLimit || w.next == 0 {
		copy(cp, w.events)
		return cp
	}
	copy(cp, w.events[w.next:])
	copy(cp[len(w.events)-w.next:], w.events[:w.next])
	return cp
}

// PersistentWriter dual-writes to an in-memory buffer and a JournalStore,
// and increments the metrics registry on each event.
type PersistentWriter struct {
	MemoryStore *MemoryWriter
	Journal     *JournalStore
	Metrics     *metrics.MetricsRegistry
}

func NewPersistentWriter(mem *MemoryWriter, journal *JournalStore, m *metrics.MetricsRegistry) *PersistentWriter {
	return &PersistentWriter{
		MemoryStore: mem,
		Journal:     journal,
		Metrics:     m,
	}
}

func (w *PersistentWriter) Write(ctx context.Context, evt Event) error {
	if w.Journal != nil {
		if err := w.Journal.Append(evt); err != nil {
			if w.Metrics != nil {
				w.Metrics.RecordAuditWriteFailure()
			}
			return err
		}
		if w.Metrics != nil {
			w.Metrics.RecordAuditJournalSuccess()
		}
	}

	if w.MemoryStore != nil {
		if err := w.MemoryStore.Write(ctx, evt); err != nil {
			return err
		}
	}

	if w.Metrics != nil {
		w.Metrics.RecordAuditEvent()
	}

	return nil
}
