package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/config"
)

func TestAuthMiddleware_ProtectsManagementRoutes(t *testing.T) {
	srv := newTestServer(t)

	unauthReq := httptest.NewRequest(http.MethodGet, "/v1/storage/pools", nil)
	unauthResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(unauthResp, unauthReq)
	if unauthResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthenticated management route to return 401, got %d", unauthResp.Code)
	}

	wrongBearerReq := httptest.NewRequest(http.MethodGet, "/v1/storage/pools", nil)
	wrongBearerReq.Header.Set("Authorization", "Bearer wrong-token")
	wrongBearerResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(wrongBearerResp, wrongBearerReq)
	if wrongBearerResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected bad bearer token to return 401, got %d", wrongBearerResp.Code)
	}

	authedReq := httptest.NewRequest(http.MethodGet, "/v1/storage/pools", nil)
	authedReq.Header.Set("Authorization", "Bearer "+testAPIKey)
	authedResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(authedResp, authedReq)
	if authedResp.Code != http.StatusOK {
		t.Fatalf("expected authenticated route to return 200, got %d", authedResp.Code)
	}
}

func TestAuthMiddleware_AllowsManagementRoutesWhenAPIKeyIsEmpty(t *testing.T) {
	cfg := config.Load()
	cfg.APIKey = ""
	cfg.MetadataDSN = t.TempDir() + "/metadata.db"
	cfg.TargetRuntimeMode = "in-memory"
	cfg.TargetRuntimeUseSudo = false
	srv, err := NewServerWithConfigE(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/storage/pools", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 in internal no-login mode, got %d", resp.Code)
	}
}

func TestAuthMiddleware_AllowsHealthWithoutCredentials(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code == http.StatusUnauthorized {
		t.Fatalf("expected /healthz to bypass auth middleware")
	}
}
