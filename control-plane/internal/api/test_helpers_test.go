package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/config"
)

const testAPIKey = "test-api-key"

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithMetadata(t, t.TempDir()+"/metadata.db")
}

func newTestServerWithMetadata(t *testing.T, metadataDSN string) *Server {
	t.Helper()
	return newTestServerWithAPIKeyAndMetadata(t, metadataDSN, testAPIKey)
}

func newTestServerWithAPIKey(t *testing.T, apiKey string) *Server {
	t.Helper()
	return newTestServerWithAPIKeyAndMetadata(t, filepath.Join(t.TempDir(), "metadata.db"), apiKey)
}

func newTestServerWithAPIKeyAndMetadata(t *testing.T, metadataDSN, apiKey string) *Server {
	t.Helper()
	if strings.TrimSpace(os.Getenv("HOLO_MEDIA_STATE_DIR")) == "" {
		t.Setenv("HOLO_MEDIA_STATE_DIR", t.TempDir())
	}
	t.Setenv("HOLO_RUN_DIR", t.TempDir())
	t.Setenv("HOLO_METADATA_DSN", metadataDSN)
	t.Setenv("HOLO_STRICT_STORAGE_FLOW", "0")
	cfg := config.Load()
	cfg.APIKey = apiKey
	cfg.LogDir = t.TempDir()
	cfg.TargetRuntimeMode = "in-memory"
	cfg.TargetRuntimeUseSudo = false
	srv, err := NewServerWithConfigE(cfg)
	if err != nil {
		t.Fatalf("new test server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func newAuthedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("X-HOLO-API-Key", testAPIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// createStandardResourceFixture builds resources through the normal endpoints.
// Library creation intentionally has no poolId; cartridges reference exactly one pool.
func createStandardResourceFixture(t *testing.T, srv *Server, suffix string) {
	t.Helper()
	createResourceFlowFixture(t, srv, "pool-"+suffix, "lib-"+suffix, "drive-"+suffix, "VTA900L06", "VTA900L06")
}

// createResourceFlowFixture creates a pool, library, drive, and cartridge through
// their supported resource endpoints. Libraries are intentionally independent
// from pools; only cartridges reference a pool.
func createResourceFlowFixture(t *testing.T, srv *Server, poolID, libraryID, driveID, cartridgeID, barcode string) {
	t.Helper()
	post := func(path string, body any) {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode resource fixture request %s: %v", path, err)
		}
		resp := httptest.NewRecorder()
		req := newAuthedRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != http.StatusCreated {
			t.Fatalf("create resource fixture %s: expected 201, got %d body=%s", path, resp.Code, resp.Body.String())
		}
	}
	post("/v1/storage/pools", map[string]any{"poolId": poolID, "name": "Pool " + poolID})
	post("/v1/libraries", map[string]any{"libraryId": libraryID, "name": "Library " + libraryID})
	post("/v1/drives", map[string]any{"driveId": driveID, "libraryId": libraryID, "slot": 1})
	post("/v1/cartridges", map[string]any{
		"poolId": poolID, "libraryId": libraryID, "cartridgeId": cartridgeID,
		"barcode": barcode, "capacityBytes": 1073741824,
	})
}
