package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTargetAccessEndpoints(t *testing.T) {
	srv := newTestServer(t)

	chainReq := newAuthedRequest(http.MethodPost, "/v1/resources/chain", bytes.NewBufferString(`{"poolId":"pool-1","poolName":"pool-1","capacityBytes":1073741824,"libraryId":"lib-1","libraryName":"lib-1","driveId":"drive-1","driveSlot":1,"cartridgeId":"car-1","barcode":"B001"}`))
	chainResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(chainResp, chainReq)
	if chainResp.Code != http.StatusCreated {
		t.Fatalf("expected chain create 201, got %d", chainResp.Code)
	}

	publishReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications", bytes.NewBufferString(`{"poolId":"pool-1","libraryId":"lib-1","driveId":"drive-1","cartridgeId":"car-1","targetIqn":"iqn.2026-04.ai.holo:acl-test","actor":"tester"}`))
	publishResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(publishResp, publishReq)
	if publishResp.Code != http.StatusAccepted {
		t.Fatalf("expected publish 202, got %d", publishResp.Code)
	}

	var publication map[string]any
	if err := json.Unmarshal(publishResp.Body.Bytes(), &publication); err != nil {
		t.Fatalf("decode publish response failed: %v", err)
	}
	publicationID, _ := publication["publicationId"].(string)
	if publicationID == "" {
		t.Fatal("publication id missing")
	}

	replaceReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications/"+publicationID+"/access-rules", bytes.NewBufferString(`{"actor":"tester","rules":[{"initiator":"iqn.1993-08.org.debian:01:init-a","permission":"allow","priority":100},{"initiator":"iqn.1993-08.org.debian:01:init-b","permission":"deny","priority":100}]}`))
	replaceResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(replaceResp, replaceReq)
	if replaceResp.Code != http.StatusOK {
		t.Fatalf("expected replace rules 200, got %d", replaceResp.Code)
	}

	listRulesReq := newAuthedRequest(http.MethodGet, "/v1/targets/publications/"+publicationID+"/access-rules", nil)
	listRulesResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(listRulesResp, listRulesReq)
	if listRulesResp.Code != http.StatusOK {
		t.Fatalf("expected list rules 200, got %d", listRulesResp.Code)
	}
	if !strings.Contains(listRulesResp.Body.String(), "init-a") {
		t.Fatalf("expected listed rules to contain init-a, got %s", listRulesResp.Body.String())
	}

	authorizeReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications/"+publicationID+"/authorize", bytes.NewBufferString(`{"initiator":"iqn.1993-08.org.debian:01:init-a","actor":"tester"}`))
	authorizeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(authorizeResp, authorizeReq)
	if authorizeResp.Code != http.StatusOK {
		t.Fatalf("expected authorize 200, got %d", authorizeResp.Code)
	}
	if !strings.Contains(authorizeResp.Body.String(), `"decision":"allow"`) {
		t.Fatalf("expected allow decision, got %s", authorizeResp.Body.String())
	}

	visibleReq := newAuthedRequest(http.MethodGet, "/v1/targets/visible?initiator=iqn.1993-08.org.debian:01:init-a&actor=tester", nil)
	visibleResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(visibleResp, visibleReq)
	if visibleResp.Code != http.StatusOK {
		t.Fatalf("expected visible query 200, got %d", visibleResp.Code)
	}
	if !strings.Contains(visibleResp.Body.String(), publicationID) {
		t.Fatalf("expected visible response to contain publication id, got %s", visibleResp.Body.String())
	}

	rollbackReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications/"+publicationID+"/access-rollback?actor=tester", nil)
	rollbackResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(rollbackResp, rollbackReq)
	if rollbackResp.Code != http.StatusOK {
		t.Fatalf("expected rollback 200, got %d", rollbackResp.Code)
	}

	healthReq := newAuthedRequest(http.MethodGet, "/healthz", nil)
	healthResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(healthResp, healthReq)
	if healthResp.Code != http.StatusOK && healthResp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected health 200 or 503, got %d", healthResp.Code)
	}
	if !strings.Contains(healthResp.Body.String(), "target-access-policy") {
		t.Fatalf("expected health payload to include target-access-policy component, got %s", healthResp.Body.String())
	}
}

func TestAuthorizeRejectsMalformedPayload(t *testing.T) {
	srv := newTestServer(t)

	resp := httptest.NewRecorder()
	req := newAuthedRequest(http.MethodPost, "/v1/targets/publications/pub-unknown/authorize", bytes.NewBufferString(`{"actor":"tester"}`))
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected malformed request to return 400, got %d", resp.Code)
	}
}

func TestAccessRulesRejectInvalidPermission(t *testing.T) {
	srv := newTestServer(t)
	req := newAuthedRequest(http.MethodPost, "/v1/targets/publications/pub-unknown/access-rules", bytes.NewBufferString(`{"actor":"tester","rules":[{"initiator":"iqn.1993-08.org.debian:01:init-a","permission":"admin","priority":100}]}`))
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid permission 400, got %d body=%s", resp.Code, resp.Body.String())
	}
}
