package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidationRunModesEndpoint(t *testing.T) {
	srv := newTestServer(t)

	chainReq := newAuthedRequest(http.MethodPost, "/v1/resources/chain", bytes.NewBufferString(`{"poolId":"pool-1","poolName":"pool-1","capacityBytes":1073741824,"libraryId":"lib-1","libraryName":"lib-1","driveId":"drive-1","driveSlot":1,"cartridgeId":"car-1","barcode":"B001"}`))
	chainResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(chainResp, chainReq)
	if chainResp.Code != http.StatusCreated {
		t.Fatalf("expected chain create 201, got %d", chainResp.Code)
	}

	pubReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications", bytes.NewBufferString(`{"poolId":"pool-1","libraryId":"lib-1","driveId":"drive-1","cartridgeId":"car-1","targetIqn":"iqn.2026-04.ai.holo:validation-mode-test","actor":"tester"}`))
	pubResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(pubResp, pubReq)
	if pubResp.Code != http.StatusAccepted {
		t.Fatalf("expected publish accepted, got %d", pubResp.Code)
	}

	var publication map[string]any
	if err := json.Unmarshal(pubResp.Body.Bytes(), &publication); err != nil {
		t.Fatalf("decode publication response failed: %v", err)
	}
	publicationID, _ := publication["publicationId"].(string)
	if publicationID == "" {
		t.Fatal("publication id missing")
	}

	fixedReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications/"+publicationID+"/validation-runs?actor=tester", bytes.NewBufferString(`{"mode":"fixed","bytes":1024,"pattern":"AB"}`))
	fixedResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(fixedResp, fixedReq)
	if fixedResp.Code != http.StatusAccepted {
		t.Fatalf("expected fixed validation accepted, got %d body=%s", fixedResp.Code, fixedResp.Body.String())
	}
	if !strings.Contains(fixedResp.Body.String(), `"mode":"fixed"`) {
		t.Fatalf("expected fixed validation response to expose mode, got %s", fixedResp.Body.String())
	}
	if !strings.Contains(fixedResp.Body.String(), `"writeDigest":"sha256:`) {
		t.Fatalf("expected fixed validation response to expose writeDigest, got %s", fixedResp.Body.String())
	}

	emptyReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications/"+publicationID+"/validation-runs?actor=tester", bytes.NewBufferString(`{"mode":"empty"}`))
	emptyResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(emptyResp, emptyReq)
	if emptyResp.Code != http.StatusAccepted {
		t.Fatalf("expected empty validation accepted, got %d body=%s", emptyResp.Code, emptyResp.Body.String())
	}
	if !strings.Contains(emptyResp.Body.String(), `"mode":"empty"`) {
		t.Fatalf("expected empty validation response to expose mode, got %s", emptyResp.Body.String())
	}
	if !strings.Contains(emptyResp.Body.String(), `"bytesWritten":0`) {
		t.Fatalf("expected empty validation 0-byte write, got %s", emptyResp.Body.String())
	}

	invalidReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications/"+publicationID+"/validation-runs?actor=tester", bytes.NewBufferString(`{"mode":"bad-mode"}`))
	invalidResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(invalidResp, invalidReq)
	if invalidResp.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid mode 400, got %d", invalidResp.Code)
	}

	listReq := newAuthedRequest(http.MethodGet, "/v1/targets/publications/"+publicationID+"/validation-runs", nil)
	listResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("expected validation list 200, got %d", listResp.Code)
	}
	if !strings.Contains(listResp.Body.String(), `"mode":"fixed"`) || !strings.Contains(listResp.Body.String(), `"mode":"empty"`) {
		t.Fatalf("expected validation list to contain both modes, got %s", listResp.Body.String())
	}
}

func TestValidationRunRejectsNonReadyPublication(t *testing.T) {
	srv := newTestServer(t)

	chainReq := newAuthedRequest(http.MethodPost, "/v1/resources/chain", bytes.NewBufferString(`{"poolId":"pool-1","poolName":"pool-1","capacityBytes":1073741824,"libraryId":"lib-1","libraryName":"lib-1","driveId":"drive-1","driveSlot":1,"cartridgeId":"car-1","barcode":"B001"}`))
	chainResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(chainResp, chainReq)
	if chainResp.Code != http.StatusCreated {
		t.Fatalf("expected chain create 201, got %d", chainResp.Code)
	}

	pubReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications", bytes.NewBufferString(`{"poolId":"pool-1","libraryId":"lib-1","driveId":"drive-1","cartridgeId":"car-1","targetIqn":"iqn.2026-04.ai.holo:validation-non-ready","actor":"tester"}`))
	pubResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(pubResp, pubReq)
	if pubResp.Code != http.StatusAccepted {
		t.Fatalf("expected publish accepted, got %d", pubResp.Code)
	}

	var publication map[string]any
	if err := json.Unmarshal(pubResp.Body.Bytes(), &publication); err != nil {
		t.Fatalf("decode publication response failed: %v", err)
	}
	publicationID, _ := publication["publicationId"].(string)
	if publicationID == "" {
		t.Fatal("publication id missing")
	}

	delReq := newAuthedRequest(http.MethodDelete, "/v1/targets/publications/"+publicationID+"?actor=tester", nil)
	delResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(delResp, delReq)
	if delResp.Code != http.StatusAccepted {
		t.Fatalf("expected unpublish accepted, got %d", delResp.Code)
	}

	validateReq := newAuthedRequest(http.MethodPost, "/v1/targets/publications/"+publicationID+"/validation-runs?actor=tester", bytes.NewBufferString(`{"mode":"fixed"}`))
	validateResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(validateResp, validateReq)
	if validateResp.Code != http.StatusBadRequest {
		t.Fatalf("expected non-ready validation to fail 400, got %d", validateResp.Code)
	}
}
