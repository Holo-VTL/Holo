package orchestration

import (
	"context"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type recordingLocalMountHelper struct {
	owners  []LocalLoopbackOwner
	ensures [][]domain.LocalLoopbackDeviceMapping
	removes [][]domain.LocalLoopbackDeviceMapping
}

func (h *recordingLocalMountHelper) Probe(context.Context) (bool, domain.LocalMountReasonCode, error) {
	return true, "", nil
}
func (h *recordingLocalMountHelper) ListOwned(context.Context) ([]LocalLoopbackOwner, error) {
	return append([]LocalLoopbackOwner(nil), h.owners...), nil
}
func (h *recordingLocalMountHelper) Ensure(_ context.Context, library domain.LocalLoopbackLibraryMapping, mappings []domain.LocalLoopbackDeviceMapping) ([]LocalLoopbackDeviceObservation, error) {
	h.ensures = append(h.ensures, append([]domain.LocalLoopbackDeviceMapping(nil), mappings...))
	observed := make([]LocalLoopbackDeviceObservation, 0, len(mappings))
	owner := LocalLoopbackOwner{LibraryID: library.LibraryID, Present: true}
	for _, mapping := range mappings {
		observed = append(observed, LocalLoopbackDeviceObservation{DeviceKey: mapping.DeviceKey, State: domain.LocalMountDeviceStateConnected, ObservedPaths: []string{"/dev/sg0"}})
		owner.Devices = append(owner.Devices, LocalLoopbackOwnedDevice{DeviceKey: mapping.DeviceKey, Kind: mapping.Kind, DriveID: mapping.DriveID, LUN: mapping.LUNIndex, IdentityRef: mapping.IdentityRef, BackendRef: mapping.BackendRef, State: mapping.State})
	}
	h.owners = []LocalLoopbackOwner{owner}
	return observed, nil
}
func (h *recordingLocalMountHelper) Remove(_ context.Context, _ domain.LocalLoopbackLibraryMapping, mappings []domain.LocalLoopbackDeviceMapping) ([]LocalLoopbackDeviceObservation, error) {
	h.removes = append(h.removes, append([]domain.LocalLoopbackDeviceMapping(nil), mappings...))
	h.owners = nil
	return []LocalLoopbackDeviceObservation{}, nil
}

type recordingLocalMountBackend struct {
	ensured   []domain.VTLDeviceDescriptor
	released  []domain.LocalLoopbackDeviceMapping
	lockCalls [][]string
}

func (b *recordingLocalMountBackend) EnsureLocalMountBackend(_ context.Context, descriptor domain.VTLDeviceDescriptor) error {
	b.ensured = append(b.ensured, descriptor)
	return nil
}
func (b *recordingLocalMountBackend) ReleaseLocalMountBackend(_ context.Context, mapping domain.LocalLoopbackDeviceMapping) error {
	b.released = append(b.released, mapping)
	return nil
}
func (b *recordingLocalMountBackend) WithLocalMountDeviceLocks(_ context.Context, keys []string, operation func() error) error {
	b.lockCalls = append(b.lockCalls, append([]string(nil), keys...))
	return operation()
}

func TestComposedLocalMountRuntimeCoordinatesBackendAndLoopbackAsOneOperation(t *testing.T) {
	ctx := context.Background()
	resources := newLocalMountServiceResources(t)
	settings := &fakeLocalMountSettings{enabled: true}
	mappings := memory.NewLocalMountRepo()
	helper := &recordingLocalMountHelper{}
	backend := &recordingLocalMountBackend{}
	runtime := NewComposedLocalMountRuntime(helper, backend)
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{Mode: "tcmu"})
	service.SetRuntime(resources, nil, mappings, runtime)

	status, err := service.Sync(ctx, "tester")
	if err != nil {
		t.Fatalf("sync local devices: %v", err)
	}
	if status.State != domain.LocalMountStateConnected || status.DesiredDeviceCount != 3 || status.ConnectedDeviceCount != 3 {
		t.Fatalf("unexpected product status: %+v", status)
	}
	if len(backend.ensured) != 3 || len(helper.ensures) != 1 || len(helper.ensures[0]) != 3 {
		t.Fatalf("backend and loopback did not receive the same inventory: backends=%d helper=%+v", len(backend.ensured), helper.ensures)
	}
	if len(backend.lockCalls) != 1 || len(backend.lockCalls[0]) != 3 {
		t.Fatalf("expected ordered device locks across backend ensure and helper attach: %v", backend.lockCalls)
	}
}

func TestLocalMountComposedRuntimeEnableDisableAndReenablePreservesDeviceIdentity(t *testing.T) {
	ctx := context.Background()
	resources := newLocalMountServiceResources(t)
	settings := &fakeLocalMountSettings{enabled: true}
	mappings := memory.NewLocalMountRepo()
	helper := &recordingLocalMountHelper{}
	backend := &recordingLocalMountBackend{}
	runtime := NewComposedLocalMountRuntime(helper, backend)
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{Mode: "tcmu"})
	service.SetRuntime(resources, nil, mappings, runtime)

	enabled, err := service.Sync(ctx, "tester")
	if err != nil || enabled.State != domain.LocalMountStateConnected || enabled.DesiredDeviceCount != 3 || enabled.ConnectedDeviceCount != 3 {
		t.Fatalf("enable did not connect the whole inventory: status=%+v err=%v", enabled, err)
	}
	before, err := mappings.ListDeviceMappings(ctx, "lib-a")
	if err != nil || len(before) != 3 {
		t.Fatalf("expected persistent mapping for each device: mappings=%+v err=%v", before, err)
	}

	if err := settings.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	disabled, err := service.Sync(ctx, "tester")
	if err != nil || disabled.State != domain.LocalMountStateDisabled || disabled.ResidualDeviceCount != 0 {
		t.Fatalf("disable did not confirm a clean detach: status=%+v err=%v", disabled, err)
	}
	if len(helper.removes) != 1 || len(helper.removes[0]) != 3 || len(backend.released) != 3 {
		t.Fatalf("disable did not remove local links before releasing all backends: removes=%+v releases=%+v", helper.removes, backend.released)
	}
	for _, mapping := range backend.released {
		if mapping.State != domain.LocalMappingStateCleanupPending {
			t.Fatalf("backend release must follow a marked local detach, got %+v", mapping)
		}
	}
	afterDisable, err := mappings.ListDeviceMappings(ctx, "lib-a")
	if err != nil || len(afterDisable) != len(before) {
		t.Fatalf("disable discarded stable identity mappings: mappings=%+v err=%v", afterDisable, err)
	}
	for i := range before {
		if before[i].DeviceKey != afterDisable[i].DeviceKey || before[i].LUNIndex != afterDisable[i].LUNIndex || before[i].BackendRef != afterDisable[i].BackendRef || afterDisable[i].State != domain.LocalMappingStateInactive {
			t.Fatalf("disable changed stable mapping or retained an active state: before=%+v after=%+v", before[i], afterDisable[i])
		}
	}

	if err := settings.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	reenabled, err := service.Sync(ctx, "tester")
	if err != nil || reenabled.State != domain.LocalMountStateConnected || reenabled.DesiredDeviceCount != 3 || reenabled.ConnectedDeviceCount != 3 {
		t.Fatalf("reenable did not restore the whole inventory: status=%+v err=%v", reenabled, err)
	}
	afterReenable, err := mappings.ListDeviceMappings(ctx, "lib-a")
	if err != nil || len(afterReenable) != len(before) {
		t.Fatalf("reenable changed mapping count: mappings=%+v err=%v", afterReenable, err)
	}
	for i := range before {
		if before[i].DeviceKey != afterReenable[i].DeviceKey || before[i].LUNIndex != afterReenable[i].LUNIndex || before[i].BackendRef != afterReenable[i].BackendRef || afterReenable[i].State != domain.LocalMappingStateActive {
			t.Fatalf("reenable changed stable device identity/LUN/backend: before=%+v after=%+v", before[i], afterReenable[i])
		}
	}
}
