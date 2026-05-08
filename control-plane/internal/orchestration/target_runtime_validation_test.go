package orchestration

import (
	"context"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestValidationRunOnReadyPublication(t *testing.T) {
	svc := seededRuntimeService(t)
	ctx := context.Background()

	pub, err := svc.Publish(ctx, PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:validation-drive",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	run, err := svc.StartValidationRun(ctx, pub.PublicationID, "ops")
	if err != nil {
		t.Fatalf("start validation failed: %v", err)
	}
	if run.Status != domain.ValidationPassed {
		t.Fatalf("expected passed validation, got %s", run.Status)
	}
	if run.Mode != domain.ValidationModeFixed {
		t.Fatalf("expected fixed mode, got %s", run.Mode)
	}
	if run.BytesWritten != run.BytesRead {
		t.Fatalf("expected bytes parity, got write=%d read=%d", run.BytesWritten, run.BytesRead)
	}
	if run.BytesWritten == 0 {
		t.Fatal("expected fixed mode to write non-zero bytes")
	}
	if run.WriteDigest == "" || run.ReadDigest == "" || run.WriteDigest != run.ReadDigest {
		t.Fatalf("expected matching non-empty digests, got write=%s read=%s", run.WriteDigest, run.ReadDigest)
	}

	runs, err := svc.ListValidationRuns(ctx, pub.PublicationID)
	if err != nil {
		t.Fatalf("list validation runs failed: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected one validation run, got %d", len(runs))
	}
}

func TestValidationRunEmptyMode(t *testing.T) {
	svc := seededRuntimeService(t)
	ctx := context.Background()

	pub, err := svc.Publish(ctx, PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:validation-empty-drive",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	run, err := svc.StartValidationRunWithRequest(ctx, pub.PublicationID, "ops", ValidationRunRequest{Mode: domain.ValidationModeEmpty})
	if err != nil {
		t.Fatalf("start empty-mode validation failed: %v", err)
	}
	if run.Status != domain.ValidationPassed {
		t.Fatalf("expected passed empty validation, got %s", run.Status)
	}
	if run.Mode != domain.ValidationModeEmpty {
		t.Fatalf("expected empty mode, got %s", run.Mode)
	}
	if run.BytesWritten != 0 || run.BytesRead != 0 {
		t.Fatalf("expected empty mode 0 bytes, got write=%d read=%d", run.BytesWritten, run.BytesRead)
	}
	if run.WriteDigest == "" || run.WriteDigest != run.ReadDigest {
		t.Fatalf("expected matching digests for empty mode, got write=%s read=%s", run.WriteDigest, run.ReadDigest)
	}
}

func TestValidationRunRejectsInvalidModeAndState(t *testing.T) {
	svc := seededRuntimeService(t)
	ctx := context.Background()

	pub, err := svc.Publish(ctx, PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:validation-invalid-drive",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	if _, err := svc.StartValidationRunWithRequest(ctx, pub.PublicationID, "ops", ValidationRunRequest{Mode: "bad-mode"}); err != domain.ErrInvalidInput {
		t.Fatalf("expected invalid input for bad mode, got %v", err)
	}

	_, err = svc.Unpublish(ctx, pub.PublicationID, "ops")
	if err != nil {
		t.Fatalf("unpublish failed: %v", err)
	}
	if _, err := svc.StartValidationRunWithRequest(ctx, pub.PublicationID, "ops", ValidationRunRequest{Mode: domain.ValidationModeFixed}); err != domain.ErrInvalidState {
		t.Fatalf("expected invalid state for disabled publication, got %v", err)
	}
}
