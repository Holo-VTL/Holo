package orchestration

import (
	"context"
	"errors"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

type localMountInventoryFixture struct {
	libraries  []*domain.VirtualLibrary
	drives     []*domain.VirtualDrive
	cartridges []*domain.VirtualCartridge
}

func (f localMountInventoryFixture) ListLibraries(context.Context) []*domain.VirtualLibrary {
	return append([]*domain.VirtualLibrary(nil), f.libraries...)
}

func (f localMountInventoryFixture) ListDrives(context.Context) []*domain.VirtualDrive {
	return append([]*domain.VirtualDrive(nil), f.drives...)
}

func (f localMountInventoryFixture) ListCartridges(context.Context) []*domain.VirtualCartridge {
	return append([]*domain.VirtualCartridge(nil), f.cartridges...)
}

type localMountPoolFixture struct {
	failed map[string]bool
}

func (f localMountPoolFixture) GetPool(_ context.Context, poolID string) (*domain.StoragePoolRuntime, error) {
	if f.failed[poolID] {
		return nil, errors.New("pool unavailable")
	}
	return &domain.StoragePoolRuntime{PoolID: poolID, Status: domain.PoolActive}, nil
}

func TestBuildLocalMountInventoryIncludesEmptyDevicesAndIgnoresPublications(t *testing.T) {
	ctx := context.Background()
	libEmpty, _ := domain.NewVirtualLibrary("lib-empty", "Empty library")
	libBusy, _ := domain.NewVirtualLibrary("lib-loaded", "Loaded library")
	driveEmpty, _ := domain.NewVirtualDrive("drive-empty", libBusy.LibraryID, 2)
	driveLoaded, _ := domain.NewVirtualDrive("drive-loaded", libBusy.LibraryID, 1)
	if err := driveLoaded.Mount("cart-loaded"); err != nil {
		t.Fatal(err)
	}
	cartridge := domain.NewVirtualCartridge("cart-loaded", "pool-a", libBusy.LibraryID, "TAPE001", 1<<30)
	resources := localMountInventoryFixture{
		libraries:  []*domain.VirtualLibrary{libBusy, libEmpty},
		drives:     []*domain.VirtualDrive{driveEmpty, driveLoaded},
		cartridges: []*domain.VirtualCartridge{cartridge},
	}

	devices, err := BuildLocalMountInventory(ctx, resources, localMountPoolFixture{})
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}
	if len(devices) != 4 {
		t.Fatalf("expected two changers and two drives, got %+v", devices)
	}
	byKey := make(map[string]domain.VTLDeviceDescriptor, len(devices))
	for _, device := range devices {
		byKey[device.DeviceKey] = device
	}
	for _, key := range []string{"changer:lib-empty", "changer:lib-loaded", "drive:drive-empty", "drive:drive-loaded"} {
		if _, ok := byKey[key]; !ok {
			t.Errorf("configured device %q missing from inventory", key)
		}
	}
	if got := byKey["drive:drive-empty"]; !got.BackendReady || got.PoolID != "" || got.LoadedCartridgeID != "" {
		t.Errorf("empty drive should be ready without a pool: %+v", got)
	}
	if got := byKey["drive:drive-loaded"]; !got.BackendReady || got.PoolID != "pool-a" || got.LoadedCartridgeID != "cart-loaded" {
		t.Errorf("loaded drive should use its cartridge pool: %+v", got)
	}
	if got := byKey["changer:lib-empty"]; !got.BackendReady || got.PoolID != "" {
		t.Errorf("empty changer should be ready without media or a pool: %+v", got)
	}
}

func TestBuildLocalMountInventoryRetainsDevicesWhenPoolUnavailable(t *testing.T) {
	ctx := context.Background()
	library, _ := domain.NewVirtualLibrary("lib-a", "Library A")
	drive, _ := domain.NewVirtualDrive("drive-a", library.LibraryID, 1)
	if err := drive.Mount("cart-a"); err != nil {
		t.Fatal(err)
	}
	resources := localMountInventoryFixture{
		libraries:  []*domain.VirtualLibrary{library},
		drives:     []*domain.VirtualDrive{drive},
		cartridges: []*domain.VirtualCartridge{domain.NewVirtualCartridge("cart-a", "pool-missing", library.LibraryID, "TAPE002", 1<<30)},
	}

	devices, err := BuildLocalMountInventory(ctx, resources, localMountPoolFixture{failed: map[string]bool{"pool-missing": true}})
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}
	var loaded *domain.VTLDeviceDescriptor
	for i := range devices {
		if devices[i].DeviceKey == "drive:drive-a" {
			loaded = &devices[i]
		}
	}
	if loaded == nil {
		t.Fatal("loaded drive disappeared from desired inventory")
	}
	if loaded.BackendReady || loaded.ReadinessReason != domain.LocalMountReasonPoolUnavailable {
		t.Fatalf("expected unavailable loaded pool to mark device not ready, got %+v", loaded)
	}
}

func TestLocalMountBackendReferencesAreStableAndUnique(t *testing.T) {
	a := localMountBackendRef("drive:a")
	if a != localMountBackendRef("drive:a") {
		t.Fatal("backend reference changed for stable device key")
	}
	if a == localMountBackendRef("drive-a") {
		t.Fatal("backend references collided after sanitization")
	}
}
