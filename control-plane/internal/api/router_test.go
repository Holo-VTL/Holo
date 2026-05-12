package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/config"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestServerPersistsRuntimePublicationsAcrossRestart(t *testing.T) {
	metadataDSN := filepath.Join(t.TempDir(), "metadata.db")
	srv := newTestServerWithMetadata(t, metadataDSN)
	chainReq := newAuthedRequest(
		http.MethodPost,
		"/v1/resources/chain",
		bytes.NewBufferString(`{"poolId":"pool-runtime","poolName":"Pool Runtime","libraryId":"lib-runtime","libraryName":"Library Runtime","driveId":"drive-runtime","driveSlot":1,"cartridgeId":"VTA901L06","barcode":"VTA901L06"}`),
	)
	chainResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(chainResp, chainReq)
	if chainResp.Code != http.StatusCreated {
		t.Fatalf("expected chain create 201, got %d body=%s", chainResp.Code, chainResp.Body.String())
	}
	firstPublications := listPublications(t, srv)
	if len(firstPublications) == 0 {
		t.Fatalf("expected initial in-memory publications to be created")
	}

	restarted := newTestServerWithMetadata(t, metadataDSN)
	restartedPublications := listPublications(t, restarted)
	if len(restartedPublications) != len(firstPublications) {
		t.Fatalf("expected runtime publications to survive restart, before=%d after=%d publications=%+v", len(firstPublications), len(restartedPublications), restartedPublications)
	}
}

func TestUpgradeRuntimePublicationRecoveryPreservesResourceIQNs(t *testing.T) {
	metadataDSN := filepath.Join(t.TempDir(), "metadata.db")
	srv := newTestServerWithMetadata(t, metadataDSN)
	chainReq := newAuthedRequest(
		http.MethodPost,
		"/v1/resources/chain",
		bytes.NewBufferString(`{"poolId":"pool-upgrade","poolName":"Pool Upgrade","libraryId":"lib-upgrade","libraryName":"Library Upgrade","driveId":"drive-upgrade","driveSlot":1,"cartridgeId":"VTA902L06","barcode":"VTA902L06"}`),
	)
	chainResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(chainResp, chainReq)
	if chainResp.Code != http.StatusCreated {
		t.Fatalf("expected chain create 201, got %d body=%s", chainResp.Code, chainResp.Body.String())
	}
	initial := listPublications(t, srv)
	if len(initial) == 0 {
		t.Fatalf("expected initial publications")
	}
	if _, err := srv.metadataDB.Exec(`DELETE FROM target_publications`); err != nil {
		t.Fatalf("delete runtime rows: %v", err)
	}

	if err := srv.resources.ensureUpgradeRuntimePublications(httptest.NewRequest(http.MethodGet, "/", nil).Context()); err != nil {
		t.Fatalf("recover upgrade publications: %v", err)
	}
	recovered := listPublications(t, srv)
	if len(recovered) != len(initial) {
		t.Fatalf("expected recovered publication count=%d, got %d publications=%+v", len(initial), len(recovered), recovered)
	}
	seen := make(map[string]struct{}, len(recovered))
	for _, publication := range recovered {
		seen[publication.TargetIQN] = struct{}{}
	}
	for _, publication := range initial {
		if _, ok := seen[publication.TargetIQN]; !ok {
			t.Fatalf("expected upgrade recovery to preserve iqn %q, got %+v", publication.TargetIQN, recovered)
		}
	}
}

func TestNewServerWithConfigEReturnsMetadataInitializationError(t *testing.T) {
	cfg := config.Load()
	cfg.APIKey = testAPIKey
	cfg.LogDir = t.TempDir()
	cfg.MetadataDSN = filepath.Join(t.TempDir(), "missing", "metadata.db")
	cfg.TargetRuntimeMode = "in-memory"
	cfg.TargetRuntimeUseSudo = false
	if _, err := NewServerWithConfigE(cfg); err == nil {
		t.Fatal("expected server construction to fail for unusable metadata dsn")
	}
}

func TestNewServerWithConfigEUsesConfiguredLogDirForAudit(t *testing.T) {
	cfg := config.Load()
	cfg.APIKey = testAPIKey
	cfg.LogDir = t.TempDir()
	cfg.MetadataDSN = filepath.Join(t.TempDir(), "metadata.db")
	cfg.TargetRuntimeMode = "in-memory"
	cfg.TargetRuntimeUseSudo = false

	srv, err := NewServerWithConfigE(cfg)
	if err != nil {
		t.Fatalf("new server failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	if _, err := os.Stat(filepath.Join(cfg.LogDir, "audit.jsonl")); err != nil {
		t.Fatalf("expected audit journal under configured log dir: %v", err)
	}
}

func TestPathWithinBaseRejectsSiblingPrefix(t *testing.T) {
	base := filepath.Clean("/srv/holo/ui")
	inside := filepath.Clean("/srv/holo/ui/assets/app.js")
	sibling := filepath.Clean("/srv/holo/ui2/index.html")

	if !pathWithinBase(base, inside) {
		t.Fatalf("expected inside path to be accepted")
	}
	if pathWithinBase(base, sibling) {
		t.Fatalf("expected sibling prefix path to be rejected")
	}
}

func TestRouterAddsSecurityHeaders(t *testing.T) {
	srv := newTestServer(t)
	req := newAuthedRequest(http.MethodGet, "/healthz", nil)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	for _, name := range []string{
		"X-Content-Type-Options",
		"X-Frame-Options",
		"Referrer-Policy",
		"Content-Security-Policy",
	} {
		if got := resp.Header().Get(name); got == "" {
			t.Fatalf("expected %s header", name)
		}
	}
}

func TestUIRouteExemptionRejectsEncodedTraversal(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/ui/%2e%2e/v1/system/overview", nil)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	if resp.Code == http.StatusOK {
		t.Fatalf("encoded traversal must not be served as UI content")
	}
}

func TestRouterRecordsRequestDurationMetrics(t *testing.T) {
	srv := newTestServer(t)

	healthReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(healthResp, healthReq)

	metricsReq := newAuthedRequest(http.MethodGet, "/metrics", nil)
	metricsResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(metricsResp, metricsReq)

	body := metricsResp.Body.String()
	if !strings.Contains(body, "holo_api_request_duration_seconds_count 1") {
		t.Fatalf("expected one recorded request before scrape, got %s", body)
	}
}

func listPublications(t *testing.T, srv *Server) []domain.TargetPublication {
	t.Helper()
	req := newAuthedRequest(http.MethodGet, "/v1/targets/publications", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected publications list 200, got %d body=%s", resp.Code, resp.Body.String())
	}
	var publications []domain.TargetPublication
	if err := json.Unmarshal(resp.Body.Bytes(), &publications); err != nil {
		t.Fatalf("decode publications: %v", err)
	}
	return publications
}
