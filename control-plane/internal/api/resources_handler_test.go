package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

type failingJSON struct{}

func (f failingJSON) MarshalJSON() ([]byte, error) {
	return nil, errors.New("encode failed")
}

func TestResourcesCompensationFailuresAreLogged(t *testing.T) {
	var logs bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(originalOutput) })

	handler := &ResourcesHandler{}
	handler.logCompensationError(context.Background(), "rollback drive", errors.New("delete failed"), "driveId", "drive-a")

	got := logs.String()
	if !strings.Contains(got, "compensation cleanup failed") {
		t.Fatalf("expected compensation failure log, got %q", got)
	}
	if !strings.Contains(got, "rollback drive") || !strings.Contains(got, "drive-a") {
		t.Fatalf("expected operation context in compensation log, got %q", got)
	}
}

func TestStoragePoolCapacityReconcilesFromCartridgeMetadata(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	createPoolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-cap","name":"Pool Capacity"}`))
	createPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createPoolResp, createPoolReq)
	if createPoolResp.Code != http.StatusCreated {
		t.Fatalf("expected create pool 201, got %d body=%s", createPoolResp.Code, createPoolResp.Body.String())
	}
	if _, err := srv.metadataDB.Exec(
		`INSERT INTO storage_pool_disks(device_path, pool_id, size_bytes, attached_at) VALUES (?, ?, ?, ?)`,
		"/dev/sdb",
		"pool-cap",
		int64(10*1024*1024),
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("seed storage pool disk: %v", err)
	}

	createLibReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-cap","name":"Library Capacity"}`))
	createLibResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createLibResp, createLibReq)
	if createLibResp.Code != http.StatusCreated {
		t.Fatalf("expected create library 201, got %d body=%s", createLibResp.Code, createLibResp.Body.String())
	}
	createCartReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-cap","libraryId":"lib-cap","cartridgeId":"VTA555L06","barcode":"VTA555L06","capacityBytes":5242880}`))
	createCartResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createCartResp, createCartReq)
	if createCartResp.Code != http.StatusCreated {
		t.Fatalf("expected create cartridge 201, got %d body=%s", createCartResp.Code, createCartResp.Body.String())
	}
	if err := writeAtomicText(cartridgeMetadataPath("VTA555L06"), "cartridge_id=VTA555L06\ncapacity_bytes=5242880\nused_bytes=1048576\n"); err != nil {
		t.Fatalf("write shared cartridge metadata: %v", err)
	}

	getPoolReq := newAuthedRequest(http.MethodGet, "/v1/storage/pools/pool-cap", nil)
	getPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getPoolResp, getPoolReq)
	if getPoolResp.Code != http.StatusOK {
		t.Fatalf("expected get pool 200, got %d body=%s", getPoolResp.Code, getPoolResp.Body.String())
	}
	var pool domain.StoragePoolRuntime
	if err := json.Unmarshal(getPoolResp.Body.Bytes(), &pool); err != nil {
		t.Fatalf("decode pool: %v", err)
	}
	if pool.Capacity.UsedBytes != 1048576 || pool.Capacity.FreeBytes != 9437184 || pool.Capacity.UsedPercent != 10 {
		t.Fatalf("expected pool capacity to reconcile from cartridge usage, got %+v", pool.Capacity)
	}
}

func TestResourcesInvalidIDUsesSafePublicError(t *testing.T) {
	handler := &ResourcesHandler{}
	req := httptest.NewRequest(http.MethodGet, "/v1/libraries/%20", nil)
	resp := httptest.NewRecorder()

	handler.handleLibraryByID(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.Code)
	}
	body := resp.Body.String()
	if !strings.Contains(body, "invalid request") {
		t.Fatalf("expected safe public error, got %q", body)
	}
	if strings.Contains(body, domain.ErrInvalidInput.Error()) {
		t.Fatalf("response leaked domain error string: %q", body)
	}
}

func TestCoreResourceCreateAndQueryEndpoints(t *testing.T) {
	srv := newTestServer(t)

	poolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-a","name":"Primary Pool","warningThresholdPct":90}`))
	poolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(poolResp, poolReq)
	if poolResp.Code != http.StatusCreated {
		t.Fatalf("expected pool create 201, got %d, body=%s", poolResp.Code, poolResp.Body.String())
	}

	listPoolsReq := newAuthedRequest(http.MethodGet, "/v1/storage/pools", nil)
	listPoolsResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(listPoolsResp, listPoolsReq)
	if listPoolsResp.Code != http.StatusOK {
		t.Fatalf("expected pools list 200, got %d", listPoolsResp.Code)
	}
	if !strings.Contains(listPoolsResp.Body.String(), "pool-a") {
		t.Fatalf("expected pools list to contain pool-a, got %s", listPoolsResp.Body.String())
	}

	getPoolReq := newAuthedRequest(http.MethodGet, "/v1/storage/pools/pool-a", nil)
	getPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getPoolResp, getPoolReq)
	if getPoolResp.Code != http.StatusOK {
		t.Fatalf("expected get pool 200, got %d", getPoolResp.Code)
	}

	libraryReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-a","name":"Library A"}`))
	libraryResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(libraryResp, libraryReq)
	if libraryResp.Code != http.StatusCreated {
		t.Fatalf("expected library create 201, got %d, body=%s", libraryResp.Code, libraryResp.Body.String())
	}

	listLibReq := newAuthedRequest(http.MethodGet, "/v1/libraries", nil)
	listLibResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(listLibResp, listLibReq)
	if listLibResp.Code != http.StatusOK {
		t.Fatalf("expected library list 200, got %d", listLibResp.Code)
	}
	if !strings.Contains(listLibResp.Body.String(), "lib-a") {
		t.Fatalf("expected libraries list to contain lib-a, got %s", listLibResp.Body.String())
	}

	driveReq := newAuthedRequest(http.MethodPost, "/v1/drives", bytes.NewBufferString(`{"driveId":"drive-a","libraryId":"lib-a","slot":1}`))
	driveResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(driveResp, driveReq)
	if driveResp.Code != http.StatusCreated {
		t.Fatalf("expected drive create 201, got %d, body=%s", driveResp.Code, driveResp.Body.String())
	}

	listDriveReq := newAuthedRequest(http.MethodGet, "/v1/drives", nil)
	listDriveResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(listDriveResp, listDriveReq)
	if listDriveResp.Code != http.StatusOK {
		t.Fatalf("expected drive list 200, got %d", listDriveResp.Code)
	}
	if !strings.Contains(listDriveResp.Body.String(), "drive-a") {
		t.Fatalf("expected drives list to contain drive-a, got %s", listDriveResp.Body.String())
	}

	cartridgeReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-a","libraryId":"lib-a","capacityBytes":1073741824,"ltoGeneration":6}`))
	cartridgeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(cartridgeResp, cartridgeReq)
	if cartridgeResp.Code != http.StatusCreated {
		t.Fatalf("expected cartridge create 201, got %d, body=%s", cartridgeResp.Code, cartridgeResp.Body.String())
	}
	if !strings.Contains(cartridgeResp.Body.String(), "VTA000L06") {
		t.Fatalf("expected generated cartridge label VTA000L06, got %s", cartridgeResp.Body.String())
	}
	metadataRaw, err := os.ReadFile(cartridgeMetadataPath("VTA000L06"))
	if err != nil {
		t.Fatalf("expected cartridge metadata to be written: %v", err)
	}
	if !strings.Contains(string(metadataRaw), "capacity_bytes=1073741824") {
		t.Fatalf("expected cartridge metadata capacity, got %s", string(metadataRaw))
	}

	listCartridgeReq := newAuthedRequest(http.MethodGet, "/v1/cartridges", nil)
	listCartridgeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(listCartridgeResp, listCartridgeReq)
	if listCartridgeResp.Code != http.StatusOK {
		t.Fatalf("expected cartridge list 200, got %d", listCartridgeResp.Code)
	}
	if !strings.Contains(listCartridgeResp.Body.String(), "VTA000L06") {
		t.Fatalf("expected cartridges list to contain VTA000L06, got %s", listCartridgeResp.Body.String())
	}

	getCartridgeReq := newAuthedRequest(http.MethodGet, "/v1/cartridges/VTA000L06", nil)
	getCartridgeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getCartridgeResp, getCartridgeReq)
	if getCartridgeResp.Code != http.StatusOK {
		t.Fatalf("expected get cartridge 200, got %d", getCartridgeResp.Code)
	}
}

func TestCoreResourceValidationAndConflicts(t *testing.T) {
	srv := newTestServer(t)

	invalidPoolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"","name":"","warningThresholdPct":90}`))
	invalidPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(invalidPoolResp, invalidPoolReq)
	if invalidPoolResp.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid pool request 400, got %d", invalidPoolResp.Code)
	}

	libraryReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-x","name":"Library X"}`))
	libraryResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(libraryResp, libraryReq)
	if libraryResp.Code != http.StatusCreated {
		t.Fatalf("expected library create 201, got %d", libraryResp.Code)
	}

	poolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-b","name":"Pool B","warningThresholdPct":90}`))
	poolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(poolResp, poolReq)
	if poolResp.Code != http.StatusCreated {
		t.Fatalf("expected first pool create 201, got %d", poolResp.Code)
	}

	dupPoolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-b","name":"Pool B2","warningThresholdPct":90}`))
	dupPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(dupPoolResp, dupPoolReq)
	if dupPoolResp.Code != http.StatusConflict {
		t.Fatalf("expected duplicate pool conflict 409, got %d", dupPoolResp.Code)
	}
}

func TestLibraryAndDriveCreateRejectsMoreThanFourDrives(t *testing.T) {
	srv := newTestServer(t)

	tooManyLibraryReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-too-many","name":"Too Many Drives","driveCount":5}`))
	tooManyLibraryResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(tooManyLibraryResp, tooManyLibraryReq)
	if tooManyLibraryResp.Code != http.StatusBadRequest {
		t.Fatalf("expected library driveCount over max to return 400, got %d body=%s", tooManyLibraryResp.Code, tooManyLibraryResp.Body.String())
	}

	tooManySlotsReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-too-many-slots","name":"Too Many Slots","slotCount":10001}`))
	tooManySlotsResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(tooManySlotsResp, tooManySlotsReq)
	if tooManySlotsResp.Code != http.StatusBadRequest {
		t.Fatalf("expected library slotCount over max to return 400, got %d body=%s", tooManySlotsResp.Code, tooManySlotsResp.Body.String())
	}

	libraryReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-drive-limit","name":"Drive Limit","driveCount":4}`))
	libraryResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(libraryResp, libraryReq)
	if libraryResp.Code != http.StatusCreated {
		t.Fatalf("expected library create 201, got %d body=%s", libraryResp.Code, libraryResp.Body.String())
	}
	var library domain.VirtualLibrary
	if err := json.Unmarshal(libraryResp.Body.Bytes(), &library); err != nil {
		t.Fatalf("decode library response: %v", err)
	}
	if library.DriveCount != 4 {
		t.Fatalf("expected stored drive count 4, got %d", library.DriveCount)
	}

	for i := 1; i <= 4; i++ {
		body := `{"driveId":"drive-limit-` + strconv.Itoa(i) + `","libraryId":"lib-drive-limit","slot":` + strconv.Itoa(i) + `}`
		driveReq := newAuthedRequest(http.MethodPost, "/v1/drives", bytes.NewBufferString(body))
		driveResp := httptest.NewRecorder()
		srv.Router().ServeHTTP(driveResp, driveReq)
		if driveResp.Code != http.StatusCreated {
			t.Fatalf("create drive %d expected 201, got %d body=%s", i, driveResp.Code, driveResp.Body.String())
		}
	}

	fifthDriveReq := newAuthedRequest(http.MethodPost, "/v1/drives", bytes.NewBufferString(`{"driveId":"drive-limit-5","libraryId":"lib-drive-limit","slot":5}`))
	fifthDriveResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(fifthDriveResp, fifthDriveReq)
	if fifthDriveResp.Code != http.StatusBadRequest {
		t.Fatalf("expected fifth drive create to return 400, got %d body=%s", fifthDriveResp.Code, fifthDriveResp.Body.String())
	}

	listDriveReq := newAuthedRequest(http.MethodGet, "/v1/drives", nil)
	listDriveResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(listDriveResp, listDriveReq)
	if listDriveResp.Code != http.StatusOK {
		t.Fatalf("expected drive list 200, got %d body=%s", listDriveResp.Code, listDriveResp.Body.String())
	}
	var drives []domain.VirtualDrive
	if err := json.Unmarshal(listDriveResp.Body.Bytes(), &drives); err != nil {
		t.Fatalf("decode drive list response: %v", err)
	}
	if len(drives) != 4 {
		t.Fatalf("expected exactly 4 drives after rejected fifth create, got %d", len(drives))
	}
}

func TestCoreResourcesPersistAcrossServerRestart(t *testing.T) {
	metadataDSN := filepath.Join(t.TempDir(), "metadata.db")
	srv := newTestServerWithMetadata(t, metadataDSN)
	reqs := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-persist","name":"Pool Persist","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-persist","name":"Library Persist","driveCount":4,"slotCount":20}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-persist-1","libraryId":"lib-persist","slot":1}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-persist","cartridgeId":"VTA900L06","libraryId":"lib-persist","barcode":"VTA900L06","capacityBytes":1073741824}`, http.StatusCreated},
	}
	for _, tc := range reqs {
		req := newAuthedRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != tc.code {
			t.Fatalf("%s %s expected %d got %d body=%s", tc.method, tc.path, tc.code, resp.Code, resp.Body.String())
		}
	}

	restarted := newTestServerWithMetadata(t, metadataDSN)
	listPoolsReq := newAuthedRequest(http.MethodGet, "/v1/storage/pools", nil)
	listPoolsResp := httptest.NewRecorder()
	restarted.Router().ServeHTTP(listPoolsResp, listPoolsReq)
	if listPoolsResp.Code != http.StatusOK {
		t.Fatalf("expected pool list 200 after restart, got %d body=%s", listPoolsResp.Code, listPoolsResp.Body.String())
	}
	var pools []domain.StoragePoolRuntime
	if err := json.Unmarshal(listPoolsResp.Body.Bytes(), &pools); err != nil {
		t.Fatalf("decode pools after restart: %v", err)
	}
	if len(pools) != 1 || pools[0].PoolID != "pool-persist" {
		t.Fatalf("unexpected pools after restart: %+v", pools)
	}

	getLibReq := newAuthedRequest(http.MethodGet, "/v1/libraries/lib-persist", nil)
	getLibResp := httptest.NewRecorder()
	restarted.Router().ServeHTTP(getLibResp, getLibReq)
	if getLibResp.Code != http.StatusOK {
		t.Fatalf("expected library get 200 after restart, got %d body=%s", getLibResp.Code, getLibResp.Body.String())
	}
	var library domain.VirtualLibrary
	if err := json.Unmarshal(getLibResp.Body.Bytes(), &library); err != nil {
		t.Fatalf("decode library after restart: %v", err)
	}
	if library.LibraryID != "lib-persist" || library.DriveCount != 4 || library.SlotCount != 20 {
		t.Fatalf("unexpected library after restart: %+v", library)
	}

	getDriveReq := newAuthedRequest(http.MethodGet, "/v1/drives/drive-persist-1", nil)
	getDriveResp := httptest.NewRecorder()
	restarted.Router().ServeHTTP(getDriveResp, getDriveReq)
	if getDriveResp.Code != http.StatusOK {
		t.Fatalf("expected drive get 200 after restart, got %d body=%s", getDriveResp.Code, getDriveResp.Body.String())
	}
	var drive domain.VirtualDrive
	if err := json.Unmarshal(getDriveResp.Body.Bytes(), &drive); err != nil {
		t.Fatalf("decode drive after restart: %v", err)
	}
	if drive.DriveID != "drive-persist-1" || drive.LibraryID != "lib-persist" {
		t.Fatalf("unexpected drive after restart: %+v", drive)
	}

	getCartReq := newAuthedRequest(http.MethodGet, "/v1/cartridges/VTA900L06", nil)
	getCartResp := httptest.NewRecorder()
	restarted.Router().ServeHTTP(getCartResp, getCartReq)
	if getCartResp.Code != http.StatusOK {
		t.Fatalf("expected cartridge get 200 after restart, got %d body=%s", getCartResp.Code, getCartResp.Body.String())
	}
	var cartridge domain.VirtualCartridge
	if err := json.Unmarshal(getCartResp.Body.Bytes(), &cartridge); err != nil {
		t.Fatalf("decode cartridge after restart: %v", err)
	}
	if cartridge.CartridgeID != "VTA900L06" || cartridge.PoolID != "pool-persist" || cartridge.LibraryID != "lib-persist" {
		t.Fatalf("unexpected cartridge after restart: %+v", cartridge)
	}
}

func TestResourcesAutoPublishLibraryAndDriveIQN(t *testing.T) {
	srv := newTestServer(t)
	reqs := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-auto-pub","name":"Pool Auto Pub","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-auto-pub","name":"Auto Pub","vendor":"IBM","libraryType":"03584L32","driveType":"ULT3580-TD6"}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"lib-auto-pub-drv-01","libraryId":"lib-auto-pub","slot":256}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-auto-pub","libraryId":"lib-auto-pub","capacityBytes":1073741824,"ltoGeneration":6}`, http.StatusCreated},
	}
	for _, tc := range reqs {
		req := newAuthedRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != tc.code {
			t.Fatalf("%s %s expected %d got %d body=%s", tc.method, tc.path, tc.code, resp.Code, resp.Body.String())
		}
	}

	publicationsReq := newAuthedRequest(http.MethodGet, "/v1/targets/publications", nil)
	publicationsResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(publicationsResp, publicationsReq)
	if publicationsResp.Code != http.StatusOK {
		t.Fatalf("expected publications list 200, got %d body=%s", publicationsResp.Code, publicationsResp.Body.String())
	}
	body := publicationsResp.Body.String()
	if !strings.Contains(body, `iqn.2026-04.cloud.backupnext.holo:library-lib-auto-pub`) {
		t.Fatalf("expected library iqn publication, got %s", body)
	}
	if !strings.Contains(body, `iqn.2026-04.cloud.backupnext.holo:drive-lib-auto-pub-drv-01`) {
		t.Fatalf("expected drive iqn publication, got %s", body)
	}
	if !strings.Contains(body, `"deviceRole":"changer"`) || !strings.Contains(body, `"deviceRole":"drive"`) {
		t.Fatalf("expected changer and drive roles, got %s", body)
	}
}

func TestCartridgeCreateRequiresCanonicalIdentity(t *testing.T) {
	srv := newTestServer(t)
	reqs := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-naming","name":"Pool Naming","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-naming","name":"Library Naming"}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-naming","cartridgeId":"car-a","libraryId":"lib-naming","barcode":"B1001","capacityBytes":1073741824}`, http.StatusBadRequest},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-naming","libraryId":"lib-naming","capacityBytes":1073741824,"mediaType":"LTO6"}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-naming","libraryId":"lib-naming","capacityBytes":1073741824,"ltoGeneration":6}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-naming","cartridgeId":"QA1000L06","libraryId":"lib-naming","barcode":"QA1000L06","capacityBytes":1073741824,"ltoGeneration":6}`, http.StatusCreated},
	}
	for _, tc := range reqs {
		req := newAuthedRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != tc.code {
			t.Fatalf("%s %s expected %d got %d body=%s", tc.method, tc.path, tc.code, resp.Code, resp.Body.String())
		}
	}

	listReq := newAuthedRequest(http.MethodGet, "/v1/cartridges", nil)
	listResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("expected list 200, got %d", listResp.Code)
	}
	if !strings.Contains(listResp.Body.String(), "VTA000L06") || !strings.Contains(listResp.Body.String(), "VTA001L06") {
		t.Fatalf("expected sequential labels VTA000L06/VTA001L06, got %s", listResp.Body.String())
	}
	if !strings.Contains(listResp.Body.String(), "QA1000L06") {
		t.Fatalf("expected custom prefixed label QA1000L06, got %s", listResp.Body.String())
	}
}

func TestResourceChainRejectsDuplicateBarcode(t *testing.T) {
	srv := newTestServer(t)
	first := newAuthedRequest(http.MethodPost, "/v1/resources/chain", bytes.NewBufferString(`{"poolId":"pool-chain","poolName":"Pool Chain","capacityBytes":1073741824,"libraryId":"lib-chain","libraryName":"Library Chain","driveId":"drive-chain-1","driveSlot":1,"cartridgeId":"VTA100L06","barcode":"VTA100L06"}`))
	firstResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(firstResp, first)
	if firstResp.Code != http.StatusCreated {
		t.Fatalf("expected first chain create 201, got %d body=%s", firstResp.Code, firstResp.Body.String())
	}

	second := newAuthedRequest(http.MethodPost, "/v1/resources/chain", bytes.NewBufferString(`{"poolId":"pool-chain","poolName":"Pool Chain","capacityBytes":1073741824,"libraryId":"lib-chain","libraryName":"Library Chain","driveId":"drive-chain-2","driveSlot":2,"cartridgeId":"VTA101L06","barcode":"VTA100L06"}`))
	secondResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(secondResp, second)
	if secondResp.Code != http.StatusConflict {
		t.Fatalf("expected duplicate barcode conflict 409, got %d body=%s", secondResp.Code, secondResp.Body.String())
	}
}

func TestCartridgeDeleteAndSlotsSync(t *testing.T) {
	mediaStateDir := t.TempDir()
	storageRoot := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	t.Setenv("HOLO_STORAGE_ROOT", storageRoot)

	srv := newTestServer(t)
	reqs := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-sync","name":"Pool Sync","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-sync","name":"Library Sync"}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-sync","libraryId":"lib-sync","slot":1}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-sync","cartridgeId":"VTA000L06","libraryId":"lib-sync","barcode":"VTA000L06","capacityBytes":1073741824}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-sync","cartridgeId":"VTA001L06","libraryId":"lib-sync","barcode":"VTA001L06","capacityBytes":1073741824}`, http.StatusCreated},
	}
	for _, tc := range reqs {
		req := newAuthedRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != tc.code {
			t.Fatalf("%s %s expected %d got %d body=%s", tc.method, tc.path, tc.code, resp.Code, resp.Body.String())
		}
	}

	canonicalLayoutDir := filepath.Join(storageRoot, "cartridges", "lib-sync", "vta000l06")
	if err := os.MkdirAll(canonicalLayoutDir, 0o755); err != nil {
		t.Fatalf("create canonical layout dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(canonicalLayoutDir, "data.segment"), []byte("legacy"), 0o644); err != nil {
		t.Fatalf("seed canonical layout data: %v", err)
	}
	legacyLayoutDir := filepath.Join(storageRoot, "drive-sync", "vta000l06")
	if err := os.MkdirAll(legacyLayoutDir, 0o755); err != nil {
		t.Fatalf("create legacy layout dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyLayoutDir, "data.segment"), []byte("legacy"), 0o644); err != nil {
		t.Fatalf("seed legacy layout data: %v", err)
	}

	slotsPath := filepath.Join(mediaStateDir, "lib-sync__drive-sync.slots")
	raw, err := os.ReadFile(slotsPath)
	if err != nil {
		t.Fatalf("expected slots file to exist: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 || lines[0] != "VTA000L06" || lines[1] != "VTA001L06" {
		t.Fatalf("unexpected slots file content before delete: %q", string(raw))
	}

	deleteReq := newAuthedRequest(http.MethodDelete, "/v1/cartridges/VTA000L06", nil)
	deleteResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected cartridge delete 204, got %d, body=%s", deleteResp.Code, deleteResp.Body.String())
	}

	if _, err := os.Stat(canonicalLayoutDir); !os.IsNotExist(err) {
		t.Fatalf("expected canonical layout cleanup, err=%v", err)
	}
	if _, err := os.Stat(legacyLayoutDir); !os.IsNotExist(err) {
		t.Fatalf("expected legacy layout cleanup, err=%v", err)
	}

	getDeletedReq := newAuthedRequest(http.MethodGet, "/v1/cartridges/VTA000L06", nil)
	getDeletedResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getDeletedResp, getDeletedReq)
	if getDeletedResp.Code != http.StatusNotFound {
		t.Fatalf("expected deleted cartridge 404, got %d", getDeletedResp.Code)
	}

	raw, err = os.ReadFile(slotsPath)
	if err != nil {
		t.Fatalf("expected slots file after delete: %v", err)
	}
	lines = strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 || lines[0] != "-" || lines[1] != "VTA001L06" {
		t.Fatalf("unexpected slots file content after delete: %q", string(raw))
	}
}

func TestDriveCreateSyncsSlotsForExistingMedia(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)

	srv := newTestServer(t)
	reqs := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-drive-sync","name":"Pool Drive Sync","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-drive-sync","name":"Library Drive Sync"}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-drive-sync","cartridgeId":"VTA000L06","libraryId":"lib-drive-sync","barcode":"VTA000L06","capacityBytes":1073741824}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-a","libraryId":"lib-drive-sync","slot":1}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-b","libraryId":"lib-drive-sync","slot":2}`, http.StatusCreated},
	}
	for _, tc := range reqs {
		req := newAuthedRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != tc.code {
			t.Fatalf("%s %s expected %d got %d body=%s", tc.method, tc.path, tc.code, resp.Code, resp.Body.String())
		}
	}

	for _, driveID := range []string{"drive-a", "drive-b"} {
		slotsPath := filepath.Join(mediaStateDir, "lib-drive-sync__"+driveID+".slots")
		raw, err := os.ReadFile(slotsPath)
		if err != nil {
			t.Fatalf("expected slots file for %s: %v", driveID, err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) == 0 || lines[0] != "VTA000L06" {
			t.Fatalf("unexpected slots content for %s: %q", driveID, string(raw))
		}
	}
}

func TestLibrarySlotCountSyncWritesAllConfiguredSlots(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)

	srv := newTestServer(t)
	reqs := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-slot-sync","name":"Pool Slot Sync","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-slot-sync","name":"Library Slot Sync","slotCount":20,"slotStartAddress":1024}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-slot-sync","libraryId":"lib-slot-sync","slot":1}`, http.StatusCreated},
	}
	for _, tc := range reqs {
		req := newAuthedRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != tc.code {
			t.Fatalf("%s %s expected %d got %d body=%s", tc.method, tc.path, tc.code, resp.Code, resp.Body.String())
		}
	}

	for i := 0; i < 20; i++ {
		req := newAuthedRequest(
			http.MethodPost,
			"/v1/cartridges",
			bytes.NewBufferString(`{"poolId":"pool-slot-sync","libraryId":"lib-slot-sync","capacityBytes":1073741824,"ltoGeneration":6}`),
		)
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != http.StatusCreated {
			t.Fatalf("create cartridge %d expected 201 got %d body=%s", i, resp.Code, resp.Body.String())
		}
	}

	slotsPath := filepath.Join(mediaStateDir, "lib-slot-sync__drive-slot-sync.slots")
	raw, err := os.ReadFile(slotsPath)
	if err != nil {
		t.Fatalf("expected slots file to exist: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 20 {
		t.Fatalf("expected 20 configured slots, got %d in %q", len(lines), string(raw))
	}
	if lines[0] != "VTA000L06" || lines[19] != "VTA019L06" {
		t.Fatalf("unexpected slot labels: first=%q last=%q full=%q", lines[0], lines[19], string(raw))
	}
}

func TestAddLibrarySlotDoesNotCreateCartridge(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)

	srv := newTestServer(t)
	setupSlotFlowLibrary(t, srv, "lib-add-empty-slot", "drive-add-empty-slot", 2)
	createSlotFlowCartridge(t, srv, "lib-add-empty-slot", "VTA000L06", false)
	createSlotFlowCartridge(t, srv, "lib-add-empty-slot", "VTA001L06", false)

	addReq := newAuthedRequest(http.MethodPost, "/v1/libraries/lib-add-empty-slot/slots", bytes.NewBufferString(`{"count":1,"actor":"web-console"}`))
	addResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(addResp, addReq)
	if addResp.Code != http.StatusOK {
		t.Fatalf("expected add slot 200, got %d body=%s", addResp.Code, addResp.Body.String())
	}

	cartridges := srv.resources.repo.ListCartridges(context.Background())
	count := 0
	for _, cartridge := range cartridges {
		if cartridge != nil && cartridge.LibraryID == "lib-add-empty-slot" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected add slot to leave cartridge count at 2, got %d", count)
	}
	slots := readExistingSlotLabels("lib-add-empty-slot", "drive-add-empty-slot")
	if len(slots) != 3 {
		t.Fatalf("expected 3 slots after expansion, got %#v", slots)
	}
	if slots[0] != "VTA000L06" || slots[1] != "VTA001L06" || slots[2] != "" {
		t.Fatalf("expected new slot to be empty, got %#v", slots)
	}
}

func TestLibrarySlotSyncRepairsDuplicateExistingLabels(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)

	srv := newTestServer(t)
	setupSlotFlowLibrary(t, srv, "lib-duplicate-slot-labels", "drive-duplicate-slot-labels", 3)
	createSlotFlowCartridge(t, srv, "lib-duplicate-slot-labels", "VTA000L06", false)
	createSlotFlowCartridge(t, srv, "lib-duplicate-slot-labels", "VTA001L06", false)

	slotsPath := filepath.Join(mediaStateDir, "lib-duplicate-slot-labels__drive-duplicate-slot-labels.slots")
	if err := os.WriteFile(slotsPath, []byte("VTA000L06\nVTA001L06\nVTA000L06\n"), 0o644); err != nil {
		t.Fatalf("write corrupted slots: %v", err)
	}

	if err := srv.resources.syncLibrarySlotsToSharedState(context.Background(), "lib-duplicate-slot-labels"); err != nil {
		t.Fatalf("sync library slots: %v", err)
	}
	slots := readExistingSlotLabels("lib-duplicate-slot-labels", "drive-duplicate-slot-labels")
	if len(slots) != 3 {
		t.Fatalf("expected 3 slots after repair, got %#v", slots)
	}
	if slots[0] != "VTA000L06" || slots[1] != "VTA001L06" || slots[2] != "" {
		t.Fatalf("expected duplicate label to be cleared, got %#v", slots)
	}
}

func TestLibrarySlotSyncLeavesDeletedSlotEmptyWithUnassignedBacklog(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)

	srv := newTestServer(t)
	setupSlotFlowLibrary(t, srv, "lib-delete-hole", "drive-delete-hole", 3)
	createSlotFlowCartridge(t, srv, "lib-delete-hole", "VTA000L06", false)
	createSlotFlowCartridge(t, srv, "lib-delete-hole", "VTA001L06", false)
	createSlotFlowCartridge(t, srv, "lib-delete-hole", "VTA002L06", false)
	createUnassignedSlotFlowCartridge(t, srv, "lib-delete-hole", "VTA003L06")

	deleteReq := newAuthedRequest(http.MethodPost, "/v1/cartridges/VTA001L06/delete", nil)
	deleteResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected delete cartridge 204, got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}

	slots := readExistingSlotLabels("lib-delete-hole", "drive-delete-hole")
	if len(slots) != 3 {
		t.Fatalf("expected 3 slots after delete, got %#v", slots)
	}
	if slots[0] != "VTA000L06" || slots[1] != "" || slots[2] != "VTA002L06" {
		t.Fatalf("expected deleted slot to remain empty, got %#v", slots)
	}
}

func TestLibrarySlotSyncClearsStaleInventoryWithoutFillingBacklog(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)

	srv := newTestServer(t)
	setupSlotFlowLibrary(t, srv, "lib-stale-inventory", "drive-stale-inventory", 2)
	createUnassignedSlotFlowCartridge(t, srv, "lib-stale-inventory", "VTA003L06")

	slotsPath := filepath.Join(mediaStateDir, "lib-stale-inventory__drive-stale-inventory.slots")
	if err := os.WriteFile(slotsPath, []byte("VTA001L06\n-\n"), 0o644); err != nil {
		t.Fatalf("write stale slots: %v", err)
	}

	if err := srv.resources.syncLibrarySlotsToSharedState(context.Background(), "lib-stale-inventory"); err != nil {
		t.Fatalf("sync library slots: %v", err)
	}
	slots := readExistingSlotLabels("lib-stale-inventory", "drive-stale-inventory")
	if len(slots) != 2 {
		t.Fatalf("expected 2 slots after stale repair, got %#v", slots)
	}
	if slots[0] != "" || slots[1] != "" {
		t.Fatalf("expected stale inventory to clear without filling backlog, got %#v", slots)
	}
}

func TestLibrarySlotSyncRepairsLegacyAssignedSlotsFromInventory(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)

	srv := newTestServer(t)
	setupSlotFlowLibrary(t, srv, "lib-legacy-assign", "drive-legacy-assign", 3)
	createUnassignedSlotFlowCartridge(t, srv, "lib-legacy-assign", "VTA000L06")
	createUnassignedSlotFlowCartridge(t, srv, "lib-legacy-assign", "VTA001L06")

	slotsPath := filepath.Join(mediaStateDir, "lib-legacy-assign__drive-legacy-assign.slots")
	if err := os.WriteFile(slotsPath, []byte("VTA000L06\n-\nVTA001L06\n"), 0o644); err != nil {
		t.Fatalf("write legacy slots: %v", err)
	}

	if err := srv.resources.syncLibrarySlotsToSharedState(context.Background(), "lib-legacy-assign"); err != nil {
		t.Fatalf("sync library slots: %v", err)
	}
	first, err := srv.resources.repo.FindCartridge(context.Background(), "VTA000L06")
	if err != nil {
		t.Fatalf("find first cartridge: %v", err)
	}
	second, err := srv.resources.repo.FindCartridge(context.Background(), "VTA001L06")
	if err != nil {
		t.Fatalf("find second cartridge: %v", err)
	}
	if first.AssignedSlotAddress == nil || *first.AssignedSlotAddress != 1 {
		t.Fatalf("expected VTA000L06 assigned to slot 1, got %#v", first.AssignedSlotAddress)
	}
	if second.AssignedSlotAddress == nil || *second.AssignedSlotAddress != 3 {
		t.Fatalf("expected VTA001L06 assigned to slot 3, got %#v", second.AssignedSlotAddress)
	}
	slots := readExistingSlotLabels("lib-legacy-assign", "drive-legacy-assign")
	if len(slots) != 3 || slots[0] != "VTA000L06" || slots[1] != "" || slots[2] != "VTA001L06" {
		t.Fatalf("expected legacy inventory to remain stable, got %#v", slots)
	}

	auditReq := newAuthedRequest(http.MethodGet, "/v1/audit/events", nil)
	auditResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(auditResp, auditReq)
	if auditResp.Code != http.StatusOK {
		t.Fatalf("expected audit list 200, got %d body=%s", auditResp.Code, auditResp.Body.String())
	}
	if !strings.Contains(auditResp.Body.String(), "cartridge_slot_repaired") || !strings.Contains(auditResp.Body.String(), "legacy_unassigned") {
		t.Fatalf("expected legacy slot repair audit event, got %s", auditResp.Body.String())
	}
}

func TestLibrarySlotSyncUsesInventorySnapshotAcrossDrives(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)

	srv := newTestServer(t)
	setupSlotFlowLibrary(t, srv, "lib-multi-drive-inventory", "drive-multi-b", 2)
	createDriveReq := newAuthedRequest(http.MethodPost, "/v1/drives", bytes.NewBufferString(`{"driveId":"drive-multi-a","libraryId":"lib-multi-drive-inventory","slot":257}`))
	createDriveResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createDriveResp, createDriveReq)
	if createDriveResp.Code != http.StatusCreated {
		t.Fatalf("expected second drive 201, got %d body=%s", createDriveResp.Code, createDriveResp.Body.String())
	}
	createUnassignedSlotFlowCartridge(t, srv, "lib-multi-drive-inventory", "VTA003L06")

	emptyFirstDrivePath := filepath.Join(mediaStateDir, "lib-multi-drive-inventory__drive-multi-a.slots")
	if err := os.WriteFile(emptyFirstDrivePath, []byte("-\n-\n"), 0o644); err != nil {
		t.Fatalf("write empty first drive slots: %v", err)
	}
	inventorySecondDrivePath := filepath.Join(mediaStateDir, "lib-multi-drive-inventory__drive-multi-b.slots")
	if err := os.WriteFile(inventorySecondDrivePath, []byte("VTA001L06\n-\n"), 0o644); err != nil {
		t.Fatalf("write inventory second drive slots: %v", err)
	}

	if err := srv.resources.syncLibrarySlotsToSharedState(context.Background(), "lib-multi-drive-inventory"); err != nil {
		t.Fatalf("sync library slots: %v", err)
	}
	slots := readExistingSlotLabels("lib-multi-drive-inventory", "drive-multi-a")
	if len(slots) != 2 {
		t.Fatalf("expected 2 slots after sync, got %#v", slots)
	}
	if slots[0] != "" || slots[1] != "" {
		t.Fatalf("expected stale inventory snapshot to prevent backlog fill on every drive, got %#v", slots)
	}
}

func TestLibrarySlotSyncDoesNotFillBacklogWhenSnapshotIsEmpty(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)

	srv := newTestServer(t)
	setupSlotFlowLibrary(t, srv, "lib-empty-snapshot", "drive-empty-snapshot", 2)
	createUnassignedSlotFlowCartridge(t, srv, "lib-empty-snapshot", "VTA003L06")

	if err := srv.resources.syncLibrarySlotsToSharedState(context.Background(), "lib-empty-snapshot"); err != nil {
		t.Fatalf("sync library slots: %v", err)
	}
	slots := readExistingSlotLabels("lib-empty-snapshot", "drive-empty-snapshot")
	if len(slots) != 2 {
		t.Fatalf("expected 2 slots after sync, got %#v", slots)
	}
	if slots[0] != "" || slots[1] != "" {
		t.Fatalf("expected empty snapshot to remain empty instead of filling backlog, got %#v", slots)
	}
}

func TestLibrarySlotSyncBootstrapsUnassignedCartridgesWithoutExistingInventory(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)

	srv := newTestServer(t)
	setupSlotFlowLibrary(t, srv, "lib-bootstrap-unassigned", "drive-bootstrap-unassigned", 3)
	slotsPath := filepath.Join(mediaStateDir, "lib-bootstrap-unassigned__drive-bootstrap-unassigned.slots")
	if err := os.Remove(slotsPath); err != nil {
		t.Fatalf("remove bootstrap slots snapshot: %v", err)
	}
	createUnassignedSlotFlowCartridge(t, srv, "lib-bootstrap-unassigned", "VTA000L06")
	createUnassignedSlotFlowCartridge(t, srv, "lib-bootstrap-unassigned", "VTA001L06")

	if err := srv.resources.syncLibrarySlotsToSharedState(context.Background(), "lib-bootstrap-unassigned"); err != nil {
		t.Fatalf("sync library slots: %v", err)
	}
	slots := readExistingSlotLabels("lib-bootstrap-unassigned", "drive-bootstrap-unassigned")
	if len(slots) != 3 {
		t.Fatalf("expected 3 slots after bootstrap, got %#v", slots)
	}
	if slots[0] != "VTA000L06" || slots[1] != "VTA001L06" || slots[2] != "" {
		t.Fatalf("expected unassigned legacy cartridges to bootstrap into empty inventory, got %#v", slots)
	}
}

func TestLibrarySlotSyncExcludesLoadedDriveMedia(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)

	srv := newTestServer(t)
	reqs := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-loaded-sync","name":"Pool Loaded Sync","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-loaded-sync","name":"Library Loaded Sync","slotCount":3}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-loaded-sync","libraryId":"lib-loaded-sync","slot":1}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-loaded-sync","cartridgeId":"VTA000L06","libraryId":"lib-loaded-sync","barcode":"VTA000L06","capacityBytes":1073741824}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-loaded-sync","cartridgeId":"VTA001L06","libraryId":"lib-loaded-sync","barcode":"VTA001L06","capacityBytes":1073741824}`, http.StatusCreated},
	}
	for _, tc := range reqs {
		req := newAuthedRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != tc.code {
			t.Fatalf("%s %s expected %d got %d body=%s", tc.method, tc.path, tc.code, resp.Code, resp.Body.String())
		}
	}

	statePath := filepath.Join(mediaStateDir, "lib-loaded-sync__drive-loaded-sync.state")
	if err := os.WriteFile(statePath, []byte("cartridge=VTA000L06\n"), 0o644); err != nil {
		t.Fatalf("write loaded state: %v", err)
	}
	req := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-loaded-sync","cartridgeId":"VTA002L06","libraryId":"lib-loaded-sync","barcode":"VTA002L06","capacityBytes":1073741824}`))
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("create third cartridge expected 201 got %d body=%s", resp.Code, resp.Body.String())
	}

	slotsPath := filepath.Join(mediaStateDir, "lib-loaded-sync__drive-loaded-sync.slots")
	raw, err := os.ReadFile(slotsPath)
	if err != nil {
		t.Fatalf("read slots file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if lines[0] != "-" || lines[1] != "VTA001L06" || lines[2] != "VTA002L06" {
		t.Fatalf("expected loaded cartridge removed from slots, got %q", string(raw))
	}
}

func TestDriveDeleteRemovesSlotsFile(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)

	srv := newTestServer(t)
	reqs := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-drive-delete","name":"Pool Drive Delete","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-drive-delete","name":"Library Drive Delete"}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-drive-delete","cartridgeId":"VTA000L06","libraryId":"lib-drive-delete","barcode":"VTA000L06","capacityBytes":1073741824}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-delete-a","libraryId":"lib-drive-delete","slot":1}`, http.StatusCreated},
	}
	for _, tc := range reqs {
		req := newAuthedRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != tc.code {
			t.Fatalf("%s %s expected %d got %d body=%s", tc.method, tc.path, tc.code, resp.Code, resp.Body.String())
		}
	}

	slotsPath := filepath.Join(mediaStateDir, "lib-drive-delete__drive-delete-a.slots")
	if _, err := os.Stat(slotsPath); err != nil {
		t.Fatalf("expected slots file before drive delete: %v", err)
	}

	deleteReq := newAuthedRequest(http.MethodDelete, "/v1/drives/drive-delete-a", nil)
	deleteResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected drive delete 204, got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}
	if _, err := os.Stat(slotsPath); !os.IsNotExist(err) {
		t.Fatalf("expected slots file removed after drive delete, err=%v", err)
	}
}

func TestLibraryDeleteCascadesDrivesAndCartridges(t *testing.T) {
	mediaStateDir := t.TempDir()
	storageRoot := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	t.Setenv("HOLO_STORAGE_ROOT", storageRoot)

	srv := newTestServer(t)
	reqs := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-lib-delete","name":"Pool Library Delete","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-delete","name":"Library Delete"}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-delete-1","libraryId":"lib-delete","slot":1}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-delete-2","libraryId":"lib-delete","slot":2}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-lib-delete","cartridgeId":"VTA100L06","libraryId":"lib-delete","barcode":"VTA100L06","capacityBytes":1073741824}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-lib-delete","cartridgeId":"VTA101L06","libraryId":"lib-delete","barcode":"VTA101L06","capacityBytes":1073741824}`, http.StatusCreated},
	}
	for _, tc := range reqs {
		req := newAuthedRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != tc.code {
			t.Fatalf("%s %s expected %d got %d body=%s", tc.method, tc.path, tc.code, resp.Code, resp.Body.String())
		}
	}

	layout1 := filepath.Join(storageRoot, "cartridges", "lib-delete", "vta100l06")
	layout2 := filepath.Join(storageRoot, "cartridges", "lib-delete", "vta101l06")
	if err := os.MkdirAll(layout1, 0o755); err != nil {
		t.Fatalf("create layout1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layout1, "data.segment"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed layout1: %v", err)
	}
	if err := os.MkdirAll(layout2, 0o755); err != nil {
		t.Fatalf("create layout2: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layout2, "data.segment"), []byte("y"), 0o644); err != nil {
		t.Fatalf("seed layout2: %v", err)
	}

	if _, err := os.Stat(filepath.Join(mediaStateDir, "lib-delete__drive-delete-1.slots")); err != nil {
		t.Fatalf("expected slots file for drive 1 before library delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mediaStateDir, "lib-delete__drive-delete-2.slots")); err != nil {
		t.Fatalf("expected slots file for drive 2 before library delete: %v", err)
	}

	deleteReq := newAuthedRequest(http.MethodDelete, "/v1/libraries/lib-delete", nil)
	deleteResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected library delete 204, got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}

	getLibraryReq := newAuthedRequest(http.MethodGet, "/v1/libraries/lib-delete", nil)
	getLibraryResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getLibraryResp, getLibraryReq)
	if getLibraryResp.Code != http.StatusNotFound {
		t.Fatalf("expected deleted library 404, got %d", getLibraryResp.Code)
	}
	getDriveReq := newAuthedRequest(http.MethodGet, "/v1/drives/drive-delete-1", nil)
	getDriveResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getDriveResp, getDriveReq)
	if getDriveResp.Code != http.StatusNotFound {
		t.Fatalf("expected deleted drive 404, got %d", getDriveResp.Code)
	}
	getCartridgeReq := newAuthedRequest(http.MethodGet, "/v1/cartridges/VTA100L06", nil)
	getCartridgeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getCartridgeResp, getCartridgeReq)
	if getCartridgeResp.Code != http.StatusNotFound {
		t.Fatalf("expected deleted cartridge 404, got %d", getCartridgeResp.Code)
	}

	if _, err := os.Stat(layout1); !os.IsNotExist(err) {
		t.Fatalf("expected layout1 removed, err=%v", err)
	}
	if _, err := os.Stat(layout2); !os.IsNotExist(err) {
		t.Fatalf("expected layout2 removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(mediaStateDir, "lib-delete__drive-delete-1.slots")); !os.IsNotExist(err) {
		t.Fatalf("expected drive 1 slots removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(mediaStateDir, "lib-delete__drive-delete-2.slots")); !os.IsNotExist(err) {
		t.Fatalf("expected drive 2 slots removed, err=%v", err)
	}
}

func TestLibraryDeleteActionEndpoint(t *testing.T) {
	srv := newTestServer(t)

	createReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-compat","name":"Library Compat"}`))
	createResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("expected create library 201, got %d body=%s", createResp.Code, createResp.Body.String())
	}

	deleteReq := newAuthedRequest(http.MethodPost, "/v1/libraries/lib-compat/delete", nil)
	deleteResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected post library delete action 204, got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}
}

func TestDriveDeleteActionEndpoint(t *testing.T) {
	srv := newTestServer(t)

	createLibReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-drive-action","name":"Library Drive Action"}`))
	createLibResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createLibResp, createLibReq)
	if createLibResp.Code != http.StatusCreated {
		t.Fatalf("expected create library 201, got %d body=%s", createLibResp.Code, createLibResp.Body.String())
	}

	createDriveReq := newAuthedRequest(http.MethodPost, "/v1/drives", bytes.NewBufferString(`{"driveId":"drive-action-1","libraryId":"lib-drive-action","slot":1}`))
	createDriveResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createDriveResp, createDriveReq)
	if createDriveResp.Code != http.StatusCreated {
		t.Fatalf("expected create drive 201, got %d body=%s", createDriveResp.Code, createDriveResp.Body.String())
	}

	deleteReq := newAuthedRequest(http.MethodPost, "/v1/drives/drive-action-1/delete", nil)
	deleteResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected post drive delete action 204, got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}
}

func TestCartridgeDeleteActionEndpoint(t *testing.T) {
	srv := newTestServer(t)

	createPoolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-cart-action","name":"Pool Cart Action","warningThresholdPct":90}`))
	createPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createPoolResp, createPoolReq)
	if createPoolResp.Code != http.StatusCreated {
		t.Fatalf("expected create pool 201, got %d body=%s", createPoolResp.Code, createPoolResp.Body.String())
	}

	createLibReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-cart-action","name":"Library Cart Action"}`))
	createLibResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createLibResp, createLibReq)
	if createLibResp.Code != http.StatusCreated {
		t.Fatalf("expected create library 201, got %d body=%s", createLibResp.Code, createLibResp.Body.String())
	}

	createCartridgeReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-cart-action","cartridgeId":"VTA999L06","libraryId":"lib-cart-action","barcode":"VTA999L06","capacityBytes":1073741824}`))
	createCartridgeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createCartridgeResp, createCartridgeReq)
	if createCartridgeResp.Code != http.StatusCreated {
		t.Fatalf("expected create cartridge 201, got %d body=%s", createCartridgeResp.Code, createCartridgeResp.Body.String())
	}

	deleteReq := newAuthedRequest(http.MethodPost, "/v1/cartridges/VTA999L06/delete", nil)
	deleteResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected post cartridge delete action 204, got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}

	recreateReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-cart-action","cartridgeId":"VTA999L06","libraryId":"lib-cart-action","barcode":"vta999l06","capacityBytes":1073741824}`))
	recreateResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(recreateResp, recreateReq)
	if recreateResp.Code != http.StatusConflict {
		t.Fatalf("expected destroyed barcode conflict 409, got %d body=%s", recreateResp.Code, recreateResp.Body.String())
	}
}

func TestCreateCartridgeAutoLabelSkipsDestroyedBarcode(t *testing.T) {
	srv := newTestServer(t)

	createPoolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-auto-label","name":"Pool Auto Label","warningThresholdPct":90}`))
	createPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createPoolResp, createPoolReq)
	if createPoolResp.Code != http.StatusCreated {
		t.Fatalf("expected create pool 201, got %d body=%s", createPoolResp.Code, createPoolResp.Body.String())
	}

	createLibReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-auto-label","name":"Library Auto Label"}`))
	createLibResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createLibResp, createLibReq)
	if createLibResp.Code != http.StatusCreated {
		t.Fatalf("expected create library 201, got %d body=%s", createLibResp.Code, createLibResp.Body.String())
	}

	createFirstReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-auto-label","cartridgeId":"VTA000L06","libraryId":"lib-auto-label","barcode":"VTA000L06","capacityBytes":1073741824}`))
	createFirstResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createFirstResp, createFirstReq)
	if createFirstResp.Code != http.StatusCreated {
		t.Fatalf("expected create cartridge 201, got %d body=%s", createFirstResp.Code, createFirstResp.Body.String())
	}

	deleteReq := newAuthedRequest(http.MethodPost, "/v1/cartridges/VTA000L06/delete", nil)
	deleteResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected delete cartridge 204, got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}

	createNextReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-auto-label","libraryId":"lib-auto-label","barcodePrefix":"VTA","capacityBytes":1073741824,"ltoGeneration":6}`))
	createNextResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createNextResp, createNextReq)
	if createNextResp.Code != http.StatusCreated {
		t.Fatalf("expected auto-label create 201, got %d body=%s", createNextResp.Code, createNextResp.Body.String())
	}
	var cartridge domain.VirtualCartridge
	if err := json.Unmarshal(createNextResp.Body.Bytes(), &cartridge); err != nil {
		t.Fatalf("decode cartridge: %v", err)
	}
	if cartridge.CartridgeID != "VTA001L06" || cartridge.Barcode != "VTA001L06" {
		t.Fatalf("expected auto label to skip retired VTA000L06, got id=%q barcode=%q", cartridge.CartridgeID, cartridge.Barcode)
	}
}

func TestCartridgeEraseActionEndpointClearsArtifactsAndKeepsBarcodeReusable(t *testing.T) {
	poolRootBase := t.TempDir()
	t.Setenv("HOLO_STORAGE_POOL_ROOT_BASE", poolRootBase)
	srv := newTestServer(t)

	createPoolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-erase-action","name":"Pool Erase Action","warningThresholdPct":90}`))
	createPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createPoolResp, createPoolReq)
	if createPoolResp.Code != http.StatusCreated {
		t.Fatalf("expected create pool 201, got %d body=%s", createPoolResp.Code, createPoolResp.Body.String())
	}

	createLibReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-erase-action","name":"Library Erase Action"}`))
	createLibResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createLibResp, createLibReq)
	if createLibResp.Code != http.StatusCreated {
		t.Fatalf("expected create library 201, got %d body=%s", createLibResp.Code, createLibResp.Body.String())
	}

	createCartridgeReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-erase-action","cartridgeId":"VTA777L06","libraryId":"lib-erase-action","barcode":"VTA777L06","capacityBytes":1073741824}`))
	createCartridgeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createCartridgeResp, createCartridgeReq)
	if createCartridgeResp.Code != http.StatusCreated {
		t.Fatalf("expected create cartridge 201, got %d body=%s", createCartridgeResp.Code, createCartridgeResp.Body.String())
	}

	layoutDir := storageutil.CanonicalCartridgeLayoutDir(storageutil.PoolStorageRoot("pool-erase-action"), "lib-erase-action", "VTA777L06")
	if err := os.MkdirAll(layoutDir, 0o755); err != nil {
		t.Fatalf("create layout dir: %v", err)
	}
	stalePath := filepath.Join(layoutDir, "stale.segment")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale artifact: %v", err)
	}

	eraseReq := newAuthedRequest(http.MethodPost, "/v1/cartridges/VTA777L06/erase", bytes.NewBufferString(`{"mode":"long","actor":"tester"}`))
	eraseResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(eraseResp, eraseReq)
	if eraseResp.Code != http.StatusOK {
		t.Fatalf("expected erase 200, got %d body=%s", eraseResp.Code, eraseResp.Body.String())
	}
	var erased domain.VirtualCartridge
	if err := json.NewDecoder(eraseResp.Body).Decode(&erased); err != nil {
		t.Fatalf("decode erase response: %v", err)
	}
	if erased.UsedBytes != 0 || erased.Barcode != "VTA777L06" {
		t.Fatalf("unexpected erased cartridge: %+v", erased)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("expected stale artifact removed, stat err=%v", err)
	}

	deleteReq := newAuthedRequest(http.MethodPost, "/v1/cartridges/VTA777L06/delete", nil)
	deleteResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected destroy after erase 204, got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}
}

func TestCartridgeShortEraseResetsKnownLayoutArtifactsOnly(t *testing.T) {
	poolRootBase := t.TempDir()
	t.Setenv("HOLO_STORAGE_POOL_ROOT_BASE", poolRootBase)
	srv := newTestServer(t)

	createPoolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-short-erase","name":"Pool Short Erase","warningThresholdPct":90}`))
	createPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createPoolResp, createPoolReq)
	if createPoolResp.Code != http.StatusCreated {
		t.Fatalf("expected create pool 201, got %d body=%s", createPoolResp.Code, createPoolResp.Body.String())
	}
	createLibReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-short-erase","name":"Library Short Erase"}`))
	createLibResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createLibResp, createLibReq)
	if createLibResp.Code != http.StatusCreated {
		t.Fatalf("expected create library 201, got %d body=%s", createLibResp.Code, createLibResp.Body.String())
	}
	createCartridgeReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-short-erase","cartridgeId":"VTA778L06","libraryId":"lib-short-erase","barcode":"VTA778L06","capacityBytes":1073741824}`))
	createCartridgeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createCartridgeResp, createCartridgeReq)
	if createCartridgeResp.Code != http.StatusCreated {
		t.Fatalf("expected create cartridge 201, got %d body=%s", createCartridgeResp.Code, createCartridgeResp.Body.String())
	}

	layoutDir := storageutil.CanonicalCartridgeLayoutDir(storageutil.PoolStorageRoot("pool-short-erase"), "lib-short-erase", "VTA778L06")
	if err := os.MkdirAll(layoutDir, 0o755); err != nil {
		t.Fatalf("create layout dir: %v", err)
	}
	dataPath := filepath.Join(layoutDir, "data_000000.seg")
	notePath := filepath.Join(layoutDir, "operator-note.txt")
	if err := os.WriteFile(dataPath, []byte("data"), 0o644); err != nil {
		t.Fatalf("write data artifact: %v", err)
	}
	if err := os.WriteFile(notePath, []byte("note"), 0o644); err != nil {
		t.Fatalf("write note artifact: %v", err)
	}

	eraseReq := newAuthedRequest(http.MethodPost, "/v1/cartridges/VTA778L06/erase", bytes.NewBufferString(`{"mode":"short","actor":"tester"}`))
	eraseResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(eraseResp, eraseReq)
	if eraseResp.Code != http.StatusOK {
		t.Fatalf("expected erase 200, got %d body=%s", eraseResp.Code, eraseResp.Body.String())
	}
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatalf("expected known data artifact removed, stat err=%v", err)
	}
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("expected non-layout file preserved, stat err=%v", err)
	}
}

func TestDriveLoadUnloadAndCartridgeVaultActions(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	createPoolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-move-action","name":"Pool Move Action","warningThresholdPct":90}`))
	createPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createPoolResp, createPoolReq)
	if createPoolResp.Code != http.StatusCreated {
		t.Fatalf("expected create pool 201, got %d body=%s", createPoolResp.Code, createPoolResp.Body.String())
	}

	createLibReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-move-action","name":"Library Move Action","slotCount":20,"slotStartAddress":1024}`))
	createLibResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createLibResp, createLibReq)
	if createLibResp.Code != http.StatusCreated {
		t.Fatalf("expected create library 201, got %d body=%s", createLibResp.Code, createLibResp.Body.String())
	}

	createDriveReq := newAuthedRequest(http.MethodPost, "/v1/drives", bytes.NewBufferString(`{"driveId":"drive-move-1","libraryId":"lib-move-action","slot":256}`))
	createDriveResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createDriveResp, createDriveReq)
	if createDriveResp.Code != http.StatusCreated {
		t.Fatalf("expected create drive 201, got %d body=%s", createDriveResp.Code, createDriveResp.Body.String())
	}

	createCartridgeReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-move-action","cartridgeId":"VTA100L06","libraryId":"lib-move-action","barcode":"VTA100L06","capacityBytes":549755813888}`))
	createCartridgeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createCartridgeResp, createCartridgeReq)
	if createCartridgeResp.Code != http.StatusCreated {
		t.Fatalf("expected create cartridge 201, got %d body=%s", createCartridgeResp.Code, createCartridgeResp.Body.String())
	}

	loadReq := newAuthedRequest(http.MethodPost, "/v1/drives/drive-move-1/load", bytes.NewBufferString(`{"cartridgeId":"VTA100L06","actor":"web-console"}`))
	loadResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(loadResp, loadReq)
	if loadResp.Code != http.StatusOK {
		t.Fatalf("expected load 200, got %d body=%s", loadResp.Code, loadResp.Body.String())
	}
	var loaded domain.VirtualDrive
	if err := json.Unmarshal(loadResp.Body.Bytes(), &loaded); err != nil {
		t.Fatalf("decode load response: %v", err)
	}
	if loaded.MountState != domain.MountLoaded || loaded.MountedCartridgeID != "VTA100L06" {
		t.Fatalf("expected loaded drive, got state=%s cartridge=%s", loaded.MountState, loaded.MountedCartridgeID)
	}

	exportMountedReq := newAuthedRequest(http.MethodPost, "/v1/cartridges/VTA100L06/export", nil)
	exportMountedResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(exportMountedResp, exportMountedReq)
	if exportMountedResp.Code != http.StatusBadRequest {
		t.Fatalf("expected export mounted cartridge 400, got %d body=%s", exportMountedResp.Code, exportMountedResp.Body.String())
	}

	unloadReq := newAuthedRequest(http.MethodPost, "/v1/drives/drive-move-1/unload", bytes.NewBufferString(`{"actor":"web-console"}`))
	unloadResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(unloadResp, unloadReq)
	if unloadResp.Code != http.StatusOK {
		t.Fatalf("expected unload 200, got %d body=%s", unloadResp.Code, unloadResp.Body.String())
	}
	var unloaded domain.VirtualDrive
	if err := json.Unmarshal(unloadResp.Body.Bytes(), &unloaded); err != nil {
		t.Fatalf("decode unload response: %v", err)
	}
	if unloaded.MountState != domain.MountEmpty || unloaded.MountedCartridgeID != "" {
		t.Fatalf("expected empty drive, got state=%s cartridge=%s", unloaded.MountState, unloaded.MountedCartridgeID)
	}

	exportReq := newAuthedRequest(http.MethodPost, "/v1/cartridges/VTA100L06/export", bytes.NewBufferString(`{"actor":"web-console"}`))
	exportResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(exportResp, exportReq)
	if exportResp.Code != http.StatusOK {
		t.Fatalf("expected export 200, got %d body=%s", exportResp.Code, exportResp.Body.String())
	}
	var exported domain.VirtualCartridge
	if err := json.Unmarshal(exportResp.Body.Bytes(), &exported); err != nil {
		t.Fatalf("decode export response: %v", err)
	}
	if exported.LifecycleState != domain.CartridgeExported {
		t.Fatalf("expected exported cartridge, got %s", exported.LifecycleState)
	}
	if ieLabels := readExistingIELabels("lib-move-action", "drive-move-1"); len(ieLabels) != 0 {
		t.Fatalf("expected shared IE port empty after export-to-vault sync, got %#v", ieLabels)
	}
	vaultLabels := readExistingVaultLabels("lib-move-action", "drive-move-1")
	if len(vaultLabels) != 1 || vaultLabels[0] != "VTA100L06" {
		t.Fatalf("expected exported cartridge in shared vault state, got %#v", vaultLabels)
	}

	importReq := newAuthedRequest(http.MethodPost, "/v1/cartridges/VTA100L06/import", bytes.NewBufferString(`{"actor":"web-console"}`))
	importResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(importResp, importReq)
	if importResp.Code != http.StatusOK {
		t.Fatalf("expected import 200, got %d body=%s", importResp.Code, importResp.Body.String())
	}
	var imported domain.VirtualCartridge
	if err := json.Unmarshal(importResp.Body.Bytes(), &imported); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if imported.LifecycleState != domain.CartridgeAvailable {
		t.Fatalf("expected available cartridge after import, got %s", imported.LifecycleState)
	}
	if ieLabels := readExistingIELabels("lib-move-action", "drive-move-1"); len(ieLabels) != 0 {
		t.Fatalf("expected shared IE port empty after import, got %#v", ieLabels)
	}
	if vaultLabels := readExistingVaultLabels("lib-move-action", "drive-move-1"); len(vaultLabels) != 0 {
		t.Fatalf("expected shared vault empty after import, got %#v", vaultLabels)
	}
}

func TestDriveLoadWritesSharedStateAndKeepsSlotPositions(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	createPoolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-slot-sync","name":"Pool Slot Sync","warningThresholdPct":90}`))
	createPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createPoolResp, createPoolReq)
	if createPoolResp.Code != http.StatusCreated {
		t.Fatalf("expected create pool 201, got %d body=%s", createPoolResp.Code, createPoolResp.Body.String())
	}

	createLibReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-slot-sync","name":"Library Slot Sync","slotCount":4,"slotStartAddress":1024}`))
	createLibResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createLibResp, createLibReq)
	if createLibResp.Code != http.StatusCreated {
		t.Fatalf("expected create library 201, got %d body=%s", createLibResp.Code, createLibResp.Body.String())
	}

	createDriveReq := newAuthedRequest(http.MethodPost, "/v1/drives", bytes.NewBufferString(`{"driveId":"drive-slot-1","libraryId":"lib-slot-sync","slot":256}`))
	createDriveResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createDriveResp, createDriveReq)
	if createDriveResp.Code != http.StatusCreated {
		t.Fatalf("expected create drive 201, got %d body=%s", createDriveResp.Code, createDriveResp.Body.String())
	}

	for _, cartridgeID := range []string{"VTA000L06", "VTA001L06"} {
		body := `{"poolId":"pool-slot-sync","cartridgeId":"` + cartridgeID + `","libraryId":"lib-slot-sync","barcode":"` + cartridgeID + `","capacityBytes":549755813888}`
		createCartridgeReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(body))
		createCartridgeResp := httptest.NewRecorder()
		srv.Router().ServeHTTP(createCartridgeResp, createCartridgeReq)
		if createCartridgeResp.Code != http.StatusCreated {
			t.Fatalf("expected create cartridge 201, got %d body=%s", createCartridgeResp.Code, createCartridgeResp.Body.String())
		}
	}

	loadReq := newAuthedRequest(http.MethodPost, "/v1/drives/drive-slot-1/load", bytes.NewBufferString(`{"cartridgeId":"VTA000L06","actor":"web-console"}`))
	loadResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(loadResp, loadReq)
	if loadResp.Code != http.StatusOK {
		t.Fatalf("expected load 200, got %d body=%s", loadResp.Code, loadResp.Body.String())
	}

	loaded, err := readDriveMediaState("lib-slot-sync", "drive-slot-1")
	if err != nil {
		t.Fatalf("read drive media state: %v", err)
	}
	if loaded != "VTA000L06" {
		t.Fatalf("expected shared drive state to load VTA000L06, got %q", loaded)
	}
	slots := readExistingSlotLabels("lib-slot-sync", "drive-slot-1")
	if len(slots) < 2 {
		t.Fatalf("expected at least two shared slots, got %#v", slots)
	}
	if slots[0] != "" || slots[1] != "VTA001L06" {
		t.Fatalf("expected loaded tape removed without shifting slot 2, got %#v", slots[:2])
	}
}

func TestCreateCartridgeRequiresExplicitSlotExpansionWhenFull(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	requests := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-full-slots","name":"Pool Full Slots","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-full-slots","name":"Library Full Slots","slotCount":1,"slotStartAddress":1024}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-full-slots","libraryId":"lib-full-slots","slot":256}`, http.StatusCreated},
	}
	for _, request := range requests {
		req := newAuthedRequest(request.method, request.path, bytes.NewBufferString(request.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != request.code {
			t.Fatalf("%s %s expected %d got %d body=%s", request.method, request.path, request.code, resp.Code, resp.Body.String())
		}
	}

	firstReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-full-slots","cartridgeId":"VTA001L06","libraryId":"lib-full-slots","barcode":"VTA001L06","capacityBytes":549755813888}`))
	firstResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(firstResp, firstReq)
	if firstResp.Code != http.StatusCreated {
		t.Fatalf("expected first cartridge 201, got %d body=%s", firstResp.Code, firstResp.Body.String())
	}
	var first domain.VirtualCartridge
	if err := json.Unmarshal(firstResp.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first cartridge: %v", err)
	}
	if first.AssignedSlotAddress == nil || *first.AssignedSlotAddress != 1024 {
		t.Fatalf("expected first cartridge assigned to slot 1024, got %+v", first.AssignedSlotAddress)
	}

	fullReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-full-slots","cartridgeId":"VTA002L06","libraryId":"lib-full-slots","barcode":"VTA002L06","capacityBytes":549755813888}`))
	fullResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(fullResp, fullReq)
	if fullResp.Code != http.StatusConflict {
		t.Fatalf("expected full ordinary create 409, got %d body=%s", fullResp.Code, fullResp.Body.String())
	}

	expandReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-full-slots","cartridgeId":"VTA002L06","libraryId":"lib-full-slots","barcode":"VTA002L06","capacityBytes":549755813888,"expandSlots":true}`))
	expandResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(expandResp, expandReq)
	if expandResp.Code != http.StatusCreated {
		t.Fatalf("expected explicit expansion create 201, got %d body=%s", expandResp.Code, expandResp.Body.String())
	}
	var expanded domain.VirtualCartridge
	if err := json.Unmarshal(expandResp.Body.Bytes(), &expanded); err != nil {
		t.Fatalf("decode expanded cartridge: %v", err)
	}
	if expanded.AssignedSlotAddress == nil || *expanded.AssignedSlotAddress != 1025 {
		t.Fatalf("expected expanded cartridge assigned to slot 1025, got %+v", expanded.AssignedSlotAddress)
	}

	getLibReq := newAuthedRequest(http.MethodGet, "/v1/libraries/lib-full-slots", nil)
	getLibResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getLibResp, getLibReq)
	if getLibResp.Code != http.StatusOK {
		t.Fatalf("expected library get 200, got %d body=%s", getLibResp.Code, getLibResp.Body.String())
	}
	var library domain.VirtualLibrary
	if err := json.Unmarshal(getLibResp.Body.Bytes(), &library); err != nil {
		t.Fatalf("decode library: %v", err)
	}
	if library.SlotCount != 2 {
		t.Fatalf("expected explicit expansion to increase slot count to 2, got %d", library.SlotCount)
	}

	auditReq := newAuthedRequest(http.MethodGet, "/v1/audit/events", nil)
	auditResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(auditResp, auditReq)
	if auditResp.Code != http.StatusOK {
		t.Fatalf("expected audit list 200, got %d body=%s", auditResp.Code, auditResp.Body.String())
	}
	if !strings.Contains(auditResp.Body.String(), "library_add_slots") || !strings.Contains(auditResp.Body.String(), "create_cartridge_expand_slots") {
		t.Fatalf("expected slot expansion audit event, got %s", auditResp.Body.String())
	}
	if !strings.Contains(auditResp.Body.String(), "cartridge_create") || !strings.Contains(auditResp.Body.String(), `"assignedSlotAddress":1025`) {
		t.Fatalf("expected cartridge create audit event, got %s", auditResp.Body.String())
	}
}

func TestExpandedCartridgeCreateFailureRollsBackSlotExpansion(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	setupSlotFlowLibrary(t, srv, "lib-expand-rollback", "drive-expand-rollback", 1)
	createSlotFlowCartridge(t, srv, "lib-expand-rollback", "VTA070L06", false)

	duplicateReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-lib-expand-rollback","cartridgeId":"VTA070L06","libraryId":"lib-expand-rollback","barcode":"VTA070L06","capacityBytes":549755813888,"expandSlots":true}`))
	duplicateResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(duplicateResp, duplicateReq)
	if duplicateResp.Code != http.StatusConflict {
		t.Fatalf("expected duplicate expanded cartridge create 409, got %d body=%s", duplicateResp.Code, duplicateResp.Body.String())
	}

	getLibReq := newAuthedRequest(http.MethodGet, "/v1/libraries/lib-expand-rollback", nil)
	getLibResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getLibResp, getLibReq)
	if getLibResp.Code != http.StatusOK {
		t.Fatalf("expected library get 200, got %d body=%s", getLibResp.Code, getLibResp.Body.String())
	}
	var library domain.VirtualLibrary
	if err := json.Unmarshal(getLibResp.Body.Bytes(), &library); err != nil {
		t.Fatalf("decode library: %v", err)
	}
	if library.SlotCount != 1 {
		t.Fatalf("expected failed expanded create to roll slot count back to 1, got %d", library.SlotCount)
	}

	auditReq := newAuthedRequest(http.MethodGet, "/v1/audit/events", nil)
	auditResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(auditResp, auditReq)
	if auditResp.Code != http.StatusOK {
		t.Fatalf("expected audit list 200, got %d body=%s", auditResp.Code, auditResp.Body.String())
	}
	if strings.Contains(auditResp.Body.String(), "create_cartridge_expand_slots") {
		t.Fatalf("did not expect expansion audit for failed cartridge create, got %s", auditResp.Body.String())
	}
}

func TestAddLibrarySlotsRejectsNegativeCountAndAuditsSuccess(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	setupSlotFlowLibrary(t, srv, "lib-add-slot-audit", "drive-add-slot-audit", 2)
	badReq := newAuthedRequest(http.MethodPost, "/v1/libraries/lib-add-slot-audit/slots", bytes.NewBufferString(`{"count":-2,"actor":"tester"}`))
	badResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(badResp, badReq)
	if badResp.Code != http.StatusBadRequest {
		t.Fatalf("expected negative slot count 400, got %d body=%s", badResp.Code, badResp.Body.String())
	}

	addReq := newAuthedRequest(http.MethodPost, "/v1/libraries/lib-add-slot-audit/slots", bytes.NewBufferString(`{"count":2,"actor":"tester"}`))
	addResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(addResp, addReq)
	if addResp.Code != http.StatusOK {
		t.Fatalf("expected add slots 200, got %d body=%s", addResp.Code, addResp.Body.String())
	}
	auditReq := newAuthedRequest(http.MethodGet, "/v1/audit/events", nil)
	auditResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(auditResp, auditReq)
	if auditResp.Code != http.StatusOK {
		t.Fatalf("expected audit list 200, got %d body=%s", auditResp.Code, auditResp.Body.String())
	}
	if !strings.Contains(auditResp.Body.String(), "library_add_slots") || !strings.Contains(auditResp.Body.String(), `"addedSlots":2`) {
		t.Fatalf("expected add slots audit event, got %s", auditResp.Body.String())
	}
}

func TestMissingLibraryDoesNotAllocateSlotLock(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	createPoolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-missing-lock","name":"Pool Missing Lock","warningThresholdPct":90}`))
	createPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createPoolResp, createPoolReq)
	if createPoolResp.Code != http.StatusCreated {
		t.Fatalf("expected pool create 201, got %d body=%s", createPoolResp.Code, createPoolResp.Body.String())
	}

	createCartridgeReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-missing-lock","cartridgeId":"VTA404L06","libraryId":"lib-missing-lock","barcode":"VTA404L06","capacityBytes":549755813888}`))
	createCartridgeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createCartridgeResp, createCartridgeReq)
	if createCartridgeResp.Code != http.StatusNotFound {
		t.Fatalf("expected missing library create 404, got %d body=%s", createCartridgeResp.Code, createCartridgeResp.Body.String())
	}

	addSlotReq := newAuthedRequest(http.MethodPost, "/v1/libraries/lib-missing-lock/slots", bytes.NewBufferString(`{"count":1}`))
	addSlotResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(addSlotResp, addSlotReq)
	if addSlotResp.Code != http.StatusNotFound {
		t.Fatalf("expected missing library add-slot 404, got %d body=%s", addSlotResp.Code, addSlotResp.Body.String())
	}

	srv.resources.slotLocksMu.Lock()
	defer srv.resources.slotLocksMu.Unlock()
	if got := len(srv.resources.slotLocks); got != 0 {
		t.Fatalf("expected no slot locks allocated for missing library requests, got %d", got)
	}
}

func TestDeleteLibraryRemovesSlotLock(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	setupSlotFlowLibrary(t, srv, "lib-delete-lock", "drive-delete-lock", 1)
	if _, err := srv.resources.addLibrarySlots(context.Background(), "lib-delete-lock", 1, "tester"); err != nil {
		t.Fatalf("seed slot lock: %v", err)
	}
	srv.resources.slotLocksMu.Lock()
	if got := len(srv.resources.slotLocks); got != 1 {
		srv.resources.slotLocksMu.Unlock()
		t.Fatalf("expected one slot lock before delete, got %d", got)
	}
	srv.resources.slotLocksMu.Unlock()

	deleteReq := newAuthedRequest(http.MethodDelete, "/v1/libraries/lib-delete-lock", nil)
	deleteResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusNoContent {
		t.Fatalf("expected library delete 204, got %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}

	srv.resources.slotLocksMu.Lock()
	defer srv.resources.slotLocksMu.Unlock()
	if got := len(srv.resources.slotLocks); got != 0 {
		t.Fatalf("expected slot lock removed after library delete, got %d", got)
	}
}

func TestConcurrentExpandedCartridgeCreatesGetDistinctSlots(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	setupSlotFlowLibrary(t, srv, "lib-concurrent-expand", "drive-concurrent-expand", 1)
	createSlotFlowCartridge(t, srv, "lib-concurrent-expand", "VTA060L06", false)

	var wg sync.WaitGroup
	errs := make(chan string, 2)
	for _, id := range []string{"VTA061L06", "VTA062L06"} {
		wg.Add(1)
		go func(cartridgeID string) {
			defer wg.Done()
			body := `{"poolId":"pool-lib-concurrent-expand","cartridgeId":"` + cartridgeID + `","libraryId":"lib-concurrent-expand","barcode":"` + cartridgeID + `","capacityBytes":549755813888,"expandSlots":true}`
			req := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(body))
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			if resp.Code != http.StatusCreated {
				errs <- cartridgeID + ": " + resp.Body.String()
			}
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent create failed: %s", err)
	}

	assigned := make(map[int]string)
	for _, id := range []string{"VTA060L06", "VTA061L06", "VTA062L06"} {
		cartridge, err := srv.resources.repo.FindCartridge(context.Background(), id)
		if err != nil {
			t.Fatalf("find cartridge %s: %v", id, err)
		}
		if cartridge.AssignedSlotAddress == nil {
			t.Fatalf("expected assigned slot for %s", id)
		}
		if other, exists := assigned[*cartridge.AssignedSlotAddress]; exists {
			t.Fatalf("slot %d assigned to both %s and %s", *cartridge.AssignedSlotAddress, other, id)
		}
		assigned[*cartridge.AssignedSlotAddress] = id
	}
}

func TestDriveUnloadReturnsToAssignedSlot(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	requests := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-assigned-unload","name":"Pool Assigned Unload","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-assigned-unload","name":"Library Assigned Unload","slotCount":2,"slotStartAddress":1024}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-assigned-unload","libraryId":"lib-assigned-unload","slot":256}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-assigned-unload","cartridgeId":"VTA010L06","libraryId":"lib-assigned-unload","barcode":"VTA010L06","capacityBytes":549755813888}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-assigned-unload","cartridgeId":"VTA011L06","libraryId":"lib-assigned-unload","barcode":"VTA011L06","capacityBytes":549755813888}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives/drive-assigned-unload/load", `{"cartridgeId":"VTA011L06","actor":"web-console"}`, http.StatusOK},
		{http.MethodPost, "/v1/drives/drive-assigned-unload/unload", `{"actor":"web-console"}`, http.StatusOK},
	}
	for _, request := range requests {
		req := newAuthedRequest(request.method, request.path, bytes.NewBufferString(request.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != request.code {
			t.Fatalf("%s %s expected %d got %d body=%s", request.method, request.path, request.code, resp.Code, resp.Body.String())
		}
	}

	getCartridgeReq := newAuthedRequest(http.MethodGet, "/v1/cartridges/VTA011L06", nil)
	getCartridgeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getCartridgeResp, getCartridgeReq)
	if getCartridgeResp.Code != http.StatusOK {
		t.Fatalf("expected cartridge get 200, got %d body=%s", getCartridgeResp.Code, getCartridgeResp.Body.String())
	}
	var cartridge domain.VirtualCartridge
	if err := json.Unmarshal(getCartridgeResp.Body.Bytes(), &cartridge); err != nil {
		t.Fatalf("decode cartridge: %v", err)
	}
	if cartridge.CurrentElementAddress == nil || *cartridge.CurrentElementAddress != 1025 {
		t.Fatalf("expected unloaded cartridge back in slot 1025, got %+v", cartridge.CurrentElementAddress)
	}
}

func TestDriveUnloadRepairsLegacyMissingAssignedSlot(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)
	ctx := context.Background()

	requests := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-legacy-unload","name":"Pool Legacy Unload","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-legacy-unload","name":"Library Legacy Unload","slotCount":2,"slotStartAddress":1024}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-legacy-unload","libraryId":"lib-legacy-unload","slot":256}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-legacy-unload","cartridgeId":"VTA012L06","libraryId":"lib-legacy-unload","barcode":"VTA012L06","capacityBytes":549755813888}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives/drive-legacy-unload/load", `{"cartridgeId":"VTA012L06","actor":"web-console"}`, http.StatusOK},
	}
	for _, request := range requests {
		req := newAuthedRequest(request.method, request.path, bytes.NewBufferString(request.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != request.code {
			t.Fatalf("%s %s expected %d got %d body=%s", request.method, request.path, request.code, resp.Code, resp.Body.String())
		}
	}

	cartridge, err := srv.resources.repo.FindCartridge(ctx, "VTA012L06")
	if err != nil {
		t.Fatalf("find legacy mounted cartridge: %v", err)
	}
	cartridge.AssignedSlotAddress = nil
	if err := srv.resources.repo.SaveCartridge(ctx, cartridge); err != nil {
		t.Fatalf("clear legacy assigned slot: %v", err)
	}

	unloadReq := newAuthedRequest(http.MethodPost, "/v1/drives/drive-legacy-unload/unload", bytes.NewBufferString(`{"actor":"web-console"}`))
	unloadResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(unloadResp, unloadReq)
	if unloadResp.Code != http.StatusOK {
		t.Fatalf("expected legacy unload 200, got %d body=%s", unloadResp.Code, unloadResp.Body.String())
	}

	getReq := newAuthedRequest(http.MethodGet, "/v1/cartridges/VTA012L06", nil)
	getResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getResp, getReq)
	if getResp.Code != http.StatusOK {
		t.Fatalf("expected cartridge get 200, got %d body=%s", getResp.Code, getResp.Body.String())
	}
	var repaired domain.VirtualCartridge
	if err := json.Unmarshal(getResp.Body.Bytes(), &repaired); err != nil {
		t.Fatalf("decode repaired cartridge: %v", err)
	}
	if repaired.AssignedSlotAddress == nil || *repaired.AssignedSlotAddress != 1024 {
		t.Fatalf("expected unload to repair assigned slot 1024, got %+v", repaired.AssignedSlotAddress)
	}
	if repaired.CurrentElementAddress == nil || *repaired.CurrentElementAddress != 1024 {
		t.Fatalf("expected legacy unload to return cartridge to slot 1024, got %+v", repaired.CurrentElementAddress)
	}
}

func TestDriveUnloadRejectsOccupiedAssignedSlot(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)
	ctx := context.Background()

	requests := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-occupied-assigned","name":"Pool Occupied Assigned","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-occupied-assigned","name":"Library Occupied Assigned","slotCount":2,"slotStartAddress":1024}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-occupied-assigned","libraryId":"lib-occupied-assigned","slot":256}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-occupied-assigned","cartridgeId":"VTA020L06","libraryId":"lib-occupied-assigned","barcode":"VTA020L06","capacityBytes":549755813888}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-occupied-assigned","cartridgeId":"VTA021L06","libraryId":"lib-occupied-assigned","barcode":"VTA021L06","capacityBytes":549755813888}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives/drive-occupied-assigned/load", `{"cartridgeId":"VTA020L06","actor":"web-console"}`, http.StatusOK},
	}
	for _, request := range requests {
		req := newAuthedRequest(request.method, request.path, bytes.NewBufferString(request.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != request.code {
			t.Fatalf("%s %s expected %d got %d body=%s", request.method, request.path, request.code, resp.Code, resp.Body.String())
		}
	}

	mounted, err := srv.resources.repo.FindCartridge(ctx, "VTA020L06")
	if err != nil {
		t.Fatalf("find mounted cartridge: %v", err)
	}
	if mounted.AssignedSlotAddress == nil {
		t.Fatalf("expected mounted cartridge to retain assigned slot")
	}
	occupier, err := srv.resources.repo.FindCartridge(ctx, "VTA021L06")
	if err != nil {
		t.Fatalf("find occupying cartridge: %v", err)
	}
	assignedSlot := *mounted.AssignedSlotAddress
	occupier.AssignedSlotAddress = &assignedSlot
	if err := srv.resources.repo.SaveCartridge(ctx, occupier); err != nil {
		t.Fatalf("save occupying cartridge: %v", err)
	}

	unloadReq := newAuthedRequest(http.MethodPost, "/v1/drives/drive-occupied-assigned/unload", bytes.NewBufferString(`{"actor":"web-console"}`))
	unloadResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(unloadResp, unloadReq)
	if unloadResp.Code != http.StatusConflict {
		t.Fatalf("expected occupied assigned slot unload 409, got %d body=%s", unloadResp.Code, unloadResp.Body.String())
	}
}

func TestVaultImportRepairsLegacyAssignedSlotAndReturnsThere(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)
	ctx := context.Background()

	setupSlotFlowLibrary(t, srv, "lib-vault-legacy", "drive-vault-legacy", 4)
	createSlotFlowCartridge(t, srv, "lib-vault-legacy", "VTA030L06", false)
	createSlotFlowCartridge(t, srv, "lib-vault-legacy", "VTA031L06", false)

	for _, id := range []string{"VTA030L06", "VTA031L06"} {
		cartridge, err := srv.resources.repo.FindCartridge(ctx, id)
		if err != nil {
			t.Fatalf("find cartridge %s: %v", id, err)
		}
		cartridge.AssignedSlotAddress = nil
		if err := srv.resources.repo.SaveCartridge(ctx, cartridge); err != nil {
			t.Fatalf("clear assigned slot %s: %v", id, err)
		}
	}
	if err := srv.resources.syncLibrarySlotsToSharedState(ctx, "lib-vault-legacy"); err != nil {
		t.Fatalf("sync library slots: %v", err)
	}

	exportSlotFlowCartridge(t, srv, "VTA030L06")
	exportSlotFlowCartridge(t, srv, "VTA031L06")
	importSlotFlowCartridge(t, srv, "VTA031L06", http.StatusOK)

	resp := httptest.NewRecorder()
	req := newAuthedRequest(http.MethodGet, "/v1/cartridges/VTA031L06", nil)
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected cartridge get 200, got %d body=%s", resp.Code, resp.Body.String())
	}
	var cartridge domain.VirtualCartridge
	if err := json.Unmarshal(resp.Body.Bytes(), &cartridge); err != nil {
		t.Fatalf("decode cartridge: %v", err)
	}
	if cartridge.AssignedSlotAddress == nil || *cartridge.AssignedSlotAddress != 2 {
		t.Fatalf("expected legacy export to repair assigned slot 2, got %+v", cartridge.AssignedSlotAddress)
	}
	if cartridge.CurrentElementAddress == nil || *cartridge.CurrentElementAddress != 2 {
		t.Fatalf("expected imported cartridge to return to slot 2, got %+v", cartridge.CurrentElementAddress)
	}
}

func TestVaultImportReassignsWhenAssignedSlotWasReused(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	setupSlotFlowLibrary(t, srv, "lib-vault-reassign", "drive-vault-reassign", 3)
	createSlotFlowCartridge(t, srv, "lib-vault-reassign", "VTA040L06", false)
	createSlotFlowCartridge(t, srv, "lib-vault-reassign", "VTA041L06", false)
	exportSlotFlowCartridge(t, srv, "VTA040L06")
	reused := createSlotFlowCartridge(t, srv, "lib-vault-reassign", "VTA042L06", false)
	if reused.AssignedSlotAddress == nil || *reused.AssignedSlotAddress != 1 {
		t.Fatalf("expected new cartridge to reuse exported slot 1, got %+v", reused.AssignedSlotAddress)
	}

	importSlotFlowCartridge(t, srv, "VTA040L06", http.StatusOK)

	resp := httptest.NewRecorder()
	req := newAuthedRequest(http.MethodGet, "/v1/cartridges/VTA040L06", nil)
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected cartridge get 200, got %d body=%s", resp.Code, resp.Body.String())
	}
	var cartridge domain.VirtualCartridge
	if err := json.Unmarshal(resp.Body.Bytes(), &cartridge); err != nil {
		t.Fatalf("decode cartridge: %v", err)
	}
	if cartridge.AssignedSlotAddress == nil || *cartridge.AssignedSlotAddress != 3 {
		t.Fatalf("expected import to reassign cartridge to next empty slot 3, got %+v", cartridge.AssignedSlotAddress)
	}
	if cartridge.CurrentElementAddress == nil || *cartridge.CurrentElementAddress != 3 {
		t.Fatalf("expected imported cartridge to occupy slot 3, got %+v", cartridge.CurrentElementAddress)
	}

	auditReq := newAuthedRequest(http.MethodGet, "/v1/audit/events", nil)
	auditResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(auditResp, auditReq)
	if auditResp.Code != http.StatusOK {
		t.Fatalf("expected audit list 200, got %d body=%s", auditResp.Code, auditResp.Body.String())
	}
	if !strings.Contains(auditResp.Body.String(), "cartridge_slot_reassigned") || !strings.Contains(auditResp.Body.String(), "vault_import_assigned_slot_unavailable") {
		t.Fatalf("expected vault import reassignment audit event, got %s", auditResp.Body.String())
	}
}

func TestVaultImportFailsWhenAssignedSlotReusedAndNoEmptySlot(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	setupSlotFlowLibrary(t, srv, "lib-vault-full", "drive-vault-full", 2)
	createSlotFlowCartridge(t, srv, "lib-vault-full", "VTA050L06", false)
	createSlotFlowCartridge(t, srv, "lib-vault-full", "VTA051L06", false)
	exportSlotFlowCartridge(t, srv, "VTA050L06")
	createSlotFlowCartridge(t, srv, "lib-vault-full", "VTA052L06", false)

	importSlotFlowCartridge(t, srv, "VTA050L06", http.StatusConflict)
}

func setupSlotFlowLibrary(t *testing.T, srv *Server, libraryID, driveID string, slotCount int) {
	t.Helper()
	requests := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-` + libraryID + `","name":"Pool ` + libraryID + `","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"` + libraryID + `","name":"` + libraryID + `","slotCount":` + strconv.Itoa(slotCount) + `,"slotStartAddress":1}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"` + driveID + `","libraryId":"` + libraryID + `","slot":256}`, http.StatusCreated},
	}
	for _, request := range requests {
		req := newAuthedRequest(request.method, request.path, bytes.NewBufferString(request.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != request.code {
			t.Fatalf("%s %s expected %d got %d body=%s", request.method, request.path, request.code, resp.Code, resp.Body.String())
		}
	}
}

func createSlotFlowCartridge(t *testing.T, srv *Server, libraryID, cartridgeID string, expand bool) domain.VirtualCartridge {
	t.Helper()
	body := `{"poolId":"pool-` + libraryID + `","cartridgeId":"` + cartridgeID + `","libraryId":"` + libraryID + `","barcode":"` + cartridgeID + `","capacityBytes":549755813888`
	if expand {
		body += `,"expandSlots":true`
	}
	body += `}`
	req := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(body))
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected create cartridge %s 201, got %d body=%s", cartridgeID, resp.Code, resp.Body.String())
	}
	var cartridge domain.VirtualCartridge
	if err := json.Unmarshal(resp.Body.Bytes(), &cartridge); err != nil {
		t.Fatalf("decode cartridge: %v", err)
	}
	return cartridge
}

func createUnassignedSlotFlowCartridge(t *testing.T, srv *Server, libraryID, cartridgeID string) {
	t.Helper()
	cartridge := domain.NewVirtualCartridge(cartridgeID, "pool-"+libraryID, libraryID, cartridgeID, 549755813888)
	if err := srv.resources.repo.CreateCartridge(context.Background(), cartridge); err != nil {
		t.Fatalf("create unassigned cartridge %s: %v", cartridgeID, err)
	}
}

func exportSlotFlowCartridge(t *testing.T, srv *Server, cartridgeID string) {
	t.Helper()
	req := newAuthedRequest(http.MethodPost, "/v1/cartridges/"+cartridgeID+"/export", bytes.NewBufferString(`{"actor":"web-console"}`))
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected export cartridge %s 200, got %d body=%s", cartridgeID, resp.Code, resp.Body.String())
	}
}

func importSlotFlowCartridge(t *testing.T, srv *Server, cartridgeID string, expected int) {
	t.Helper()
	req := newAuthedRequest(http.MethodPost, "/v1/cartridges/"+cartridgeID+"/import", bytes.NewBufferString(`{"actor":"web-console"}`))
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != expected {
		t.Fatalf("expected import cartridge %s %d, got %d body=%s", cartridgeID, expected, resp.Code, resp.Body.String())
	}
}

func TestCartridgeListIncludesCurrentSlotElementAddress(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	requests := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-elements","name":"Pool Elements","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-elements","name":"Library Elements","slotCount":12,"slotStartAddress":1024}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-elements","libraryId":"lib-elements","slot":256}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-elements","cartridgeId":"VTA000L06","libraryId":"lib-elements","barcode":"VTA000L06","capacityBytes":549755813888}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-elements","cartridgeId":"VTA001L06","libraryId":"lib-elements","barcode":"VTA001L06","capacityBytes":549755813888}`, http.StatusCreated},
	}
	for _, request := range requests {
		req := newAuthedRequest(request.method, request.path, bytes.NewBufferString(request.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != request.code {
			t.Fatalf("%s %s expected %d got %d body=%s", request.method, request.path, request.code, resp.Code, resp.Body.String())
		}
	}

	slotsPath := filepath.Join(mediaStateDir, sanitizeStateID("lib-elements__drive-elements")+".slots")
	if err := writeAtomicText(slotsPath, "VTA000L06\n-\n-\n-\n-\n-\n-\n-\n-\n-\nVTA001L06\n-\n"); err != nil {
		t.Fatalf("write shared slot state: %v", err)
	}

	listReq := newAuthedRequest(http.MethodGet, "/v1/cartridges", nil)
	listResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("expected cartridge list 200, got %d body=%s", listResp.Code, listResp.Body.String())
	}
	var cartridges []domain.VirtualCartridge
	if err := json.Unmarshal(listResp.Body.Bytes(), &cartridges); err != nil {
		t.Fatalf("decode cartridge list: %v", err)
	}
	addressByID := make(map[string]int)
	for _, cartridge := range cartridges {
		if cartridge.CurrentElementAddress != nil {
			addressByID[cartridge.CartridgeID] = *cartridge.CurrentElementAddress
		}
	}
	if addressByID["VTA000L06"] != 1024 {
		t.Fatalf("expected VTA000L06 at element 1024, got %d", addressByID["VTA000L06"])
	}
	if addressByID["VTA001L06"] != 1034 {
		t.Fatalf("expected VTA001L06 at element 1034, got %d", addressByID["VTA001L06"])
	}
}

func TestCartridgeElementAddressUsesNewestDriveSlotFile(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	requests := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-newest","name":"Pool Newest","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-newest","name":"Library Newest","slotCount":12,"slotStartAddress":1024}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-a","libraryId":"lib-newest","slot":256}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-b","libraryId":"lib-newest","slot":257}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-newest","cartridgeId":"VTA010L06","libraryId":"lib-newest","barcode":"VTA010L06","capacityBytes":549755813888}`, http.StatusCreated},
	}
	for _, request := range requests {
		req := newAuthedRequest(request.method, request.path, bytes.NewBufferString(request.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != request.code {
			t.Fatalf("%s %s expected %d got %d body=%s", request.method, request.path, request.code, resp.Code, resp.Body.String())
		}
	}

	stalePath := filepath.Join(mediaStateDir, sanitizeStateID("lib-newest__drive-a")+".slots")
	freshPath := filepath.Join(mediaStateDir, sanitizeStateID("lib-newest__drive-b")+".slots")
	if err := writeAtomicText(stalePath, "VTA010L06\n-\n-\n-\n-\n-\n-\n-\n-\n-\n-\n-\n"); err != nil {
		t.Fatalf("write stale slots: %v", err)
	}
	if err := writeAtomicText(freshPath, "-\n-\n-\n-\n-\n-\n-\n-\n-\n-\nVTA010L06\n-\n"); err != nil {
		t.Fatalf("write fresh slots: %v", err)
	}
	oldTime := time.Now().Add(-time.Hour)
	newTime := time.Now()
	if err := os.Chtimes(stalePath, oldTime, oldTime); err != nil {
		t.Fatalf("set stale mtime: %v", err)
	}
	if err := os.Chtimes(freshPath, newTime, newTime); err != nil {
		t.Fatalf("set fresh mtime: %v", err)
	}

	req := newAuthedRequest(http.MethodGet, "/v1/cartridges/VTA010L06", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected cartridge get 200, got %d body=%s", resp.Code, resp.Body.String())
	}
	var cartridge domain.VirtualCartridge
	if err := json.Unmarshal(resp.Body.Bytes(), &cartridge); err != nil {
		t.Fatalf("decode cartridge: %v", err)
	}
	if cartridge.CurrentElementAddress == nil || *cartridge.CurrentElementAddress != 1034 {
		t.Fatalf("expected newest slot file to place cartridge at 1034, got %#v body=%s", cartridge.CurrentElementAddress, resp.Body.String())
	}
}

func TestCartridgeElementAddressOmittedWhenMissingOrMounted(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	requests := []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{http.MethodPost, "/v1/storage/pools", `{"poolId":"pool-omit","name":"Pool Omit","warningThresholdPct":90}`, http.StatusCreated},
		{http.MethodPost, "/v1/libraries", `{"libraryId":"lib-omit","name":"Library Omit","slotCount":4,"slotStartAddress":1024}`, http.StatusCreated},
		{http.MethodPost, "/v1/drives", `{"driveId":"drive-omit","libraryId":"lib-omit","slot":256}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-omit","cartridgeId":"VTA020L06","libraryId":"lib-omit","barcode":"VTA020L06","capacityBytes":549755813888}`, http.StatusCreated},
		{http.MethodPost, "/v1/cartridges", `{"poolId":"pool-omit","cartridgeId":"VTA021L06","libraryId":"lib-omit","barcode":"VTA021L06","capacityBytes":549755813888}`, http.StatusCreated},
	}
	for _, request := range requests {
		req := newAuthedRequest(request.method, request.path, bytes.NewBufferString(request.body))
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		if resp.Code != request.code {
			t.Fatalf("%s %s expected %d got %d body=%s", request.method, request.path, request.code, resp.Code, resp.Body.String())
		}
	}

	slotsPath := filepath.Join(mediaStateDir, sanitizeStateID("lib-omit__drive-omit")+".slots")
	if err := writeAtomicText(slotsPath, "VTA021L06\n-\n-\n-\n"); err != nil {
		t.Fatalf("write shared slot state: %v", err)
	}
	if err := writeDriveMediaState("lib-omit", "drive-omit", "VTA021L06"); err != nil {
		t.Fatalf("write shared loaded state: %v", err)
	}

	req := newAuthedRequest(http.MethodGet, "/v1/cartridges", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected cartridge list 200, got %d body=%s", resp.Code, resp.Body.String())
	}
	var cartridges []domain.VirtualCartridge
	if err := json.Unmarshal(resp.Body.Bytes(), &cartridges); err != nil {
		t.Fatalf("decode cartridge list: %v", err)
	}
	for _, cartridge := range cartridges {
		switch cartridge.CartridgeID {
		case "VTA020L06":
			if cartridge.CurrentElementAddress != nil {
				t.Fatalf("expected missing cartridge address to be omitted, got %#v", cartridge.CurrentElementAddress)
			}
		case "VTA021L06":
			if cartridge.LifecycleState != domain.CartridgeMounted {
				t.Fatalf("expected loaded cartridge to reconcile mounted, got %s", cartridge.LifecycleState)
			}
			if cartridge.CurrentElementAddress != nil {
				t.Fatalf("expected mounted cartridge address to be omitted, got %#v", cartridge.CurrentElementAddress)
			}
		}
	}
}

func TestResourceListsReconcileSharedDriveState(t *testing.T) {
	mediaStateDir := t.TempDir()
	t.Setenv("HOLO_MEDIA_STATE_DIR", mediaStateDir)
	srv := newTestServer(t)

	createPoolReq := newAuthedRequest(http.MethodPost, "/v1/storage/pools", bytes.NewBufferString(`{"poolId":"pool-reconcile","name":"Pool Reconcile","warningThresholdPct":90}`))
	createPoolResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createPoolResp, createPoolReq)
	if createPoolResp.Code != http.StatusCreated {
		t.Fatalf("expected create pool 201, got %d body=%s", createPoolResp.Code, createPoolResp.Body.String())
	}

	createLibReq := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString(`{"libraryId":"lib-reconcile","name":"Library Reconcile","slotCount":4,"slotStartAddress":1024}`))
	createLibResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createLibResp, createLibReq)
	if createLibResp.Code != http.StatusCreated {
		t.Fatalf("expected create library 201, got %d body=%s", createLibResp.Code, createLibResp.Body.String())
	}

	createDriveReq := newAuthedRequest(http.MethodPost, "/v1/drives", bytes.NewBufferString(`{"driveId":"drive-reconcile-1","libraryId":"lib-reconcile","slot":256}`))
	createDriveResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createDriveResp, createDriveReq)
	if createDriveResp.Code != http.StatusCreated {
		t.Fatalf("expected create drive 201, got %d body=%s", createDriveResp.Code, createDriveResp.Body.String())
	}

	createCartridgeReq := newAuthedRequest(http.MethodPost, "/v1/cartridges", bytes.NewBufferString(`{"poolId":"pool-reconcile","cartridgeId":"VTA777L06","libraryId":"lib-reconcile","barcode":"VTA777L06","capacityBytes":549755813888}`))
	createCartridgeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(createCartridgeResp, createCartridgeReq)
	if createCartridgeResp.Code != http.StatusCreated {
		t.Fatalf("expected create cartridge 201, got %d body=%s", createCartridgeResp.Code, createCartridgeResp.Body.String())
	}

	if err := writeDriveMediaState("lib-reconcile", "drive-reconcile-1", "VTA777L06"); err != nil {
		t.Fatalf("write shared loaded state: %v", err)
	}
	if err := writeAtomicText(cartridgeMetadataPath("VTA777L06"), "cartridge_id=VTA777L06\ncapacity_bytes=549755813888\nused_bytes=1048576\n"); err != nil {
		t.Fatalf("write shared cartridge metadata: %v", err)
	}
	listDriveReq := newAuthedRequest(http.MethodGet, "/v1/drives", nil)
	listDriveResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(listDriveResp, listDriveReq)
	if listDriveResp.Code != http.StatusOK {
		t.Fatalf("expected drive list 200, got %d body=%s", listDriveResp.Code, listDriveResp.Body.String())
	}
	if !strings.Contains(listDriveResp.Body.String(), `"mountedCartridgeId":"VTA777L06"`) {
		t.Fatalf("expected drive list to reconcile loaded cartridge, got %s", listDriveResp.Body.String())
	}

	getCartridgeReq := newAuthedRequest(http.MethodGet, "/v1/cartridges/VTA777L06", nil)
	getCartridgeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getCartridgeResp, getCartridgeReq)
	if getCartridgeResp.Code != http.StatusOK {
		t.Fatalf("expected cartridge get 200, got %d body=%s", getCartridgeResp.Code, getCartridgeResp.Body.String())
	}
	if !strings.Contains(getCartridgeResp.Body.String(), `"lifecycleState":"mounted"`) {
		t.Fatalf("expected cartridge get to reconcile mounted lifecycle, got %s", getCartridgeResp.Body.String())
	}
	if !strings.Contains(getCartridgeResp.Body.String(), `"usedBytes":1048576`) {
		t.Fatalf("expected cartridge get to reconcile used bytes, got %s", getCartridgeResp.Body.String())
	}

	if err := writeDriveMediaState("lib-reconcile", "drive-reconcile-1", ""); err != nil {
		t.Fatalf("clear shared loaded state: %v", err)
	}
	getDriveReq := newAuthedRequest(http.MethodGet, "/v1/drives/drive-reconcile-1", nil)
	getDriveResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getDriveResp, getDriveReq)
	if getDriveResp.Code != http.StatusOK {
		t.Fatalf("expected drive get 200, got %d body=%s", getDriveResp.Code, getDriveResp.Body.String())
	}
	if !strings.Contains(getDriveResp.Body.String(), `"mountState":"empty"`) || strings.Contains(getDriveResp.Body.String(), "mountedCartridgeId") {
		t.Fatalf("expected drive get to reconcile empty mount, got %s", getDriveResp.Body.String())
	}

	iePath := filepath.Join(mediaStateDir, sanitizeStateID("lib-reconcile__drive-reconcile-1")+".ie")
	if err := writeAtomicText(iePath, "VTA777L06\n"); err != nil {
		t.Fatalf("write shared IE state: %v", err)
	}
	getExportedReq := newAuthedRequest(http.MethodGet, "/v1/cartridges/VTA777L06", nil)
	getExportedResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getExportedResp, getExportedReq)
	if getExportedResp.Code != http.StatusOK {
		t.Fatalf("expected exported cartridge get 200, got %d body=%s", getExportedResp.Code, getExportedResp.Body.String())
	}
	if !strings.Contains(getExportedResp.Body.String(), `"lifecycleState":"exported"`) {
		t.Fatalf("expected cartridge get to reconcile IE export lifecycle, got %s", getExportedResp.Body.String())
	}
}

func TestRespondJSONReturns500WhenEncodingFails(t *testing.T) {
	resp := httptest.NewRecorder()
	respondJSON(resp, http.StatusOK, failingJSON{})
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when JSON encoding fails, got %d", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), "internal server error") {
		t.Fatalf("expected safe internal server error body, got %s", resp.Body.String())
	}
}

func TestNormalizeDeviceProfileMapsMarketedModels(t *testing.T) {
	cases := []struct {
		vendor   string
		model    string
		expected string
	}{
		{vendor: "IBM", model: "IBM TS2290", expected: "ibm-ult3580-td9"},
		{vendor: "HP/HPE", model: "HPE StoreEver LTO-9 Ultrium 45000", expected: "hpe-ultrium-9-scsi"},
		{vendor: "Quantum", model: "Quantum Scalar i500", expected: "adic-scalar-i500"},
		{vendor: "Oracle / StorageTek", model: "StorageTek SL150", expected: "stk-sl150"},
		{vendor: "Spectra Logic", model: "Spectra TFinity ExaScale", expected: "spectra-tfinity-exascale"},
	}
	for _, tc := range cases {
		if got := normalizeDeviceProfile(tc.vendor, tc.model, "fallback"); got != tc.expected {
			t.Fatalf("normalizeDeviceProfile(%q, %q) = %q, want %q", tc.vendor, tc.model, got, tc.expected)
		}
	}
}

func TestResourcesRejectControlCharactersInIDsAndProfiles(t *testing.T) {
	srv := newTestServer(t)
	req := newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString("{\"libraryId\":\"lib-bad\\n\",\"name\":\"Library\",\"libraryType\":\"ibm-03584l32\"}"))
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected control-character library id 400, got %d body=%s", resp.Code, resp.Body.String())
	}

	req = newAuthedRequest(http.MethodPost, "/v1/libraries", bytes.NewBufferString("{\"libraryId\":\"lib-profile\",\"name\":\"Library\",\"libraryType\":\"bad\\nprofile\"}"))
	resp = httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected control-character profile 400, got %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestRemoveAllApprovedLayoutPathRejectsOutsideRoot(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("create outside dir: %v", err)
	}
	if err := removeAllApprovedLayoutPath(outside, []string{filepath.Join(t.TempDir(), "allowed")}); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("expected outside cleanup to be rejected, got %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside dir should remain, err=%v", err)
	}
}
