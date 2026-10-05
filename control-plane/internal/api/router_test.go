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
	createResourceFlowFixture(t, srv, "pool-runtime", "lib-runtime", "drive-runtime", "VTA901L06", "VTA901L06")
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

func TestManagementAuthenticationModesPreserveDefaultNoLogin(t *testing.T) {
	noLogin := newTestServerWithAPIKey(t, "")
	list := httptest.NewRequest(http.MethodGet, "/v1/libraries", nil)
	listResp := httptest.NewRecorder()
	noLogin.Router().ServeHTTP(listResp, list)
	if listResp.Code != http.StatusOK {
		t.Fatalf("default no-login mode should allow existing management use, got %d", listResp.Code)
	}

	create := httptest.NewRequest(http.MethodPost, "http://example.com/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-no-login","name":"No Login"}`))
	create.Header.Set("Content-Type", "application/json")
	createResp := httptest.NewRecorder()
	noLogin.Router().ServeHTTP(createResp, create)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("default no-login resource creation should work without credential steps, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	configured := newTestServerWithAPIKey(t, "configured-test-key")
	unauthorized := httptest.NewRequest(http.MethodGet, "/v1/libraries", nil)
	unauthorizedResp := httptest.NewRecorder()
	configured.Router().ServeHTTP(unauthorizedResp, unauthorized)
	if unauthorizedResp.Code != http.StatusUnauthorized {
		t.Fatalf("configured key mode must still require the key, got %d", unauthorizedResp.Code)
	}
	valid := httptest.NewRequest(http.MethodGet, "/v1/libraries", nil)
	valid.Header.Set("X-HOLO-API-Key", "configured-test-key")
	validResp := httptest.NewRecorder()
	configured.Router().ServeHTTP(validResp, valid)
	if validResp.Code != http.StatusOK {
		t.Fatalf("configured key mode should accept the configured key, got %d", validResp.Code)
	}
}

func TestStandardResourceFixtureUsesNormalAPIs(t *testing.T) {
	srv := newTestServer(t)
	createStandardResourceFixture(t, srv, "normal-flow")

	library, err := srv.resources.repo.FindLibrary(httptest.NewRequest(http.MethodGet, "/", nil).Context(), "lib-normal-flow")
	if err != nil {
		t.Fatalf("normal resource flow did not create its library: %v", err)
	}
	if library.LibraryID != "lib-normal-flow" {
		t.Fatalf("unexpected library created: %+v", library)
	}
}

func TestServerRestartCleanupFailureBlocksAutomaticPublication(t *testing.T) {
	metadataDSN := filepath.Join(t.TempDir(), "metadata.db")
	srv := newTestServerWithMetadata(t, metadataDSN)
	createResourceFlowFixture(t, srv, "pool-restart", "lib-restart", "drive-restart", "VTA903L06", "VTA903L06")
	if _, err := srv.metadataDB.Exec(`DELETE FROM target_publications`); err != nil {
		t.Fatal(err)
	}
	securityHelper := filepath.Join(t.TempDir(), "security-helper")
	loopbackHelper := filepath.Join(t.TempDir(), "loopback-helper")
	for path, script := range map[string]string{
		securityHelper: "#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"ok\":true,\"code\":\"ok\",\"ready\":true}'\n",
		loopbackHelper: "#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"version\":1,\"ok\":false,\"reason\":\"cleanup_failed\"}'\n",
	} {
		if err := os.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOLO_ISCSI_SECURITY_HELPER", securityHelper)
	t.Setenv("HOLO_LOCAL_LOOPBACK_HELPER", loopbackHelper)
	cfg := config.Load()
	cfg.MetadataDSN = metadataDSN
	cfg.LogDir = t.TempDir()
	cfg.TargetRuntimeMode = "tcmu"
	cfg.TargetRuntimeUseSudo = false
	restarted, err := NewServerWithConfigE(cfg)
	if restarted != nil {
		_ = restarted.Close()
	}
	if err == nil {
		t.Fatal("startup continued after unverified local mapping cleanup")
	}
	if publications := listPublications(t, srv); len(publications) != 0 {
		t.Fatalf("startup fallback created publications after cleanup failure: %+v", publications)
	}
}

func TestUpgradeRuntimePublicationRecoveryPreservesResourceIQNs(t *testing.T) {
	metadataDSN := filepath.Join(t.TempDir(), "metadata.db")
	srv := newTestServerWithMetadata(t, metadataDSN)
	createResourceFlowFixture(t, srv, "pool-upgrade", "lib-upgrade", "drive-upgrade", "VTA902L06", "VTA902L06")
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
	cfg.ISCSISecretKeyPath = filepath.Join(t.TempDir(), "vault.key")
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
	if got := srv.ops.support.ISCSISecretKeyPath; got != cfg.ISCSISecretKeyPath {
		t.Fatalf("support bundle must use the configured secret-key path, got %q want %q", got, cfg.ISCSISecretKeyPath)
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
