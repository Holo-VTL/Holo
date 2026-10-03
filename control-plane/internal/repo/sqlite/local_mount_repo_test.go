package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestLocalMountEnabledAndMappingsPersistAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	repo := NewLocalMountRepo(db)
	if err := repo.SetEnabled(ctx, true); err != nil {
		t.Fatalf("persist enabled setting: %v", err)
	}
	library := testLocalLoopbackLibraryMapping()
	if err := repo.SaveLibraryMapping(ctx, library); err != nil {
		t.Fatalf("save library mapping: %v", err)
	}
	changer := testLocalLoopbackChangerMapping()
	drive := testLocalLoopbackDriveMapping(1, "drive-a")
	for _, mapping := range []domain.LocalLoopbackDeviceMapping{changer, drive} {
		if err := repo.SaveDeviceMapping(ctx, mapping); err != nil {
			t.Fatalf("save device mapping %q: %v", mapping.DeviceKey, err)
		}
	}
	if err := repo.MarkDeviceCleanupPending(ctx, drive.DeviceKey); err != nil {
		t.Fatalf("mark drive cleanup pending: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first database handle: %v", err)
	}

	db, err = Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen sqlite db: %v", err)
	}
	defer db.Close()
	repo = NewLocalMountRepo(db)
	enabled, err := repo.Enabled(ctx)
	if err != nil || !enabled {
		t.Fatalf("enabled intent did not persist: enabled=%t err=%v", enabled, err)
	}
	libraries, err := repo.ListLibraryMappings(ctx)
	if err != nil || len(libraries) != 1 || libraries[0] != library {
		t.Fatalf("library mapping changed across restart: mappings=%+v err=%v", libraries, err)
	}
	devices, err := repo.ListDeviceMappings(ctx, library.LibraryID)
	if err != nil || len(devices) != 2 {
		t.Fatalf("device mappings changed across restart: mappings=%+v err=%v", devices, err)
	}
	if devices[1].DeviceKey != drive.DeviceKey || devices[1].State != domain.LocalMappingStateCleanupPending {
		t.Fatalf("cleanup record was not retained across restart: %+v", devices)
	}
}

func TestLocalMountRepoRejectsIdentityAndLibraryLUNCollisions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	defer db.Close()
	repo := NewLocalMountRepo(db)
	if err := repo.SaveLibraryMapping(ctx, testLocalLoopbackLibraryMapping()); err != nil {
		t.Fatalf("save first library mapping: %v", err)
	}
	if err := repo.SaveDeviceMapping(ctx, testLocalLoopbackChangerMapping()); err != nil {
		t.Fatalf("save changer mapping: %v", err)
	}
	if err := repo.SaveDeviceMapping(ctx, testLocalLoopbackDriveMapping(1, "drive-a")); err != nil {
		t.Fatalf("save drive mapping: %v", err)
	}
	if err := repo.DeleteLibraryMapping(ctx, "library-a"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected library mapping to be retained while devices are mapped, got %v", err)
	}

	conflictingIdentity := testLocalLoopbackDriveMapping(1, "drive-a")
	conflictingIdentity.IdentityRef = "changer-identity-a"
	if err := repo.SaveDeviceMapping(ctx, conflictingIdentity); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected duplicate identity to fail with ErrConflict, got %v", err)
	}
	conflictingLUN := testLocalLoopbackDriveMapping(1, "drive-b")
	if err := repo.SaveDeviceMapping(ctx, conflictingLUN); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected drive LUN collision to fail with ErrConflict, got %v", err)
	}
	if err := repo.SaveLibraryMapping(ctx, domain.LocalLoopbackLibraryMapping{
		LibraryID: "library-b", TargetNAA: "naa.50014056b18af0f5", NexusNAA: "naa.5001405db2f4505c", TPGTag: 1,
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected target NAA collision to fail with ErrConflict, got %v", err)
	}
	if err := repo.SaveLibraryMapping(ctx, domain.LocalLoopbackLibraryMapping{
		LibraryID: "library-b", TargetNAA: "naa.50014056b18af0f6", NexusNAA: "naa.5001405db2f4505b", TPGTag: 1,
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected nexus NAA collision to fail with ErrConflict, got %v", err)
	}
	changedIdentity := testLocalLoopbackLibraryMapping()
	changedIdentity.TargetNAA = "naa.50014056b18af0f6"
	if err := repo.SaveLibraryMapping(ctx, changedIdentity); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected persistent library identity to be immutable, got %v", err)
	}
}

func testLocalLoopbackLibraryMapping() domain.LocalLoopbackLibraryMapping {
	return domain.LocalLoopbackLibraryMapping{
		LibraryID: "library-a", TargetNAA: "naa.50014056b18af0f5",
		NexusNAA: "naa.5001405db2f4505b", TPGTag: 1,
	}
}

func testLocalLoopbackChangerMapping() domain.LocalLoopbackDeviceMapping {
	return domain.LocalLoopbackDeviceMapping{
		DeviceKey: "changer:library-a", LibraryID: "library-a", Kind: domain.LocalDeviceKindChanger,
		LUNIndex: 0, IdentityRef: "changer-identity-a", BackendRef: "holo_backstore_a",
		State: domain.LocalMappingStateActive,
	}
}

func testLocalLoopbackDriveMapping(lun int, driveID string) domain.LocalLoopbackDeviceMapping {
	return domain.LocalLoopbackDeviceMapping{
		DeviceKey: "drive:" + driveID, LibraryID: "library-a", Kind: domain.LocalDeviceKindDrive,
		DriveID: driveID, LUNIndex: lun, IdentityRef: "drive-identity-" + driveID,
		BackendRef: "holo_backstore_" + driveID, State: domain.LocalMappingStateActive,
	}
}
