package audit

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJournalStoreReopensAfterCloseAndHandlesInvalidInputs(t *testing.T) {
	if err := (*JournalStore)(nil).Close(); err != nil {
		t.Fatalf("closing nil journal should succeed: %v", err)
	}
	var nilStore *JournalStore
	if nilStore.ParseErrors() != 0 || nilStore.SizeBytes() != 0 {
		t.Fatal("nil journal should report zero parse errors and size")
	}
	if _, err := NewJournalStore(t.TempDir()); err == nil {
		t.Fatal("opening a directory as an audit journal should fail")
	}

	store, err := NewJournalStore(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	if err := store.Append(Event{Details: map[string]any{"unsupported": make(chan int)}}); err == nil {
		t.Fatal("unmarshalable event details should fail before writing")
	}
	event := Event{EventID: "reopen", Actor: "system", Action: "test", OccurredAt: time.Now().UTC()}
	if err := store.Append(event); err != nil {
		t.Fatalf("append first event: %v", err)
	}
	if got := store.SizeBytes(); got <= 0 {
		t.Fatalf("expected non-empty journal, got %d bytes", got)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	if got := store.SizeBytes(); got <= 0 {
		t.Fatalf("closed journal should still report file size, got %d", got)
	}
	if err := store.Append(event); err != nil {
		t.Fatalf("append should reopen a closed journal: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close reopened journal: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("closing an already closed journal should succeed: %v", err)
	}
	events, err := store.ReadAll(10)
	if err != nil || len(events) != 2 {
		t.Fatalf("expected two events after reopening, got %d events and err=%v", len(events), err)
	}
}

func TestJournalStoreUsesUnlimitedCurrentFileWhenRotationDisabled(t *testing.T) {
	store, err := NewJournalStore(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.maxBytes = 0
	if err := store.Append(Event{EventID: "no-rotate", Action: "test"}); err != nil {
		t.Fatalf("append with rotation disabled: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, ok := parseAuditArchiveTime(strings.TrimPrefix(entry.Name(), filepath.Base(store.path)+".")); strings.HasPrefix(entry.Name(), filepath.Base(store.path)+".") && ok {
			t.Fatalf("rotation-disabled journal should not create archive %q", entry.Name())
		}
	}
}

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

func TestJournalStorePrunesAuditArchivesOnOpenAndRotate(t *testing.T) {
	t.Setenv("HOLO_AUDIT_MAX_ARCHIVES", "2")
	t.Setenv("HOLO_AUDIT_MAX_BYTES", "1048576")
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "audit.jsonl")
	archivePaths := make([]string, 0, 3)
	for day := 1; day <= 3; day++ {
		createdAt := time.Date(2026, time.September, day, 0, 0, 0, 0, time.UTC)
		archivePath := fmt.Sprintf("%s.%s.%d", path, createdAt.Format("20060102T150405Z"), createdAt.UnixNano())
		if err := os.WriteFile(archivePath, []byte("archived"), 0o640); err != nil {
			t.Fatalf("write archive fixture: %v", err)
		}
		archivePaths = append(archivePaths, archivePath)
	}
	unrelatedPath := path + ".keep"
	if err := os.WriteFile(unrelatedPath, []byte("keep"), 0o640); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}

	store, err := NewJournalStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := os.Stat(archivePaths[0]); !os.IsNotExist(err) {
		t.Fatalf("expected oldest archive to be removed on open, stat err=%v", err)
	}
	for _, archivePath := range archivePaths[1:] {
		if _, err := os.Stat(archivePath); err != nil {
			t.Fatalf("expected recent archive %s to remain: %v", archivePath, err)
		}
	}

	event := Event{Actor: "system", Action: "test", ObjectType: "node", Result: "success", OccurredAt: time.Now().UTC()}
	event.EventID = "event-1"
	if err := store.Append(event); err != nil {
		t.Fatal(err)
	}
	store.maxBytes = store.SizeBytes()
	event.EventID = "event-2"
	if err := store.Append(event); err != nil {
		t.Fatal(err)
	}
	event.EventID = "event-3"
	if err := store.Append(event); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	archiveCount := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(path)+".") {
			if _, ok := parseAuditArchiveTime(strings.TrimPrefix(entry.Name(), filepath.Base(path)+".")); ok {
				archiveCount++
			}
		}
	}
	if archiveCount != 2 {
		t.Fatalf("expected archive count to stay capped at 2 after rotations, got %d", archiveCount)
	}
	if _, err := os.Stat(unrelatedPath); err != nil {
		t.Fatalf("expected unrelated file to remain: %v", err)
	}
}

func TestJournalStore_ReadAllCountsAndLogsMalformedRows(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "audit.jsonl")
	store, err := NewJournalStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	valid := `{"eventId":"test-1","actor":"system","action":"test","objectType":"node","objectId":"node-1","result":"success","occurredAt":"2026-05-11T00:00:00Z"}` + "\n"
	if err := os.WriteFile(dbPath, []byte("{not-json}\n"+valid), 0o640); err != nil {
		t.Fatalf("write audit fixture: %v", err)
	}

	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(original) })

	events, err := store.ReadAll(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != "test-1" {
		t.Fatalf("expected one valid event after malformed row, got %+v", events)
	}
	if got := store.ParseErrors(); got != 1 {
		t.Fatalf("expected one parse error, got %d", got)
	}
	gotLog := logs.String()
	if !strings.Contains(gotLog, "line=1") || !strings.Contains(gotLog, dbPath) {
		t.Fatalf("expected parse failure log with path and line, got %q", gotLog)
	}
}
