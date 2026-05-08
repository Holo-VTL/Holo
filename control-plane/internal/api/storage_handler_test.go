package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type staticRunner struct {
	payload string
}

func (r *staticRunner) Run(_ context.Context, command string, args ...string) (string, error) {
	if command == "sudo" && len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	if command != "lsblk" {
		return "", nil
	}
	if len(args) >= 2 && args[0] == "-no" && args[1] == "FSTYPE" {
		return "", nil
	}
	return r.payload, nil
}

const storageTestLsblk = `{
  "blockdevices": [
    {
      "name": "sdb",
      "path": "/dev/sdb",
      "type": "disk",
      "size": "1099511627776",
      "mountpoint": null,
      "fstype": "",
      "model": "DATA",
      "serial": "DATA001",
      "vendor": "ATA"
    }
  ]
}`

func newStorageHandlerForTest() *StorageHandler {
	repo := memory.NewStoragePoolRepo()
	svc := orchestration.NewStorageManagementService(repo, audit.NewMemoryWriter(), &staticRunner{payload: storageTestLsblk})
	return NewStorageHandler(svc)
}

func TestStorageHandler_PoolCRUDAndCapacity(t *testing.T) {
	h := newStorageHandlerForTest()

	createReq := httptest.NewRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-1","name":"Pool 1","warningThresholdPct":90}`))
	createResp := httptest.NewRecorder()
	h.handlePools(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected create pool 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/storage/pools", nil)
	listResp := httptest.NewRecorder()
	h.handlePools(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("expected list pools 200, got %d", listResp.Code)
	}

	var listed []map[string]any
	if err := json.Unmarshal(listResp.Body.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal list response failed: %v body=%s", err, listResp.Body.String())
	}
	if len(listed) != 1 {
		t.Fatalf("expected one pool, got %d", len(listed))
	}

	attachReq := httptest.NewRequest(http.MethodPost, "/v1/storage/pools/pool-1/disks/attach", bytes.NewBufferString(`{"devicePath":"/dev/sdb"}`))
	attachResp := httptest.NewRecorder()
	h.handlePoolSubresource(attachResp, attachReq)
	if attachResp.Code != http.StatusOK {
		t.Fatalf("expected attach 200, got %d body=%s", attachResp.Code, attachResp.Body.String())
	}

	capacityReq := httptest.NewRequest(http.MethodGet, "/v1/storage/pools/pool-1/capacity", nil)
	capacityResp := httptest.NewRecorder()
	h.handlePoolSubresource(capacityResp, capacityReq)
	if capacityResp.Code != http.StatusOK {
		t.Fatalf("expected capacity 200, got %d body=%s", capacityResp.Code, capacityResp.Body.String())
	}
	if !bytes.Contains(capacityResp.Body.Bytes(), []byte("totalBytes")) {
		t.Fatalf("expected capacity payload, got %s", capacityResp.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/v1/storage/pools/pool-1", nil)
	deleteResp := httptest.NewRecorder()
	h.handlePoolSubresource(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected delete with attached disk to detach and succeed (204), got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}
}

func TestStorageHandler_DeleteActionEndpoint(t *testing.T) {
	h := newStorageHandlerForTest()

	createReq := httptest.NewRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-compat","name":"Pool Compat","warningThresholdPct":90}`))
	createResp := httptest.NewRecorder()
	h.handlePools(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected create pool 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodPost, "/v1/storage/pools/pool-compat/delete", nil)
	deleteResp := httptest.NewRecorder()
	h.handlePoolSubresource(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected post delete action 204, got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}
}

func TestStorageHandler_DiscoveryEndpoint(t *testing.T) {
	h := newStorageHandlerForTest()
	req := httptest.NewRequest(http.MethodGet, "/v1/storage/disks/discovery", nil)
	resp := httptest.NewRecorder()
	h.handleDisksDiscovery(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected discover 200, got %d body=%s", resp.Code, resp.Body.String())
	}
	if !bytes.Contains(resp.Body.Bytes(), []byte("/dev/sdb")) {
		t.Fatalf("expected /dev/sdb in response, got %s", resp.Body.String())
	}
}

func TestStorageHandler_InvalidBody(t *testing.T) {
	h := newStorageHandlerForTest()
	req := httptest.NewRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"","name":""}`))
	resp := httptest.NewRecorder()
	h.handlePools(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 invalid request, got %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestStorageHandlerRejectsUnsafeDevicePath(t *testing.T) {
	h := newStorageHandlerForTest()
	createReq := httptest.NewRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-devpath","name":"Pool DevPath","warningThresholdPct":90}`))
	createResp := httptest.NewRecorder()
	h.handlePools(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected create pool 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	attachReq := httptest.NewRequest(http.MethodPost, "/v1/storage/pools/pool-devpath/disks/attach", bytes.NewBufferString(`{"devicePath":"/tmp/not-a-disk"}`))
	attachResp := httptest.NewRecorder()
	h.handlePoolSubresource(attachResp, attachReq)
	if attachResp.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid device path 400, got %d body=%s", attachResp.Code, attachResp.Body.String())
	}
}

func TestStorageHandler_DuplicatePoolReturnsConflict(t *testing.T) {
	h := newStorageHandlerForTest()
	createBody := `{"poolId":"pool-dup","name":"Pool Dup","warningThresholdPct":90}`

	firstReq := httptest.NewRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(createBody))
	firstResp := httptest.NewRecorder()
	h.handlePools(firstResp, firstReq)
	if firstResp.Code != http.StatusCreated {
		t.Fatalf("expected first create 201, got %d body=%s", firstResp.Code, firstResp.Body.String())
	}

	secondReq := httptest.NewRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(createBody))
	secondResp := httptest.NewRecorder()
	h.handlePools(secondResp, secondReq)
	if secondResp.Code != http.StatusConflict {
		t.Fatalf("expected duplicate create 409, got %d body=%s", secondResp.Code, secondResp.Body.String())
	}
}

func TestStorageHandler_NotFoundAndDetachCoverage(t *testing.T) {
	h := newStorageHandlerForTest()

	getMissingReq := httptest.NewRequest(http.MethodGet, "/v1/storage/pools/missing", nil)
	getMissingResp := httptest.NewRecorder()
	h.handlePoolSubresource(getMissingResp, getMissingReq)
	if getMissingResp.Code != http.StatusNotFound {
		t.Fatalf("expected missing pool 404, got %d body=%s", getMissingResp.Code, getMissingResp.Body.String())
	}

	createReq := httptest.NewRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-detach","name":"Pool Detach","warningThresholdPct":90}`))
	createResp := httptest.NewRecorder()
	h.handlePools(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected create pool 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	attachReq := httptest.NewRequest(http.MethodPost, "/v1/storage/pools/pool-detach/disks/attach", bytes.NewBufferString(`{"devicePath":"/dev/sdb"}`))
	attachResp := httptest.NewRecorder()
	h.handlePoolSubresource(attachResp, attachReq)
	if attachResp.Code != http.StatusOK {
		t.Fatalf("expected attach 200, got %d body=%s", attachResp.Code, attachResp.Body.String())
	}

	detachReq := httptest.NewRequest(http.MethodPost, "/v1/storage/pools/pool-detach/disks/detach", bytes.NewBufferString(`{"devicePath":"/dev/sdb"}`))
	detachResp := httptest.NewRecorder()
	h.handlePoolSubresource(detachResp, detachReq)
	if detachResp.Code != http.StatusOK {
		t.Fatalf("expected detach 200, got %d body=%s", detachResp.Code, detachResp.Body.String())
	}
}

func TestStorageHandler_RespondStorageErrorMaps507(t *testing.T) {
	resp := httptest.NewRecorder()
	respondStorageError(resp, domain.ErrCapacityExceeded)
	if resp.Code != http.StatusInsufficientStorage {
		t.Fatalf("expected 507 for capacity exceeded, got %d body=%s", resp.Code, resp.Body.String())
	}
}
