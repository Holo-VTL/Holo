package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/metrics"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
)

func TestHealthHandler_AllHealthy_Returns200(t *testing.T) {
	registry := metrics.NewMetricsRegistry()
	h := orchestration.NewHealthServiceWithConfig(nil, nil, nil, registry, "", "in-memory")
	ops := NewOpsHandler(h, nil, 3260)

	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()

	ops.handleHealth(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d body=%s", rr.Code, rr.Body.String())
	}

	var res orchestration.HealthSummary
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}

	if res.Status != "healthy" {
		t.Fatalf("expected healthy summary, got %s", res.Status)
	}
	if !containsComponent(res.Components, "database") ||
		!containsComponent(res.Components, "dataPlane") ||
		!containsComponent(res.Components, "tcmuRunner") {
		t.Fatalf("expected database/dataPlane/tcmuRunner components, got %+v", res.Components)
	}
}

func TestHealthHandler_DatabaseDown_Returns503(t *testing.T) {
	registry := metrics.NewMetricsRegistry()
	h := orchestration.NewHealthServiceWithConfig(
		nil,
		nil,
		nil,
		registry,
		"postgres://127.0.0.1:1/holo?sslmode=disable",
		"in-memory",
	)
	ops := NewOpsHandler(h, nil, 3260)

	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()

	ops.handleHealth(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "\"database\"") || !strings.Contains(rr.Body.String(), "\"down\"") {
		t.Fatalf("expected degraded database status in response, got %s", rr.Body.String())
	}
}

func TestHealthHandler_AuditJournalFailureReturns503(t *testing.T) {
	registry := metrics.NewMetricsRegistry()
	registry.RecordAuditWriteFailure()
	h := orchestration.NewHealthServiceWithConfig(nil, nil, nil, registry, "", "in-memory")
	ops := NewOpsHandler(h, nil, 3260)

	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()

	ops.handleHealth(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "\"audit-log\"") || !strings.Contains(rr.Body.String(), "\"down\"") {
		t.Fatalf("expected degraded audit-log status in response, got %s", rr.Body.String())
	}
}

func containsComponent(items []orchestration.ComponentHealth, name string) bool {
	for _, item := range items {
		if item.Name == name {
			return true
		}
	}
	return false
}
