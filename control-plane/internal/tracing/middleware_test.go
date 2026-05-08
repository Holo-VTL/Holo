package tracing

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTraceMiddleware_ExtractsTraceId(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	var extracted string
	handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extracted = TraceID(r.Context())
	}))

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if extracted != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("Expected extracted traceId '4bf92f3577b34da6a3ce929d0e0e4736', got '%s'", extracted)
	}
}

func TestTraceMiddleware_GeneratesNewTraceId(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	var extracted string
	handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extracted = TraceID(r.Context())
	}))

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if extracted == "" {
		t.Errorf("Expected generated traceId, got empty string")
	}
	if len(extracted) != 32 {
		t.Errorf("Expected 32 character traceId, got '%s'", extracted)
	}
}
