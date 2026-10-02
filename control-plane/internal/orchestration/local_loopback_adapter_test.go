package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type fakeLocalLoopbackHelperRunner struct {
	path     string
	input    []byte
	reply    []byte
	err      error
	deadline time.Time
}

func (r *fakeLocalLoopbackHelperRunner) Run(ctx context.Context, path string, input []byte) ([]byte, error) {
	r.path = path
	r.input = append([]byte(nil), input...)
	r.deadline, _ = ctx.Deadline()
	return r.reply, r.err
}

func TestLocalLoopbackAdapterPassesOnlyTypedMappingAndUsesBoundedHelperCall(t *testing.T) {
	runner := &fakeLocalLoopbackHelperRunner{reply: []byte(`{"version":1,"ok":true,"result":[{"deviceKey":"changer:library-a","state":"connected","observedPaths":["/dev/sg0"]},{"deviceKey":"drive:drive-a","state":"connected","observedPaths":["/dev/sg1","/dev/st0"]}]}`)}
	adapter := NewLocalLoopbackAdapter(defaultLocalLoopbackHelperPath, true)
	adapter.runner = runner
	library := testLoopbackLibraryMapping()
	devices := []domain.LocalLoopbackDeviceMapping{testLoopbackChangerMapping(), testLoopbackDriveMapping()}

	observed, err := adapter.Ensure(context.Background(), library, devices)
	if err != nil || len(observed) != 2 || observed[1].State != domain.LocalMountDeviceStateConnected {
		t.Fatalf("unexpected loopback observations: %+v err=%v", observed, err)
	}
	if runner.path != defaultLocalLoopbackHelperPath {
		t.Fatalf("helper path changed: %q", runner.path)
	}
	var request map[string]any
	if err := json.Unmarshal(runner.input, &request); err != nil {
		t.Fatalf("decode helper request: %v", err)
	}
	if request["operation"] != "ensure" || strings.Contains(string(runner.input), "backstorePath") || strings.Contains(string(runner.input), "secret") {
		t.Fatalf("helper received an unexpected request: %s", runner.input)
	}
	mapping, ok := request["mapping"].(map[string]any)
	if !ok {
		t.Fatalf("helper request omitted its mapping: %s", runner.input)
	}
	wireDevices, ok := mapping["devices"].([]any)
	if !ok || len(wireDevices) != len(devices) {
		t.Fatalf("helper request has invalid device mappings: %s", runner.input)
	}
	wantLUNs := map[string]float64{"changer:library-a": 0, "drive:drive-a": 5}
	for _, item := range wireDevices {
		device, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("helper request has an invalid device row: %s", runner.input)
		}
		key, _ := device["deviceKey"].(string)
		lun, hasLUN := device["lun"].(float64)
		if !hasLUN || lun != wantLUNs[key] {
			t.Fatalf("helper request must use its `lun` field for %q: %s", key, runner.input)
		}
		if _, hasLUNIndex := device["lunIndex"]; hasLUNIndex {
			t.Fatalf("helper request must not expose the repository `lunIndex` field: %s", runner.input)
		}
	}
	if runner.deadline.IsZero() || time.Until(runner.deadline) > localLoopbackHelperTimeout {
		t.Fatalf("helper call does not have a bounded deadline: %v", runner.deadline)
	}
}

func TestLocalLoopbackAdapterRejectsInvalidMappingsAndUntrustedReplies(t *testing.T) {
	runner := &fakeLocalLoopbackHelperRunner{reply: []byte(`{"version":1,"ok":true,"result":[]}`)}
	adapter := NewLocalLoopbackAdapter(defaultLocalLoopbackHelperPath, false)
	adapter.runner = runner
	if _, err := adapter.Ensure(context.Background(), testLoopbackLibraryMapping(), []domain.LocalLoopbackDeviceMapping{
		{DeviceKey: "drive:drive-a", LibraryID: "library-b", Kind: domain.LocalDeviceKindDrive, DriveID: "drive-a", LUNIndex: 1, IdentityRef: "drive-a", BackendRef: "holo_backstore_a", State: domain.LocalMappingStateActive},
	}); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("mapping from another library should be rejected before helper call, got %v", err)
	}
	if runner.input != nil {
		t.Fatal("invalid mapping reached the privileged helper")
	}

	runner.reply = []byte(`{"version":1,"ok":true,"result":[],"stderr":"untrusted"}`)
	if _, err := adapter.ListOwned(context.Background()); !errors.Is(err, ErrLocalLoopbackHelperUnavailable) {
		t.Fatalf("unknown response fields must be rejected, got %v", err)
	}
	runner.reply = []byte(`{"version":1,"ok":false,"reason":"raw stderr secret=canary"}`)
	if _, _, err := adapter.Probe(context.Background()); !errors.Is(err, ErrLocalLoopbackHelperUnavailable) {
		t.Fatalf("unknown helper reason must be rejected, got %v", err)
	}
}

func TestLocalLoopbackAdapterListsOwnedDeviceMappingsForSafeCleanup(t *testing.T) {
	runner := &fakeLocalLoopbackHelperRunner{reply: []byte(`{"version":1,"ok":true,"result":[{"libraryId":"library-a","targetNaa":"naa.50014056b18af0f5","nexusNaa":"naa.5001405db2f4505b","tpgTag":1,"devices":[{"deviceKey":"changer:library-a","kind":"changer","lun":0,"identityRef":"changer-identity-a","backendRef":"holo_backstore_changer","state":"cleanup_pending"}],"present":true}]}`)}
	adapter := NewLocalLoopbackAdapter(defaultLocalLoopbackHelperPath, false)
	adapter.runner = runner

	owners, err := adapter.ListOwned(context.Background())
	if err != nil || len(owners) != 1 {
		t.Fatalf("list owned loopback mappings: owners=%+v err=%v", owners, err)
	}
	if owners[0].TPGTag != 1 || !owners[0].Present || len(owners[0].Devices) != 1 {
		t.Fatalf("owned mapping omitted cleanup details: %+v", owners[0])
	}
	if owners[0].Devices[0].DeviceKey != "changer:library-a" || owners[0].Devices[0].State != domain.LocalMappingStateCleanupPending {
		t.Fatalf("owned device mapping was not decoded for cleanup: %+v", owners[0].Devices[0])
	}
}

func TestLocalLoopbackAdapterRejectsUnknownDeviceStateAndPath(t *testing.T) {
	devices := testLoopbackRemoveDevices()
	runner := &fakeLocalLoopbackHelperRunner{reply: []byte(`{"version":1,"ok":true,"result":[{"deviceKey":"changer:library-a","state":"connected","observedPaths":["/dev/sg0"]},{"deviceKey":"drive:drive-a","state":"residual","observedPaths":["/etc/passwd"]}]}`)}
	adapter := NewLocalLoopbackAdapter(defaultLocalLoopbackHelperPath, false)
	adapter.runner = runner
	if _, err := adapter.Remove(context.Background(), testLoopbackLibraryMapping(), devices); err == nil {
		t.Fatal("helper output containing an arbitrary filesystem path must be rejected")
	}
	runner.reply = []byte(`{"version":1,"ok":true,"result":[{"deviceKey":"changer:library-a","state":"connected","observedPaths":["/dev/sg0"]},{"deviceKey":"drive:drive-a","state":"invented","observedPaths":[]}]}`)
	if _, err := adapter.Remove(context.Background(), testLoopbackLibraryMapping(), devices); err == nil {
		t.Fatal("unknown device state must be rejected")
	}
}

func testLoopbackLibraryMapping() domain.LocalLoopbackLibraryMapping {
	return domain.LocalLoopbackLibraryMapping{LibraryID: "library-a", TargetNAA: "naa.50014056b18af0f5", NexusNAA: "naa.5001405db2f4505b", TPGTag: 1}
}

func testLoopbackDriveMapping() domain.LocalLoopbackDeviceMapping {
	return domain.LocalLoopbackDeviceMapping{
		DeviceKey: "drive:drive-a", LibraryID: "library-a", Kind: domain.LocalDeviceKindDrive,
		DriveID: "drive-a", LUNIndex: 5, IdentityRef: "drive-identity-a",
		BackendRef: "holo_backstore_drive_a", State: domain.LocalMappingStateActive,
	}
}

func testLoopbackChangerMapping() domain.LocalLoopbackDeviceMapping {
	return domain.LocalLoopbackDeviceMapping{
		DeviceKey: "changer:library-a", LibraryID: "library-a", Kind: domain.LocalDeviceKindChanger,
		LUNIndex: 0, IdentityRef: "changer-identity-a", BackendRef: "holo_backstore_changer",
		State: domain.LocalMappingStateActive,
	}
}

func testLoopbackRemoveDevices() []domain.LocalLoopbackDeviceMapping {
	changer := testLoopbackChangerMapping()
	drive := testLoopbackDriveMapping()
	drive.State = domain.LocalMappingStateCleanupPending
	return []domain.LocalLoopbackDeviceMapping{changer, drive}
}
