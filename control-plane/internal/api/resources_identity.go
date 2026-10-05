package api

import (
	"context"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/orchestration"
)

func (h *ResourcesHandler) validateCartridgeIdentity(ctx context.Context, cartridge *domain.VirtualCartridge, driveIDs ...string) error {
	if cartridge == nil {
		return domain.ErrInvalidInput
	}
	var pools []*domain.StoragePoolRuntime
	if h.storage != nil {
		pools = h.storage.ListPools(ctx)
	}
	driveID := ""
	if len(driveIDs) > 0 {
		driveID = driveIDs[0]
	}
	return orchestration.ValidateResourceIdentity(
		h.repo.ListLibraries(ctx), h.repo.ListDrives(ctx), h.repo.ListCartridges(ctx), pools,
		cartridge.LibraryID, driveID, cartridge.CartridgeID,
	)
}
