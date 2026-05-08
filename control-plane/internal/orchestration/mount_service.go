package orchestration

import (
	"context"
	"log"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type MountService struct {
	repo   *memory.CoreResourcesRepo
	auditW audit.Writer
}

func NewMountService(repo *memory.CoreResourcesRepo, auditW audit.Writer) *MountService {
	return &MountService{repo: repo, auditW: auditW}
}

func (s *MountService) MountCartridge(ctx context.Context, driveID, cartridgeID, actor string) error {
	drive, err := s.repo.FindDrive(ctx, driveID)
	if err != nil {
		return err
	}
	cart, err := s.repo.FindCartridge(ctx, cartridgeID)
	if err != nil {
		return err
	}
	if err := drive.Mount(cart.CartridgeID); err != nil {
		return err
	}
	if err := cart.TransitionTo(domain.CartridgeMounted); err != nil {
		return err
	}
	if err := s.auditW.Write(ctx, audit.Event{
		Actor:      actor,
		Action:     "mount",
		ObjectType: "cartridge",
		ObjectID:   cartridgeID,
		Result:     "success",
	}); err != nil {
		log.Printf("AUDIT WRITE FAILURE: %v (event: %s/%s)", err, "mount", cartridgeID)
	}
	return nil
}
