package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

func (h *ResourcesHandler) deleteLibraryCascade(ctx context.Context, libraryID string) error {
	libraryID = strings.TrimSpace(libraryID)
	if validateManagementID(libraryID) != nil {
		return domain.ErrInvalidInput
	}
	if _, err := h.repo.FindLibrary(ctx, libraryID); err != nil {
		return err
	}
	unlock := h.lockLibrarySlots(libraryID)
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	if _, err := h.repo.FindLibrary(ctx, libraryID); err != nil {
		return err
	}
	if err := h.reconcileMediaState(ctx); err != nil {
		return err
	}
	if err := h.unpublishDependentPublications(ctx, libraryID, "", ""); err != nil {
		return err
	}
	drives := h.repo.ListDrives(ctx)
	cartridges := h.repo.ListCartridges(ctx)
	deletedPoolIDs := make([]string, 0)

	for _, drive := range drives {
		if drive != nil && drive.LibraryID == libraryID && drive.MountedCartridgeID != "" {
			return domain.ErrInvalidState
		}
	}

	for _, cartridge := range cartridges {
		if cartridge == nil || cartridge.LibraryID != libraryID {
			continue
		}
		if err := h.validateCartridgeIdentity(ctx, cartridge); err != nil {
			return err
		}
		if poolID := strings.TrimSpace(cartridge.PoolID); poolID != "" {
			deletedPoolIDs = append(deletedPoolIDs, poolID)
		}
		if err := func() error {
			storageLease, err := acquireCartridgeMutationLeases(cartridge)
			if err != nil {
				return err
			}
			defer storageLease.Release()
			if err := storageLease.VerifyPoolRoot(); err != nil {
				return err
			}
			return removeCartridgeLayoutArtifacts(cartridge)
		}(); err != nil {
			return err
		}
		if err := h.repo.DeleteCartridge(ctx, cartridge.CartridgeID); err != nil {
			return err
		}
	}

	for _, drive := range drives {
		if drive == nil || drive.LibraryID != libraryID {
			continue
		}
		if drive.MountedCartridgeID != "" {
			return domain.ErrInvalidState
		}
		if err := h.repo.DeleteDrive(ctx, drive.DriveID); err != nil {
			return err
		}
		if err := removeDriveMediaStateFile(drive.LibraryID, drive.DriveID); err != nil {
			return err
		}
	}
	if err := h.syncPoolUsage(ctx, deletedPoolIDs...); err != nil {
		return err
	}

	if err := h.repo.DeleteLibrary(ctx, libraryID); err != nil {
		return err
	}
	h.syncLocalMount("web-console")
	unlock()
	locked = false
	h.slotLocksMu.Lock()
	delete(h.slotLocks, libraryID)
	h.slotLocksMu.Unlock()
	return nil
}

func removeDriveMediaStateFile(libraryID, driveID string) error {
	stateKey := storageutil.MediaStateKey(strings.TrimSpace(libraryID), strings.TrimSpace(driveID))
	base := filepath.Join(mediaStateDir(), sanitizeStateID(stateKey))
	for _, suffix := range []string{".slots", ".state", ".ie"} {
		if err := os.Remove(base + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (h *ResourcesHandler) unpublishDependentPublications(ctx context.Context, libraryID, driveID, cartridgeID string) error {
	if h.target == nil {
		return nil
	}
	libraryID = strings.TrimSpace(libraryID)
	driveID = strings.TrimSpace(driveID)
	cartridgeID = strings.TrimSpace(cartridgeID)
	for _, publication := range h.target.ListPublications(ctx) {
		if publication == nil {
			continue
		}
		match := false
		if libraryID != "" && publication.LibraryID == libraryID {
			match = true
		}
		if driveID != "" && publication.DriveID == driveID {
			match = true
		}
		if cartridgeID != "" && publication.CartridgeID == cartridgeID {
			match = true
		}
		if !match {
			continue
		}
		if _, err := h.target.Unpublish(ctx, publication.PublicationID, "system"); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	return nil
}
