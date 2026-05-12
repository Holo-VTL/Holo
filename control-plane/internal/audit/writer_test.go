package audit

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/metrics"
)

type failingJournalAppender struct {
	err error
}

func (a failingJournalAppender) Append(Event) error {
	return a.err
}

func TestMemoryWriterCapsWithFreshBackingArray(t *testing.T) {
	w := NewMemoryWriter()
	for i := 0; i < 10001; i++ {
		if err := w.Write(context.Background(), Event{
			EventID:    "event",
			Actor:      "test",
			Action:     "write",
			ObjectType: "unit",
			ObjectID:   "id",
			Result:     "success",
			OccurredAt: time.Unix(int64(i), 0),
		}); err != nil {
			t.Fatalf("write event %d: %v", i, err)
		}
	}
	if len(w.events) != 10000 {
		t.Fatalf("expected capped events, got %d", len(w.events))
	}
	if cap(w.events) != 10000 {
		t.Fatalf("expected fresh backing array cap 10000, got %d", cap(w.events))
	}
	events := w.Events()
	if got := events[0].OccurredAt.Unix(); got != 1 {
		t.Fatalf("expected oldest retained event timestamp 1, got %d", got)
	}
	if got := events[len(events)-1].OccurredAt.Unix(); got != 10000 {
		t.Fatalf("expected newest retained event timestamp 10000, got %d", got)
	}
}

func TestPersistentWriterDoesNotWriteMemoryWhenJournalAppendFails(t *testing.T) {
	appendErr := errors.New("append failed")
	mem := NewMemoryWriter()
	registry := metrics.NewMetricsRegistry()
	writer := &PersistentWriter{
		MemoryStore: mem,
		Journal:     failingJournalAppender{err: appendErr},
		Metrics:     registry,
	}

	err := writer.Write(context.Background(), Event{
		EventID:    "event",
		Actor:      "test",
		Action:     "write",
		ObjectType: "unit",
		ObjectID:   "id",
		Result:     "success",
		OccurredAt: time.Now().UTC(),
	})

	if !errors.Is(err, appendErr) {
		t.Fatalf("expected append error, got %v", err)
	}
	if got := len(mem.Events()); got != 0 {
		t.Fatalf("expected memory writer to remain empty, got %d events", got)
	}
	if got := atomic.LoadInt64(&registry.AuditWriteFailures); got != 1 {
		t.Fatalf("expected one audit write failure metric, got %d", got)
	}
}

func TestPersistentWriterRecordsJournalSizeAndLagStats(t *testing.T) {
	journal, err := NewJournalStore(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	registry := metrics.NewMetricsRegistry()
	writer := NewPersistentWriter(NewMemoryWriter(), journal, registry)

	occurredAt := time.Unix(100, 0).UTC()
	err = writer.Write(context.Background(), Event{
		EventID:    "event",
		Actor:      "test",
		Action:     "write",
		ObjectType: "unit",
		ObjectID:   "id",
		Result:     "success",
		OccurredAt: occurredAt,
	})
	if err != nil {
		t.Fatalf("write event: %v", err)
	}

	if got := atomic.LoadInt64(&registry.AuditJournalSizeBytes); got <= 0 {
		t.Fatalf("expected positive journal size, got %d", got)
	}
	if got := atomic.LoadInt64(&registry.AuditJournalLastWriteUnix); got != occurredAt.Unix() {
		t.Fatalf("expected last write unix %d, got %d", occurredAt.Unix(), got)
	}
}
