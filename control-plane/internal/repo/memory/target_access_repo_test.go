package memory

import (
	"context"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestTargetAccessRepoCapsSnapshotHistory(t *testing.T) {
	repo := NewTargetAccessRepo()
	ctx := context.Background()
	publicationID := "pub-cap"

	totalWrites := maxSnapshotHistory + 15
	for i := 0; i < totalWrites; i++ {
		_, err := repo.ReplaceRules(ctx, publicationID, "tester", []domain.InitiatorRule{
			{
				Initiator:  "iqn.2026-04.ai.holo:init-a",
				Permission: domain.PermissionAllow,
				Priority:   i + 1,
			},
		})
		if err != nil {
			t.Fatalf("replace rules failed at iteration %d: %v", i, err)
		}
	}

	if count := repo.SnapshotCount(); count != maxSnapshotHistory {
		t.Fatalf("expected capped snapshot count %d, got %d", maxSnapshotHistory, count)
	}

	current, err := repo.CurrentSnapshot(ctx, publicationID)
	if err != nil {
		t.Fatalf("current snapshot failed: %v", err)
	}
	if current.Version != totalWrites {
		t.Fatalf("expected latest version %d, got %d", totalWrites, current.Version)
	}
}
