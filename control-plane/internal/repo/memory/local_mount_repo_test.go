package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestLocalMountRepoKeepsMappingsStableAndCleanupPending(t *testing.T) {
	ctx := context.Background()
	repo := NewLocalMountRepo()
	if err := repo.SetEnabled(ctx, true); err != nil {
		t.Fatalf("set local mount enabled: %v", err)
	}
	if err := repo.SaveLibraryMapping(ctx, memoryLocalMountLibrary()); err != nil {
		t.Fatalf("save library mapping: %v", err)
	}
	device := memoryLocalMountDevice("drive:drive-a", "drive-a", 1)
	if err := repo.SaveDeviceMapping(ctx, device); err != nil {
		t.Fatalf("save device mapping: %v", err)
	}
	if err := repo.MarkDeviceCleanupPending(ctx, device.DeviceKey); err != nil {
		t.Fatalf("mark cleanup pending: %v", err)
	}
	if err := repo.SaveDeviceMapping(ctx, device); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("cleanup-pending mapping must not silently return to active: %v", err)
	}
	devices, err := repo.ListDeviceMappings(ctx, "library-a")
	if err != nil || len(devices) != 1 || devices[0].State != domain.LocalMappingStateCleanupPending {
		t.Fatalf("cleanup mapping was not retained: devices=%+v err=%v", devices, err)
	}
	if err := repo.DeleteLibraryMapping(ctx, "library-a"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("library mapping must remain while a device mapping exists: %v", err)
	}
}

func TestLocalMountRepoRejectsConflictingNAAAndLUN(t *testing.T) {
	ctx := context.Background()
	repo := NewLocalMountRepo()
	if err := repo.SaveLibraryMapping(ctx, memoryLocalMountLibrary()); err != nil {
		t.Fatalf("save first library mapping: %v", err)
	}
	if err := repo.SaveLibraryMapping(ctx, domain.LocalLoopbackLibraryMapping{
		LibraryID: "library-b", TargetNAA: "naa.50014056b18af0f6", NexusNAA: "naa.5001405db2f4505b", TPGTag: 1,
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected duplicate nexus NAA to be rejected, got %v", err)
	}
	if err := repo.SaveDeviceMapping(ctx, memoryLocalMountDevice("drive:drive-a", "drive-a", 1)); err != nil {
		t.Fatalf("save first device: %v", err)
	}
	if err := repo.SaveDeviceMapping(ctx, memoryLocalMountDevice("drive:drive-b", "drive-b", 1)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected duplicate library LUN to be rejected, got %v", err)
	}
}

func memoryLocalMountLibrary() domain.LocalLoopbackLibraryMapping {
	return domain.LocalLoopbackLibraryMapping{
		LibraryID: "library-a", TargetNAA: "naa.50014056b18af0f5",
		NexusNAA: "naa.5001405db2f4505b", TPGTag: 1,
	}
}

func memoryLocalMountDevice(key, driveID string, lun int) domain.LocalLoopbackDeviceMapping {
	return domain.LocalLoopbackDeviceMapping{
		DeviceKey: key, LibraryID: "library-a", Kind: domain.LocalDeviceKindDrive,
		DriveID: driveID, LUNIndex: lun, IdentityRef: "identity-" + driveID,
		BackendRef: "holo_backstore_" + driveID, State: domain.LocalMappingStateActive,
	}
}
