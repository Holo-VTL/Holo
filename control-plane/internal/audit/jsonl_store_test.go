package audit

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalStore_AppendAndRead(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "holo_audit_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "audit.jsonl")
	store, err := NewJournalStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	evt := Event{
		EventID:    "test-1",
		Actor:      "system",
		Action:     "test",
		ObjectType: "node",
		ObjectID:   "node-1",
		Result:     "success",
		OccurredAt: time.Now().UTC(),
	}

	if err := store.Append(evt); err != nil {
		t.Fatal(err)
	}

	events, err := store.ReadAll(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].EventID != "test-1" {
		t.Errorf("expected EventID 'test-1', got '%s'", events[0].EventID)
	}
}
