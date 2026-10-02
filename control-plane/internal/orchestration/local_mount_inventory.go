package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

type LocalMountInventoryRepository interface {
	ListLibraries(context.Context) []*domain.VirtualLibrary
	ListDrives(context.Context) []*domain.VirtualDrive
	ListCartridges(context.Context) []*domain.VirtualCartridge
}

type LocalMountPoolReader interface {
	GetPool(context.Context, string) (*domain.StoragePoolRuntime, error)
}

// BuildLocalMountInventory describes every configured changer and drive.
// Publication and CHAP state are intentionally not eligibility filters.
func BuildLocalMountInventory(ctx context.Context, resources LocalMountInventoryRepository, pools LocalMountPoolReader) ([]domain.VTLDeviceDescriptor, error) {
	if resources == nil {
		return nil, domain.ErrInvalidInput
	}

	libraries := resources.ListLibraries(ctx)
	drives := resources.ListDrives(ctx)
	cartridges := resources.ListCartridges(ctx)
	sort.Slice(libraries, func(i, j int) bool { return libraryID(libraries[i]) < libraryID(libraries[j]) })
	sort.Slice(drives, func(i, j int) bool {
		if drives[i] == nil || drives[j] == nil {
			return drives[i] != nil
		}
		if drives[i].LibraryID == drives[j].LibraryID && drives[i].Slot != drives[j].Slot {
			return drives[i].Slot < drives[j].Slot
		}
		return drives[i].DriveID < drives[j].DriveID
	})

	cartridgeByID := make(map[string]*domain.VirtualCartridge, len(cartridges))
	for _, cartridge := range cartridges {
		if cartridge != nil {
			cartridgeByID[cartridge.CartridgeID] = cartridge
		}
	}

	result := make([]domain.VTLDeviceDescriptor, 0, len(libraries)+len(drives))
	for _, library := range libraries {
		if library == nil || domain.ValidateManagementID(library.LibraryID) != nil {
			continue
		}
		libraryDrives := make([]*domain.VirtualDrive, 0)
		libraryDriveIDs := make([]string, 0)
		for _, drive := range drives {
			if drive != nil && drive.LibraryID == library.LibraryID {
				libraryDrives = append(libraryDrives, drive)
				libraryDriveIDs = append(libraryDriveIDs, drive.DriveID)
			}
		}
		changer := domain.VTLDeviceDescriptor{
			DeviceKey:          domain.ChangerDeviceKey(library.LibraryID),
			Kind:               domain.LocalDeviceKindChanger,
			LibraryID:          library.LibraryID,
			DisplayName:        library.Name,
			DriveIDs:           append([]string(nil), libraryDriveIDs...),
			Profile:            profileOrDefault(library.LibraryType, "changer"),
			DriveProfile:       profileOrDefault(library.DriveType, "drive"),
			CompressionEnabled: library.CompressionEnabled,
			DedupEnabled:       library.DedupEnabled,
			IdentityRef:        identityReference(domain.ChangerDeviceKey(library.LibraryID)),
			BackendRef:         localMountBackendRef(domain.ChangerDeviceKey(library.LibraryID)),
			BackendReady:       library.Status == domain.LibraryReady,
		}
		if !changer.BackendReady {
			changer.ReadinessReason = domain.LocalMountReasonBackendNotReady
		}
		result = append(result, changer)

		for _, drive := range libraryDrives {
			deviceKey := domain.DriveDeviceKey(drive.DriveID)
			descriptor := domain.VTLDeviceDescriptor{
				DeviceKey:          deviceKey,
				Kind:               domain.LocalDeviceKindDrive,
				LibraryID:          library.LibraryID,
				DisplayName:        fmt.Sprintf("%s / %s", library.Name, drive.DriveID),
				DriveID:            drive.DriveID,
				Profile:            profileOrDefault(library.DriveType, "drive"),
				CompressionEnabled: library.CompressionEnabled,
				DedupEnabled:       library.DedupEnabled,
				IdentityRef:        identityReference(deviceKey),
				BackendRef:         localMountBackendRef(deviceKey),
				BackendReady:       library.Status == domain.LibraryReady,
			}
			if drive.MountState == domain.MountError {
				descriptor.BackendReady = false
				descriptor.ReadinessReason = domain.LocalMountReasonBackendNotReady
			}
			if drive.MountedCartridgeID != "" {
				descriptor.LoadedCartridgeID = drive.MountedCartridgeID
				cartridge := cartridgeByID[drive.MountedCartridgeID]
				if cartridge == nil || cartridge.LibraryID != library.LibraryID || strings.TrimSpace(cartridge.PoolID) == "" {
					descriptor.BackendReady = false
					descriptor.ReadinessReason = domain.LocalMountReasonPoolUnavailable
				} else {
					descriptor.PoolID = cartridge.PoolID
					if pools != nil {
						if _, err := pools.GetPool(ctx, cartridge.PoolID); err != nil {
							descriptor.BackendReady = false
							descriptor.ReadinessReason = domain.LocalMountReasonPoolUnavailable
						}
					}
				}
			}
			if !descriptor.BackendReady && descriptor.ReadinessReason == "" {
				descriptor.ReadinessReason = domain.LocalMountReasonBackendNotReady
			}
			result = append(result, descriptor)
		}
	}
	return result, nil
}

func localMountBackendRef(deviceKey string) string {
	base := strings.ToLower(storageutil.SanitizeLayoutID(deviceKey))
	if base == "" {
		base = "device"
	}
	if len(base) > 48 {
		base = base[:48]
	}
	hash := sha256.Sum256([]byte(deviceKey))
	return fmt.Sprintf("holo_local_%s_%s", base, hex.EncodeToString(hash[:4]))
}

func identityReference(deviceKey string) string {
	return "identity_" + localMountBackendRef(deviceKey)[len("holo_local_"):]
}

func profileOrDefault(value, fallback string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return fallback
	}
	return strings.ToLower(strings.Join(fields, "-"))
}

func libraryID(library *domain.VirtualLibrary) string {
	if library == nil {
		return ""
	}
	return library.LibraryID
}
