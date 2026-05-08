package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
	if strings.TrimSpace(os.Getenv("HOLO_MEDIA_STATE_DIR")) == "" {
		t.Setenv("HOLO_MEDIA_STATE_DIR", t.TempDir())
	}
	t.Setenv("HOLO_METADATA_DSN", metadataDSN)
	t.Setenv("HOLO_STRICT_STORAGE_FLOW", "0")
	cfg := config.Load()
	cfg.APIKey = testAPIKey
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
	return req
}
