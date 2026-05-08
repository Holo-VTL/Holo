package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPolicyHandler_AccessPolicyRejectsNilBody(t *testing.T) {
	srv := newTestServer(t)
	req := newAuthedRequest(http.MethodPost, "/v1/access-policies", nil)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for nil body, got %d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "invalid request body") {
		t.Fatalf("expected safe invalid request body message, got %s", resp.Body.String())
	}
}

func TestPolicyHandler_RetentionPolicyRejectsNilBody(t *testing.T) {
	srv := newTestServer(t)
	req := newAuthedRequest(http.MethodPost, "/v1/retention-policies", nil)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for nil body, got %d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "invalid request body") {
		t.Fatalf("expected safe invalid request body message, got %s", resp.Body.String())
	}
}

func TestPolicyHandler_AccessPolicyRejectsUnknownField(t *testing.T) {
	srv := newTestServer(t)
	body := `{"policyId":"p-1","scope":"global","subject":"initiator-1","permission":"allow","effectiveFrom":"2026-01-01T00:00:00Z","extra":"x"}`
	req := newAuthedRequest(http.MethodPost, "/v1/access-policies", bytes.NewBufferString(body))
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown field, got %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestPolicyHandler_CreatePoliciesSuccess(t *testing.T) {
	srv := newTestServer(t)
	accessBody := `{"policyId":"p-1","scope":"global","subject":"initiator-1","permission":"allow","effectiveFrom":"2026-01-01T00:00:00Z"}`
	accessReq := newAuthedRequest(http.MethodPost, "/v1/access-policies", bytes.NewBufferString(accessBody))
	accessResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(accessResp, accessReq)
	if accessResp.Code != http.StatusCreated {
		t.Fatalf("expected access policy 201, got %d body=%s", accessResp.Code, accessResp.Body.String())
	}

	lockUntil := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	retentionBody := `{"retentionId":"r-1","cartridgeId":"cart-1","mode":"worm","lockUntil":"` + lockUntil + `","createdBy":"tester"}`
	retentionReq := newAuthedRequest(http.MethodPost, "/v1/retention-policies", bytes.NewBufferString(retentionBody))
	retentionResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(retentionResp, retentionReq)
	if retentionResp.Code != http.StatusCreated {
		t.Fatalf("expected retention policy 201, got %d body=%s", retentionResp.Code, retentionResp.Body.String())
	}
}

func TestPolicyHandler_FailsLoudlyWhenRepoMissing(t *testing.T) {
	h := NewPolicyHandler(nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/access-policies", bytes.NewBufferString(`{"policyId":"p-1","scope":"global","subject":"initiator-1","permission":"allow","effectiveFrom":"2026-01-01T00:00:00Z"}`))
	resp := httptest.NewRecorder()

	h.handleCreateAccessPolicy(resp, req)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when repository wiring is missing, got %d body=%s", resp.Code, resp.Body.String())
	}
}
