package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestTargetDiscoveryEndpoints(t *testing.T) {
	srv := newTestServer(t)

	chainReq := newAuthedRequest(http.MethodPost, "/v1/resources/chain", bytes.NewBufferString(`{"poolId":"pool-1","poolName":"pool-1","capacityBytes":1073741824,"libraryId":"lib-1","libraryName":"lib-1","driveId":"drive-1","driveSlot":1,"cartridgeId":"car-1","barcode":"B001"}`))
	chainResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(chainResp, chainReq)
	if chainResp.Code != http.StatusCreated {
		t.Fatalf("expected chain create 201, got %d", chainResp.Code)
	}

	initiator := "iqn.1993-08.org.debian:01:init-a"
	prepareSecurityTarget(t, srv, "iqn.2026-04.ai.holo:discover-a", []string{initiator})
	prepareSecurityTarget(t, srv, "iqn.2026-04.ai.holo:discover-b", nil)
	publishAResp := publishTargetForDiscoveryTest(t, srv, "iqn.2026-04.ai.holo:discover-a")
	publishTargetForDiscoveryTest(t, srv, "iqn.2026-04.ai.holo:discover-b")

	publicationA := decodePublicationID(t, publishAResp)

	discoverReq := newAuthedRequest(http.MethodGet, "/v1/targets/discovery?initiator=iqn.1993-08.org.debian:01:init-a&actor=tester", nil)
	discoverResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(discoverResp, discoverReq)
	if discoverResp.Code != http.StatusOK {
		t.Fatalf("expected discovery 200, got %d", discoverResp.Code)
	}
	if !strings.Contains(discoverResp.Body.String(), publicationA) {
		t.Fatalf("expected discovery to contain publicationA, got %s", discoverResp.Body.String())
	}
	if strings.Contains(discoverResp.Body.String(), "discover-b") {
		t.Fatalf("expected discovery to exclude publicationB, got %s", discoverResp.Body.String())
	}

	unpublishReq := newAuthedRequest(http.MethodDelete, "/v1/targets/publications/"+publicationA+"?actor=tester", nil)
	unpublishResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(unpublishResp, unpublishReq)
	if unpublishResp.Code != http.StatusAccepted {
		t.Fatalf("expected unpublish 202, got %d", unpublishResp.Code)
	}

	discoverAfterReq := newAuthedRequest(http.MethodGet, "/v1/targets/discovery?initiator=iqn.1993-08.org.debian:01:init-a&actor=tester", nil)
	discoverAfterResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(discoverAfterResp, discoverAfterReq)
	if discoverAfterResp.Code != http.StatusOK {
		t.Fatalf("expected discovery after unpublish 200, got %d", discoverAfterResp.Code)
	}
	if strings.Contains(discoverAfterResp.Body.String(), publicationA) {
		t.Fatalf("expected unpublished publication not discoverable, got %s", discoverAfterResp.Body.String())
	}

	healthReq := newAuthedRequest(http.MethodGet, "/healthz", nil)
	healthResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(healthResp, healthReq)
	if healthResp.Code != http.StatusOK && healthResp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected health 200 or 503, got %d", healthResp.Code)
	}
	if !strings.Contains(healthResp.Body.String(), "target-discovery") {
		t.Fatalf("expected health to include target-discovery component, got %s", healthResp.Body.String())
	}

	auditReq := newAuthedRequest(http.MethodGet, "/v1/audit/events", nil)
	auditResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(auditResp, auditReq)
	if auditResp.Code != http.StatusOK {
		t.Fatalf("expected audit list 200, got %d", auditResp.Code)
	}
	if !strings.Contains(auditResp.Body.String(), "discover_targets") {
		t.Fatalf("expected audit events to include discover_targets action, got %s", auditResp.Body.String())
	}
}

func TestTargetDiscoveryRejectsMalformedQuery(t *testing.T) {
	srv := newTestServer(t)

	badReq := newAuthedRequest(http.MethodGet, "/v1/targets/discovery", nil)
	badResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(badResp, badReq)
	if badResp.Code != http.StatusBadRequest {
		t.Fatalf("expected missing initiator to return 400, got %d", badResp.Code)
	}

	methodReq := newAuthedRequest(http.MethodPost, "/v1/targets/discovery", bytes.NewBufferString(`{"initiator":"iqn.1"}`))
	methodResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(methodResp, methodReq)
	if methodResp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected wrong method to return 405, got %d", methodResp.Code)
	}
}

func publishTargetForDiscoveryTest(t *testing.T, srv *Server, iqn string) *httptest.ResponseRecorder {
	t.Helper()
	req := newAuthedRequest(http.MethodPost, "/v1/targets/publications", bytes.NewBufferString(`{"poolId":"pool-1","libraryId":"lib-1","driveId":"drive-1","cartridgeId":"car-1","targetIqn":"`+iqn+`","actor":"tester"}`))
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusAccepted {
		t.Fatalf("expected publish accepted, got %d", resp.Code)
	}
	return resp
}

func decodePublicationID(t *testing.T, resp *httptest.ResponseRecorder) string {
	t.Helper()
	var publication map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &publication); err != nil {
		t.Fatalf("decode publication response failed: %v", err)
	}
	id, _ := publication["publicationId"].(string)
	if id == "" {
		t.Fatal("publication id missing")
	}
	return id
}

func prepareSecurityTarget(t *testing.T, srv *Server, targetIQN string, initiators []string) {
	t.Helper()
	ctx := context.Background()
	service := srv.security.service
	if err := service.RegisterTarget(ctx, targetIQN, "lib-1", "drive-1", "drive"); err != nil {
		t.Fatalf("register target security identity: %v", err)
	}
	if err := service.SetTargetOffline(ctx, targetIQN, true, "tester"); err != nil {
		t.Fatalf("mark target offline: %v", err)
	}
	binding, err := service.GetBinding(ctx, domain.SecurityScopeTarget, targetIQN)
	if err != nil {
		t.Fatal(err)
	}
	binding.Generation++
	binding.Authentication = &domain.ISCSIAuthenticationPolicy{Mode: domain.ISCSIAuthNone, Initiators: append([]string(nil), initiators...), RestrictInitiators: len(initiators) == 0}
	if err := service.PutBinding(ctx, binding, "tester"); err != nil {
		t.Fatalf("save target initiator allowlist: %v", err)
	}
}
