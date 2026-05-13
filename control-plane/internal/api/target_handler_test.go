package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTargetPublicationEndpoints(t *testing.T) {
	srv := newTestServer(t)

	chainReq := newAuthedRequest(http.MethodPost, "/v1/resources/chain", bytes.NewBufferString(`{"poolId":"pool-1","poolName":"pool-1","capacityBytes":1073741824,"libraryId":"lib-1","libraryName":"lib-1","driveId":"drive-1","driveSlot":1,"cartridgeId":"car-1","barcode":"B001"}`))
	chainResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(chainResp, chainReq)
	if chainResp.Code != http.StatusCreated {
		t.Fatalf("expected chain create 201, got %d", chainResp.Code)
	}

	pubReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications", bytes.NewBufferString(`{"poolId":"pool-1","libraryId":"lib-1","driveId":"drive-1","cartridgeId":"car-1","targetIqn":"iqn.2026-04.ai.holo:test-handler-drive","actor":"tester"}`))
	pubResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(pubResp, pubReq)
	if pubResp.Code != http.StatusAccepted {
		t.Fatalf("expected publish accepted, got %d", pubResp.Code)
	}

	listReq := newAuthedRequest(http.MethodGet, "/v1/targets/publications", nil)
	listResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("expected list 200, got %d", listResp.Code)
	}
	var publications []struct {
		TargetIQN      string `json:"targetIqn"`
		ConnectedHosts struct {
			Available    bool     `json:"available"`
			HostCount    int      `json:"hostCount"`
			SessionCount int      `json:"sessionCount"`
			Initiators   []string `json:"initiators"`
		} `json:"connectedHosts"`
	}
	if err := json.Unmarshal(listResp.Body.Bytes(), &publications); err != nil {
		t.Fatalf("unmarshal publications: %v", err)
	}
	for _, publication := range publications {
		if publication.TargetIQN != "iqn.2026-04.ai.holo:test-handler-drive" {
			continue
		}
		if !publication.ConnectedHosts.Available || publication.ConnectedHosts.HostCount != 0 || publication.ConnectedHosts.SessionCount != 0 || len(publication.ConnectedHosts.Initiators) != 0 {
			t.Fatalf("expected no active sessions summary, got %+v", publication.ConnectedHosts)
		}
		return
	}
	t.Fatalf("expected published drive target in response, got %+v", publications)
}

func TestTargetPublicationRejectsNilBody(t *testing.T) {
	srv := newTestServer(t)

	req := newAuthedRequest(http.MethodPost, "/v1/targets/publications", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for nil body, got %d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "invalid request body") {
		t.Fatalf("expected safe invalid request body response, got %s", resp.Body.String())
	}
}

func TestTargetPublicationRejectsMalformedIQNAndProfile(t *testing.T) {
	srv := newTestServer(t)
	req := newAuthedRequest(http.MethodPost, "/v1/targets/publications", bytes.NewBufferString(`{"libraryId":"lib-1","driveId":"drive-1","cartridgeId":"car-1","targetIqn":"iqn.2026-04.ai.holo:../../bad","deviceProfile":"bad\nprofile","actor":"tester"}`))
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid target publication to return 400, got %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestTargetValidationRunsUsesSafeNotFoundMessage(t *testing.T) {
	srv := newTestServer(t)

	req := newAuthedRequest(http.MethodGet, "/v1/targets/publications/missing/validation-runs", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing publication validation-runs, got %d body=%s", resp.Code, resp.Body.String())
	}
	if strings.Contains(strings.ToLower(resp.Body.String()), "err") {
		t.Fatalf("response should not leak internal error text, got %s", resp.Body.String())
	}
}

func TestTargetPublicationDeleteActionEndpoint(t *testing.T) {
	srv := newTestServer(t)

	chainReq := newAuthedRequest(http.MethodPost, "/v1/resources/chain", bytes.NewBufferString(`{"poolId":"pool-del-action","poolName":"pool-del-action","capacityBytes":1073741824,"libraryId":"lib-del-action","libraryName":"lib-del-action","driveId":"drive-del-action","driveSlot":1,"cartridgeId":"car-del-action","barcode":"B101"}`))
	chainResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(chainResp, chainReq)
	if chainResp.Code != http.StatusCreated {
		t.Fatalf("expected chain create 201, got %d body=%s", chainResp.Code, chainResp.Body.String())
	}

	pubReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications", bytes.NewBufferString(`{"libraryId":"lib-del-action","driveId":"drive-del-action","cartridgeId":"car-del-action","targetIqn":"iqn.2026-04.ai.holo:test-delete-action","actor":"tester"}`))
	pubResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(pubResp, pubReq)
	if pubResp.Code != http.StatusAccepted {
		t.Fatalf("expected publish accepted, got %d body=%s", pubResp.Code, pubResp.Body.String())
	}
	var publication map[string]any
	if err := json.Unmarshal(pubResp.Body.Bytes(), &publication); err != nil {
		t.Fatalf("unmarshal publication failed: %v body=%s", err, pubResp.Body.String())
	}
	publicationID, _ := publication["publicationId"].(string)
	if publicationID == "" {
		t.Fatalf("publicationId missing in response: %s", pubResp.Body.String())
	}

	deleteReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications/"+publicationID+"/delete?actor=tester", nil)
	deleteResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusAccepted {
		t.Fatalf("expected post unpublish action 202, got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}
}

func TestTargetPublicationListReturnsLatestPerIQNByDefault(t *testing.T) {
	srv := newTestServer(t)

	chainReq := newAuthedRequest(http.MethodPost, "/v1/resources/chain", bytes.NewBufferString(`{"poolId":"pool-dedupe","poolName":"pool-dedupe","capacityBytes":1073741824,"libraryId":"lib-dedupe","libraryName":"lib-dedupe","driveId":"drive-dedupe","driveSlot":1,"cartridgeId":"car-dedupe","barcode":"B201"}`))
	chainResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(chainResp, chainReq)
	if chainResp.Code != http.StatusCreated {
		t.Fatalf("expected chain create 201, got %d body=%s", chainResp.Code, chainResp.Body.String())
	}

	publish := func() string {
		req := newAuthedRequest(http.MethodPost, "/v1/targets/publications", bytes.NewBufferString(`{"libraryId":"lib-dedupe","driveId":"drive-dedupe","cartridgeId":"car-dedupe","targetIqn":"iqn.2026-04.ai.holo:test-dedupe","actor":"tester"}`))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != http.StatusAccepted {
			t.Fatalf("expected publish accepted, got %d body=%s", resp.Code, resp.Body.String())
		}
		var publication map[string]any
		if err := json.Unmarshal(resp.Body.Bytes(), &publication); err != nil {
			t.Fatalf("unmarshal publication failed: %v body=%s", err, resp.Body.String())
		}
		publicationID, _ := publication["publicationId"].(string)
		if publicationID == "" {
			t.Fatalf("publicationId missing in response: %s", resp.Body.String())
		}
		return publicationID
	}

	firstPublicationID := publish()

	deleteReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications/"+firstPublicationID+"/delete?actor=tester", nil)
	deleteResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusAccepted {
		t.Fatalf("expected post unpublish action 202, got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}

	_ = publish()

	listReq := newAuthedRequest(http.MethodGet, "/v1/targets/publications", nil)
	listResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("expected list 200, got %d body=%s", listResp.Code, listResp.Body.String())
	}
	var listPayload []map[string]any
	if err := json.Unmarshal(listResp.Body.Bytes(), &listPayload); err != nil {
		t.Fatalf("unmarshal deduped list failed: %v body=%s", err, listResp.Body.String())
	}
	matches := 0
	for _, row := range listPayload {
		if iqn, _ := row["targetIqn"].(string); iqn == "iqn.2026-04.ai.holo:test-dedupe" {
			matches++
			if state, _ := row["state"].(string); state != "ready" {
				t.Fatalf("expected latest ready publication for deduped iqn, got %s body=%s", state, listResp.Body.String())
			}
		}
	}
	if matches != 1 {
		t.Fatalf("expected deduped iqn to appear once, got %d body=%s", matches, listResp.Body.String())
	}

	historyReq := newAuthedRequest(http.MethodGet, "/v1/targets/publications?history=all", nil)
	historyResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(historyResp, historyReq)
	if historyResp.Code != http.StatusOK {
		t.Fatalf("expected history list 200, got %d body=%s", historyResp.Code, historyResp.Body.String())
	}
	var historyPayload []map[string]any
	if err := json.Unmarshal(historyResp.Body.Bytes(), &historyPayload); err != nil {
		t.Fatalf("unmarshal history list failed: %v body=%s", err, historyResp.Body.String())
	}
	if len(historyPayload) < 2 {
		t.Fatalf("expected full history payload, got %d body=%s", len(historyPayload), historyResp.Body.String())
	}
	for _, row := range historyPayload {
		if _, ok := row["connectedHosts"]; ok {
			t.Fatalf("expected history list to skip session discovery, got connectedHosts in body=%s", historyResp.Body.String())
		}
	}
}

func TestTargetLocalMountEndpointsPersistToggle(t *testing.T) {
	srv := newTestServer(t)

	getReq := newAuthedRequest(http.MethodGet, "/v1/targets/local-mount", nil)
	getResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getResp, getReq)
	if getResp.Code != http.StatusOK {
		t.Fatalf("expected local mount status 200, got %d body=%s", getResp.Code, getResp.Body.String())
	}
	var initial map[string]any
	if err := json.Unmarshal(getResp.Body.Bytes(), &initial); err != nil {
		t.Fatalf("unmarshal initial status: %v", err)
	}
	if enabled, _ := initial["enabled"].(bool); enabled {
		t.Fatalf("expected local mount disabled by default: %s", getResp.Body.String())
	}

	postReq := newAuthedRequest(http.MethodPost, "/v1/targets/local-mount", bytes.NewBufferString(`{"enabled":true,"actor":"tester"}`))
	postResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(postResp, postReq)
	if postResp.Code != http.StatusOK {
		t.Fatalf("expected local mount enable 200, got %d body=%s", postResp.Code, postResp.Body.String())
	}
	var enabledPayload map[string]any
	if err := json.Unmarshal(postResp.Body.Bytes(), &enabledPayload); err != nil {
		t.Fatalf("unmarshal enabled status: %v", err)
	}
	if enabled, _ := enabledPayload["enabled"].(bool); !enabled {
		t.Fatalf("expected local mount enabled: %s", postResp.Body.String())
	}
}
