package audit

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
)

type failingWriter struct{}

func (f failingWriter) Write(_ context.Context, _ Event) error {
	return errors.New("writer boom")
}

func TestEmitTargetRuntimeEvent_GeneratesUniqueEventIDs(t *testing.T) {
	writer := NewMemoryWriter()
	ctx := context.Background()

	EmitTargetRuntimeEvent(ctx, writer, "tester", "publish", "pub-1", "success", nil)
	EmitTargetRuntimeEvent(ctx, writer, "tester", "publish", "pub-1", "success", nil)

	events := writer.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].EventID == events[1].EventID {
		t.Fatalf("expected unique event IDs, got duplicate %q", events[0].EventID)
	}
}

func TestNewEventID_GeneratesUniqueEventIDs(t *testing.T) {
	first := NewEventID("storage_pool_create", "pool-1")
	second := NewEventID("storage_pool_create", "pool-1")
	if first == second {
		t.Fatalf("expected unique event IDs, got duplicate %q", first)
	}
}

func TestEmitTargetAccessPolicyEvent_GeneratesUniqueEventIDs(t *testing.T) {
	writer := NewMemoryWriter()
	ctx := context.Background()

	EmitTargetAccessPolicyEvent(ctx, writer, "tester", "replace", "pub-1", "success", nil)
	EmitTargetAccessPolicyEvent(ctx, writer, "tester", "replace", "pub-1", "success", nil)

	events := writer.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].EventID == events[1].EventID {
		t.Fatalf("expected unique event IDs, got duplicate %q", events[0].EventID)
	}
}

func TestEmitTargetRuntimeEvent_LogsFailureWhenWriterFails(t *testing.T) {
	var buf bytes.Buffer
	original := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(original)

	EmitTargetRuntimeEvent(context.Background(), failingWriter{}, "tester", "publish", "pub-1", "success", nil)
	if !strings.Contains(buf.String(), "AUDIT WRITE FAILURE") {
		t.Fatalf("expected audit write failure log, got %q", buf.String())
	}
}
