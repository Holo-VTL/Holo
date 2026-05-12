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
