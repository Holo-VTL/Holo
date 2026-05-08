package audit

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestQueryServiceReturnsWrittenEvents(t *testing.T) {
	w := NewMemoryWriter()
	s := NewQueryService(w)
	_ = w.Write(context.Background(), Event{EventID: "1", Actor: "ops", Action: "test", ObjectType: "pool", ObjectID: "p1", Result: "success", OccurredAt: time.Now().UTC()})
	res, _ := s.Query(context.Background(), QueryParams{})
	if res.Total != 1 {
		t.Fatalf("expected 1 event, got %d", res.Total)
	}
}

func TestAuditQueryService_FilterByAction(t *testing.T) {
	writer := NewMemoryWriter()

	for i := 0; i < 20; i++ {
		action := "other"
		if i%2 == 0 {
			action = "publish"
		}
		evt := Event{
			EventID:    fmt.Sprintf("evt-%d", i),
			Action:     action,
			Actor:      "test",
			OccurredAt: time.Now(),
		}
		writer.Write(context.Background(), evt)
	}

	qs := NewQueryService(writer)
	res, _ := qs.Query(context.Background(), QueryParams{Action: "publish"})

	if res.Total != 10 {
		t.Errorf("Expected 10 total matches, got %d", res.Total)
	}
	if len(res.Records) != 10 {
		t.Errorf("Expected 10 records, got %d", len(res.Records))
	}
}

func TestAuditQueryService_Pagination(t *testing.T) {
	writer := NewMemoryWriter()

	for i := 0; i < 15; i++ {
		writer.Write(context.Background(), Event{
			EventID: fmt.Sprintf("evt-%d", i),
			Action:  "test",
		})
	}

	qs := NewQueryService(writer)

	// Page 1
	res1, _ := qs.Query(context.Background(), QueryParams{Limit: 10})
	if len(res1.Records) != 10 {
		t.Fatalf("Expected 10 records, got %d", len(res1.Records))
	}
	if res1.NextCursor == "" {
		t.Fatalf("Expected non-empty nextCursor")
	}

	if res1.Records[0].EventID != "evt-14" {
		t.Errorf("Expected first record to be evt-14, got %s", res1.Records[0].EventID)
	}

	// Page 2
	res2, _ := qs.Query(context.Background(), QueryParams{Limit: 10, Cursor: res1.NextCursor})
	if len(res2.Records) != 5 {
		t.Fatalf("Expected 5 records on second page, got %d", len(res2.Records))
	}
	if res2.NextCursor != "" {
		t.Fatalf("Expected empty nextCursor on last page, got %s", res2.NextCursor)
	}

	if res2.Records[4].EventID != "evt-0" {
		t.Errorf("Expected last record to be evt-0, got %s", res2.Records[4].EventID)
	}
}

func TestAuditQueryService_RejectsCursorForDifferentFilter(t *testing.T) {
	writer := NewMemoryWriter()
	for i := 0; i < 12; i++ {
		action := "publish"
		if i%2 == 0 {
			action = "delete"
		}
		_ = writer.Write(context.Background(), Event{
			EventID: fmt.Sprintf("evt-%d", i),
			Action:  action,
		})
	}

	qs := NewQueryService(writer)
	first, err := qs.Query(context.Background(), QueryParams{Action: "publish", Limit: 3})
	if err != nil {
		t.Fatalf("first page failed: %v", err)
	}
	if first.NextCursor == "" {
		t.Fatalf("expected next cursor")
	}

	_, err = qs.Query(context.Background(), QueryParams{Action: "delete", Limit: 3, Cursor: first.NextCursor})
	if err != ErrInvalidCursor {
		t.Fatalf("expected invalid cursor for changed filter, got %v", err)
	}
}

func TestAuditQueryService_RejectsMalformedCursor(t *testing.T) {
	writer := NewMemoryWriter()
	_ = writer.Write(context.Background(), Event{EventID: "evt-1", Action: "publish"})
	qs := NewQueryService(writer)

	_, err := qs.Query(context.Background(), QueryParams{Cursor: "not-base64"})
	if err != ErrInvalidCursor {
		t.Fatalf("expected invalid cursor, got %v", err)
	}
}
