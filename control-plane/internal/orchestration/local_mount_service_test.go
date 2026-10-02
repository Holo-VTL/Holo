package orchestration

import (
	"context"
	"errors"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type fakeLocalMountSettings struct{ enabled bool }

func (s *fakeLocalMountSettings) Enabled(context.Context) (bool, error) { return s.enabled, nil }
func (s *fakeLocalMountSettings) SetEnabled(_ context.Context, enabled bool) error {
	s.enabled = enabled
	return nil
}

type fakeLocalMountRuntime struct {
	available   bool
	probeErr    error
	owners      []LocalLoopbackOwner
	observed    map[string]LocalLoopbackDeviceObservation
	ensureCalls [][]domain.LocalLoopbackDeviceMapping
	removeCalls [][]domain.LocalLoopbackDeviceMapping
	released    []string
	ensureErr   error
	removeErr   error
}

type fakeLocalMountIdentityRuntime struct {
	*fakeLocalMountRuntime
	identities map[string]string
}

func (r *fakeLocalMountIdentityRuntime) ResolveLocalMountIdentity(_ context.Context, descriptor domain.VTLDeviceDescriptor, _ *domain.TargetPublication) (string, error) {
	if identity := r.identities[descriptor.DeviceKey]; identity != "" {
		return identity, nil
	}
	return descriptor.IdentityRef, nil
}

func (r *fakeLocalMountRuntime) Probe(context.Context) (bool, domain.LocalMountReasonCode, error) {
	return r.available, "", r.probeErr
}
func (r *fakeLocalMountRuntime) ListOwned(context.Context) ([]LocalLoopbackOwner, error) {
	return append([]LocalLoopbackOwner(nil), r.owners...), nil
}
func (r *fakeLocalMountRuntime) Ensure(_ context.Context, _ domain.LocalLoopbackLibraryMapping, mappings []domain.LocalLoopbackDeviceMapping, _ []domain.VTLDeviceDescriptor) ([]LocalLoopbackDeviceObservation, error) {
	r.ensureCalls = append(r.ensureCalls, append([]domain.LocalLoopbackDeviceMapping(nil), mappings...))
	if r.ensureErr != nil {
		return failedLocalMountObservations(mappings, r.ensureErr), r.ensureErr
	}
	result := make([]LocalLoopbackDeviceObservation, 0, len(mappings))
	owned := LocalLoopbackOwner{LibraryID: mappings[0].LibraryID, Present: true}
	for _, mapping := range mappings {
		observation, ok := r.observed[mapping.DeviceKey]
		if !ok {
			observation = LocalLoopbackDeviceObservation{DeviceKey: mapping.DeviceKey, State: domain.LocalMountDeviceStateConnected, ObservedPaths: []string{"/dev/sg0"}}
		}
		result = append(result, observation)
		owned.Devices = append(owned.Devices, LocalLoopbackOwnedDevice{DeviceKey: mapping.DeviceKey, Kind: mapping.Kind, DriveID: mapping.DriveID, LUN: mapping.LUNIndex, IdentityRef: mapping.IdentityRef, BackendRef: mapping.BackendRef, State: mapping.State})
	}
	r.owners = []LocalLoopbackOwner{owned}
	return result, nil
}
func (r *fakeLocalMountRuntime) Remove(_ context.Context, library domain.LocalLoopbackLibraryMapping, mappings []domain.LocalLoopbackDeviceMapping) ([]LocalLoopbackDeviceObservation, error) {
	r.removeCalls = append(r.removeCalls, append([]domain.LocalLoopbackDeviceMapping(nil), mappings...))
	if r.removeErr != nil {
		return failedLocalMountObservations(mappings, r.removeErr), r.removeErr
	}
	active := make([]LocalLoopbackOwnedDevice, 0)
	for _, mapping := range mappings {
		if mapping.State == domain.LocalMappingStateActive {
			active = append(active, LocalLoopbackOwnedDevice{DeviceKey: mapping.DeviceKey, Kind: mapping.Kind, DriveID: mapping.DriveID, LUN: mapping.LUNIndex, IdentityRef: mapping.IdentityRef, BackendRef: mapping.BackendRef, State: mapping.State})
		}
	}
	if len(active) == 0 {
		r.owners = nil
	} else {
		r.owners = []LocalLoopbackOwner{{LibraryID: library.LibraryID, Present: true, Devices: active}}
	}
	return []LocalLoopbackDeviceObservation{}, nil
}
func (r *fakeLocalMountRuntime) ReleaseBackend(_ context.Context, mapping domain.LocalLoopbackDeviceMapping) error {
	r.released = append(r.released, mapping.DeviceKey)
	return nil
}

func TestLocalMountSyncUsesWholeLibraryInventoryWithoutPublicationOrCHAPFilter(t *testing.T) {
	ctx := context.Background()
	resources := newLocalMountServiceResources(t)
	settings := &fakeLocalMountSettings{}
	mappings := memory.NewLocalMountRepo()
	runtime := &fakeLocalMountRuntime{available: true, observed: map[string]LocalLoopbackDeviceObservation{}}
	targets := memory.NewTargetRuntimeRepo()
	service := NewLocalMountService(settings, targets, audit.NewMemoryWriter(), TargetRuntimeConfig{Mode: "tcmu"})
	service.SetRuntime(resources, nil, mappings, runtime)

	if err := settings.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	status, err := service.Sync(ctx, "tester")
	if err != nil {
		t.Fatalf("enable local mount: %v", err)
	}
	if status.State != domain.LocalMountStateConnected || status.DesiredDeviceCount != 3 || status.ConnectedDeviceCount != 3 {
		t.Fatalf("expected changer and both drives to connect without network publications, got %+v", status)
	}
	if len(runtime.ensureCalls) != 1 || len(runtime.ensureCalls[0]) != 3 {
		t.Fatalf("expected one library loopback mapping with all three devices, got %+v", runtime.ensureCalls)
	}
	if _, err := mappings.ListLibraryMappings(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := mappings.ListDeviceMappings(ctx, "lib-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 3 {
		t.Fatalf("expected persistent mappings for every configured device, got %+v", stored)
	}
	for _, device := range stored {
		if device.Kind == domain.LocalDeviceKindDrive && device.LUNIndex < 1 {
			t.Fatalf("drive got invalid LUN: %+v", device)
		}
	}
}

func TestLocalMountRetainsNotReadyLoadedDriveInDesiredCount(t *testing.T) {
	ctx := context.Background()
	resources := newLocalMountServiceResources(t)
	drive, _ := domain.NewVirtualDrive("drive-c", "lib-a", 3)
	if err := drive.Mount("cart-missing-pool"); err != nil {
		t.Fatal(err)
	}
	if err := resources.CreateDrive(ctx, drive); err != nil {
		t.Fatal(err)
	}
	if err := resources.CreateCartridge(ctx, domain.NewVirtualCartridge("cart-missing-pool", "missing-pool", "lib-a", "TAPE999", 1<<30)); err != nil {
		t.Fatal(err)
	}
	settings := &fakeLocalMountSettings{enabled: true}
	mappings := memory.NewLocalMountRepo()
	runtime := &fakeLocalMountRuntime{available: true, observed: map[string]LocalLoopbackDeviceObservation{}}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{Mode: "tcmu"})
	service.SetRuntime(resources, failingPoolReader{}, mappings, runtime)

	status, err := service.Sync(ctx, "tester")
	if err != nil {
		t.Fatalf("pool not-ready state should be reported per-device: %v", err)
	}
	if status.DesiredDeviceCount != 4 || status.ConnectedDeviceCount != 3 || status.State != domain.LocalMountStatePartial {
		t.Fatalf("pool-failed drive must remain in denominator: %+v", status)
	}
	var notReady bool
	for _, device := range status.Devices {
		if device.DeviceKey == "drive:drive-c" && device.State == domain.LocalMountDeviceStateNotReady && device.ReasonCode == domain.LocalMountReasonPoolUnavailable {
			notReady = true
		}
	}
	if !notReady {
		t.Fatalf("expected loaded drive to report pool unavailable: %+v", status.Devices)
	}
}

func TestLocalMountProbeFailureCannotMasqueradeAsZeroDeviceSuccess(t *testing.T) {
	ctx := context.Background()
	resources := newLocalMountServiceResources(t)
	settings := &fakeLocalMountSettings{enabled: true}
	runtime := &fakeLocalMountRuntime{available: false, probeErr: errors.New("loopback unavailable")}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{Mode: "tcmu"})
	service.SetRuntime(resources, nil, memory.NewLocalMountRepo(), runtime)

	status, err := service.Sync(ctx, "tester")
	if err == nil || status.State != domain.LocalMountStateFailed || status.Enabled != true {
		t.Fatalf("probe failure was not reported as failed intent: status=%+v err=%v", status, err)
	}
}

func TestLocalMountDisableDoesNotHideUnknownOwnedResidual(t *testing.T) {
	ctx := context.Background()
	settings := &fakeLocalMountSettings{enabled: false}
	runtime := &fakeLocalMountRuntime{available: true, owners: []LocalLoopbackOwner{{LibraryID: "lib-orphan", Present: true, Devices: []LocalLoopbackOwnedDevice{{DeviceKey: "changer:lib-orphan", Kind: domain.LocalDeviceKindChanger}}}}}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{Mode: "tcmu"})
	service.SetRuntime(newLocalMountServiceResources(t), nil, memory.NewLocalMountRepo(), runtime)

	status, err := service.Sync(ctx, "tester")
	if err == nil || status.State != domain.LocalMountStateFailed || status.ResidualDeviceCount != 1 {
		t.Fatalf("unknown owned mapping was incorrectly reported as disabled: status=%+v err=%v", status, err)
	}
}

func TestLocalMountDisableRemovesOnlyOwnedLocalMappingsAndReleasesBackends(t *testing.T) {
	ctx := context.Background()
	resources := newLocalMountServiceResources(t)
	settings := &fakeLocalMountSettings{}
	mappings := memory.NewLocalMountRepo()
	runtime := &fakeLocalMountRuntime{available: true, observed: map[string]LocalLoopbackDeviceObservation{}}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{Mode: "tcmu"})
	service.SetRuntime(resources, nil, mappings, runtime)

	if err := settings.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Sync(ctx, "tester"); err != nil {
		t.Fatal(err)
	}
	if err := settings.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	status, err := service.Sync(ctx, "tester")
	if err != nil {
		t.Fatalf("disable local mount: %v", err)
	}
	if status.State != domain.LocalMountStateDisabled || status.ResidualDeviceCount != 0 {
		t.Fatalf("expected confirmed clean disable, got %+v", status)
	}
	if len(runtime.removeCalls) != 1 || len(runtime.removeCalls[0]) != 3 {
		t.Fatalf("expected removal of the three owned devices: %+v", runtime.removeCalls)
	}
	if len(runtime.released) != 3 {
		t.Fatalf("expected shared backend releases after detach, got %v", runtime.released)
	}
	libraries, err := mappings.ListLibraryMappings(ctx)
	if err != nil || len(libraries) != 1 {
		t.Fatalf("expected stable library mapping retained while disabled, libraries=%+v err=%v", libraries, err)
	}
	stored, err := mappings.ListDeviceMappings(ctx, "lib-a")
	if err != nil || len(stored) != 3 {
		t.Fatalf("expected stable device/LUN mappings retained while disabled, mappings=%+v err=%v", stored, err)
	}
	for _, mapping := range stored {
		if mapping.State != domain.LocalMappingStateInactive {
			t.Fatalf("expected detached mappings to be inactive, got %+v", mapping)
		}
	}
}

func TestLocalMountKeepsStableLUNsAcrossDisableAndTopologyGrowth(t *testing.T) {
	ctx := context.Background()
	resources := newLocalMountServiceResources(t)
	settings := &fakeLocalMountSettings{}
	mappings := memory.NewLocalMountRepo()
	runtime := &fakeLocalMountRuntime{available: true, observed: map[string]LocalLoopbackDeviceObservation{}}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{Mode: "tcmu"})
	service.SetRuntime(resources, nil, mappings, runtime)

	if err := settings.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Sync(ctx, "tester"); err != nil {
		t.Fatal(err)
	}
	before, err := mappings.ListDeviceMappings(ctx, "lib-a")
	if err != nil {
		t.Fatal(err)
	}
	originalLUNs := make(map[string]int)
	for _, mapping := range before {
		originalLUNs[mapping.DeviceKey] = mapping.LUNIndex
	}
	if err := settings.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Sync(ctx, "tester"); err != nil {
		t.Fatal(err)
	}

	newDrive, err := domain.NewVirtualDrive("drive-c", "lib-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := resources.CreateDrive(ctx, newDrive); err != nil {
		t.Fatal(err)
	}
	if err := settings.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	status, err := service.Sync(ctx, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if status.DesiredDeviceCount != 4 {
		t.Fatalf("expected newly added drive in inventory, got %+v", status)
	}
	after, err := mappings.ListDeviceMappings(ctx, "lib-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, mapping := range after {
		if oldLUN, existed := originalLUNs[mapping.DeviceKey]; existed && oldLUN != mapping.LUNIndex {
			t.Fatalf("existing device LUN changed across disable/growth: %s %d -> %d", mapping.DeviceKey, oldLUN, mapping.LUNIndex)
		}
	}
}

func TestLocalMountSyncRecreatesMappingsWhenVPDIdentityChanges(t *testing.T) {
	ctx := context.Background()
	resources := newLocalMountServiceResources(t)
	descriptors, err := BuildLocalMountInventory(ctx, resources, nil)
	if err != nil {
		t.Fatal(err)
	}
	mappings := memory.NewLocalMountRepo()
	if err := mappings.SaveLibraryMapping(ctx, localLoopbackLibraryMapping("lib-a")); err != nil {
		t.Fatal(err)
	}
	owner := LocalLoopbackOwner{LibraryID: "lib-a", Present: true}
	actualIdentities := []string{"IBMChangerSerial", "IBMDriveSerialA", "IBMDriveSerialB"}
	resolved := make(map[string]string, len(descriptors))
	nextDriveLUN := 1
	for i, descriptor := range descriptors {
		lun := 0
		if descriptor.Kind == domain.LocalDeviceKindDrive {
			lun = nextDriveLUN
			nextDriveLUN++
		}
		mapping := domain.LocalLoopbackDeviceMapping{
			DeviceKey: descriptor.DeviceKey, LibraryID: descriptor.LibraryID, Kind: descriptor.Kind,
			DriveID: descriptor.DriveID, LUNIndex: lun, IdentityRef: "legacy-identity-" + string(rune('a'+i)),
			BackendRef: localMountBackendRef(descriptor.DeviceKey), State: domain.LocalMappingStateActive,
		}
		if err := mappings.SaveDeviceMapping(ctx, mapping); err != nil {
			t.Fatal(err)
		}
		resolved[descriptor.DeviceKey] = actualIdentities[i]
		owner.Devices = append(owner.Devices, LocalLoopbackOwnedDevice{
			DeviceKey: mapping.DeviceKey, Kind: mapping.Kind, DriveID: mapping.DriveID, LUN: mapping.LUNIndex,
			IdentityRef: mapping.IdentityRef, BackendRef: mapping.BackendRef, State: mapping.State,
		})
	}
	baseRuntime := &fakeLocalMountRuntime{available: true, owners: []LocalLoopbackOwner{owner}, observed: map[string]LocalLoopbackDeviceObservation{}}
	runtime := &fakeLocalMountIdentityRuntime{fakeLocalMountRuntime: baseRuntime, identities: resolved}
	settings := &fakeLocalMountSettings{enabled: true}
	service := NewLocalMountService(settings, memory.NewTargetRuntimeRepo(), audit.NewMemoryWriter(), TargetRuntimeConfig{Mode: "tcmu"})
	service.SetRuntime(resources, nil, mappings, runtime)

	status, err := service.Sync(ctx, "tester")
	if err != nil {
		t.Fatalf("sync with changed VPD identity: %v; status=%+v", err, status)
	}
	if status.State != domain.LocalMountStateConnected || status.ConnectedDeviceCount != len(descriptors) {
		t.Fatalf("expected all devices to reconnect after identity migration: %+v", status)
	}
	if len(baseRuntime.removeCalls) != 1 || len(baseRuntime.removeCalls[0]) != len(descriptors) {
		t.Fatalf("identity change must detach the complete owned loopback before remapping: %+v", baseRuntime.removeCalls)
	}
	if len(baseRuntime.released) != len(descriptors) {
		t.Fatalf("expected old local mappings to be released before recreation, got %v", baseRuntime.released)
	}
	updated, err := mappings.ListDeviceMappings(ctx, "lib-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != len(descriptors) {
		t.Fatalf("expected one persisted mapping per device, got %+v", updated)
	}
	for _, mapping := range updated {
		if mapping.IdentityRef != resolved[mapping.DeviceKey] {
			t.Fatalf("mapping did not persist its resolved VPD serial: %+v", mapping)
		}
	}
}

func newLocalMountServiceResources(t *testing.T) *memory.CoreResourcesRepo {
	t.Helper()
	ctx := context.Background()
	resources := memory.NewCoreResourcesRepo()
	library, err := domain.NewVirtualLibrary("lib-a", "Library A")
	if err != nil {
		t.Fatal(err)
	}
	if err := resources.CreateLibrary(ctx, library); err != nil {
		t.Fatal(err)
	}
	for slot, id := range []string{"drive-b", "drive-a"} {
		drive, err := domain.NewVirtualDrive(id, library.LibraryID, slot+1)
		if err != nil {
			t.Fatal(err)
		}
		if err := resources.CreateDrive(ctx, drive); err != nil {
			t.Fatal(err)
		}
	}
	return resources
}

type failingPoolReader struct{}

func (failingPoolReader) GetPool(context.Context, string) (*domain.StoragePoolRuntime, error) {
	return nil, domain.ErrNotFound
}
