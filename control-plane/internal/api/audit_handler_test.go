package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
)

func TestAuditHandler_ReturnsCorrectJSON(t *testing.T) {
	writer := audit.NewMemoryWriter()
	qSvc := audit.NewQueryService(writer)
	handler := NewAuditHandler(qSvc, nil)

	// Write one event
	writer.Write(context.Background(), audit.Event{
		EventID:    "e1",
		Actor:      "admin",
		Action:     "login",
		Result:     "success",
		OccurredAt: time.Now().UTC(),
	})

	req := httptest.NewRequest("GET", "/api/v1/audit?action=login&limit=10", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	var res audit.QueryResult
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}

	if res.Total != 1 {
		t.Errorf("Expected total 1, got %d", res.Total)
	}
	if len(res.Records) != 1 {
		t.Fatalf("Expected 1 record, got %d", len(res.Records))
	}
	if res.Records[0].EventID != "e1" {
		t.Errorf("Expected event e1, got %s", res.Records[0].EventID)
	}
}

func TestAuditHandlerRejectsInvalidTimeFilter(t *testing.T) {
	handler := NewAuditHandler(audit.NewQueryService(audit.NewMemoryWriter()), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit?after=not-a-time", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid timestamp, got %d", rr.Code)
	}
}

func TestAuditHandlerRejectsOversizedLimit(t *testing.T) {
	handler := NewAuditHandler(audit.NewQueryService(audit.NewMemoryWriter()), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit?limit=501", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized limit, got %d", rr.Code)
	}
}

func TestAuditHandlerRejectsInvalidCursor(t *testing.T) {
	handler := NewAuditHandler(audit.NewQueryService(audit.NewMemoryWriter()), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit?cursor=not-base64", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid cursor, got %d", rr.Code)
	}
}
