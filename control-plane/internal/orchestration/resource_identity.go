package orchestration

import (
	"strings"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

// ValidateResourceIdentity blocks an operation that touches a historical
// resource whose stored IDs now alias the same on-disk or device identity.
func ValidateResourceIdentity(
	libraries []*domain.VirtualLibrary,
	drives []*domain.VirtualDrive,
	cartridges []*domain.VirtualCartridge,
	pools []*domain.StoragePoolRuntime,
	libraryID, driveID, cartridgeID string,
) error {
	var library *domain.VirtualLibrary
	for _, candidate := range libraries {
		if candidate == nil {
			continue
		}
		if candidate.LibraryID == libraryID {
			library = candidate
			continue
		}
	}
	if library == nil {
		return domain.ErrNotFound
	}
	var drive *domain.VirtualDrive
	if driveID != "" {
		for _, candidate := range drives {
			if candidate != nil && candidate.DriveID == driveID {
				drive = candidate
				break
			}
		}
		if drive == nil {
			return domain.ErrNotFound
		}
	}
	var cartridge *domain.VirtualCartridge
	for _, candidate := range cartridges {
		if candidate != nil && candidate.CartridgeID == cartridgeID {
			cartridge = candidate
			break
		}
	}
	if cartridge == nil {
		return domain.ErrNotFound
	}
	if driveID == "" {
		for _, candidate := range drives {
			if candidate == nil || candidate.LibraryID != libraryID {
				continue
			}
			if err := ValidateResourceIdentity(libraries, drives, cartridges, pools, libraryID, candidate.DriveID, cartridgeID); err != nil {
				return err
			}
		}
	}

	libraryComponent := storageutil.LibraryDirectoryProjection(library.LibraryID)
	libraryIQN := normalizedResourceIQN(library.IQN)
	driveIQN := ""
	if drive != nil {
		driveIQN = normalizedResourceIQN(drive.IQN)
	}
	if libraryIQN != "" && libraryIQN == driveIQN {
		return domain.ErrIdentityConflict
	}
	for _, candidate := range libraries {
		if candidate == nil || candidate.LibraryID == libraryID {
			continue
		}
		if storageutil.LibraryDirectoryProjection(candidate.LibraryID) == libraryComponent ||
			deviceIQNAliases(libraryIQN, driveIQN, candidate.IQN) {
			return domain.ErrIdentityConflict
		}
	}
	for _, candidate := range drives {
		if candidate == nil || candidate.DriveID == driveID {
			continue
		}
		if drive != nil && (storageutil.DriveDirectoryProjection(candidate.DriveID) == storageutil.DriveDirectoryProjection(drive.DriveID) ||
			storageutil.MediaStatePathProjection(candidate.LibraryID, candidate.DriveID) == storageutil.MediaStatePathProjection(drive.LibraryID, drive.DriveID) ||
			deviceIQNAliases(libraryIQN, driveIQN, candidate.IQN)) {
			return domain.ErrIdentityConflict
		}
	}
	cartridgeMetadata := storageutil.CartridgeMetadataProjection(cartridge.CartridgeID)
	cartridgeLayout := storageutil.CartridgeLayoutProjection(cartridge.LibraryID, cartridge.CartridgeID)
	for _, candidate := range cartridges {
		if candidate == nil || candidate.CartridgeID == cartridgeID {
			continue
		}
		if storageutil.CartridgeMetadataProjection(candidate.CartridgeID) == cartridgeMetadata ||
			storageutil.CartridgeLayoutProjection(candidate.LibraryID, candidate.CartridgeID) == cartridgeLayout {
			return domain.ErrIdentityConflict
		}
	}
	poolProjection := storageutil.PoolRootProjection(cartridge.PoolID)
	for _, candidate := range pools {
		if candidate != nil && candidate.PoolID != cartridge.PoolID && storageutil.PoolRootProjection(candidate.PoolID) == poolProjection {
			return domain.ErrIdentityConflict
		}
	}
	return nil
}

func deviceIQNAliases(libraryIQN, driveIQN, candidateIQN string) bool {
	candidateIQN = normalizedResourceIQN(candidateIQN)
	return candidateIQN != "" && (candidateIQN == libraryIQN || candidateIQN == driveIQN)
}

func normalizedResourceIQN(iqn string) string {
	return strings.ToLower(strings.TrimSpace(iqn))
}
