package orchestration

import (
	"context"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestRollbackDisablesPublication(t *testing.T) {
	svc := seededRuntimeService(t)
	ctx := context.Background()

	pub, err := svc.Publish(ctx, PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:rollback-drive",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	rolled, err := svc.Rollback(ctx, pub.PublicationID, "ops")
	if err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	if rolled.State != domain.PublicationDisabled {
		t.Fatalf("expected disabled state after rollback, got %s", rolled.State)
	}

	rolled, err = svc.Rollback(ctx, pub.PublicationID, "ops")
	if err != nil {
		t.Fatalf("rollback noop failed: %v", err)
	}
	if rolled.State != domain.PublicationDisabled {
		t.Fatalf("expected disabled state after rollback noop, got %s", rolled.State)
	}
}
