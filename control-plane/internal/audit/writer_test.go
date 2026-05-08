package audit

import (
	"context"
	"testing"
	"time"
)

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
