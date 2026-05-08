package orchestration

import (
	"context"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/auth"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type accessServices struct {
	runtime *TargetRuntimeService
	access  *TargetAccessService
}

func seededAccessServices(t *testing.T) accessServices {
	t.Helper()
	coreRepo := memory.NewCoreResourcesRepo()
	runtimeRepo := memory.NewTargetRuntimeRepo()
	accessRepo := memory.NewTargetAccessRepo()
	auditWriter := audit.NewMemoryWriter()

	lib, err := domain.NewVirtualLibrary("lib-1", "lib-1")
	if err != nil {
		t.Fatalf("new library failed: %v", err)
	}
	drive, err := domain.NewVirtualDrive("drive-1", "lib-1", 1)
	if err != nil {
		t.Fatalf("new drive failed: %v", err)
	}
	car := domain.NewVirtualCartridge("car-1", "pool-1", "lib-1", "B001", 1<<30)

	ctx := context.Background()
	_ = coreRepo.SaveLibrary(ctx, lib)
	_ = coreRepo.SaveDrive(ctx, drive)
	_ = coreRepo.SaveCartridge(ctx, car)

	runtimeSvc := NewTargetRuntimeService(coreRepo, runtimeRepo, auditWriter, nil)
	accessSvc := NewTargetAccessService(runtimeRepo, accessRepo, auth.NewAccessEvaluator(), auditWriter)
	return accessServices{runtime: runtimeSvc, access: accessSvc}
}

func publishForAccess(t *testing.T, runtime *TargetRuntimeService, suffix string) *domain.TargetPublication {
	t.Helper()
	pub, err := runtime.Publish(context.Background(), PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:" + suffix,
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	return pub
}

func TestTargetAccessReplaceAuthorizeAndVisibility(t *testing.T) {
	svcs := seededAccessServices(t)
	ctx := context.Background()

	pubA := publishForAccess(t, svcs.runtime, "acl-a")
	pubB := publishForAccess(t, svcs.runtime, "acl-b")

	_, err := svcs.access.ReplaceRules(ctx, pubA.PublicationID, "ops", []domain.InitiatorRule{
		{Initiator: "iqn.1993-08.org.debian:01:init-a", Permission: domain.PermissionAllow, Priority: 100},
		{Initiator: "iqn.1993-08.org.debian:01:init-b", Permission: domain.PermissionDeny, Priority: 100},
	})
	if err != nil {
		t.Fatalf("replace rules for pubA failed: %v", err)
	}

	_, err = svcs.access.ReplaceRules(ctx, pubB.PublicationID, "ops", []domain.InitiatorRule{
		{Initiator: "iqn.1993-08.org.debian:01:init-b", Permission: domain.PermissionAllow, Priority: 100},
	})
	if err != nil {
		t.Fatalf("replace rules for pubB failed: %v", err)
	}

	allowDecision, err := svcs.access.Authorize(ctx, pubA.PublicationID, "iqn.1993-08.org.debian:01:init-a", "ops")
	if err != nil {
		t.Fatalf("authorize allow failed: %v", err)
	}
	if allowDecision.Decision != domain.PermissionAllow {
		t.Fatalf("expected allow decision, got %s", allowDecision.Decision)
	}

	denyDecision, err := svcs.access.Authorize(ctx, pubA.PublicationID, "iqn.1993-08.org.debian:01:init-b", "ops")
	if err != nil {
		t.Fatalf("authorize deny failed: %v", err)
	}
	if denyDecision.Decision != domain.PermissionDeny {
		t.Fatalf("expected deny decision, got %s", denyDecision.Decision)
	}

	defaultDecision, err := svcs.access.Authorize(ctx, pubA.PublicationID, "iqn.1993-08.org.debian:01:init-c", "ops")
	if err != nil {
		t.Fatalf("authorize default deny failed: %v", err)
	}
	if defaultDecision.Decision != domain.PermissionDeny || defaultDecision.Reason != "default_deny_no_match" {
		t.Fatalf("expected default deny decision, got decision=%s reason=%s", defaultDecision.Decision, defaultDecision.Reason)
	}

	visibleA, err := svcs.access.ListVisiblePublications(ctx, "iqn.1993-08.org.debian:01:init-a", "ops")
	if err != nil {
		t.Fatalf("visibility query for init-a failed: %v", err)
	}
	if len(visibleA) != 1 || visibleA[0].PublicationID != pubA.PublicationID {
		t.Fatalf("unexpected visible set for init-a: %+v", visibleA)
	}

	visibleB, err := svcs.access.ListVisiblePublications(ctx, "iqn.1993-08.org.debian:01:init-b", "ops")
	if err != nil {
		t.Fatalf("visibility query for init-b failed: %v", err)
	}
	if len(visibleB) != 1 || visibleB[0].PublicationID != pubB.PublicationID {
		t.Fatalf("unexpected visible set for init-b: %+v", visibleB)
	}
}

func TestTargetAccessRollbackRestoresPreviousDecision(t *testing.T) {
	svcs := seededAccessServices(t)
	ctx := context.Background()
	pub := publishForAccess(t, svcs.runtime, "acl-rollback")

	_, err := svcs.access.ReplaceRules(ctx, pub.PublicationID, "ops", []domain.InitiatorRule{{
		Initiator:  "iqn.1993-08.org.debian:01:init-a",
		Permission: domain.PermissionAllow,
		Priority:   100,
	}})
	if err != nil {
		t.Fatalf("initial replace failed: %v", err)
	}

	_, err = svcs.access.ReplaceRules(ctx, pub.PublicationID, "ops", []domain.InitiatorRule{{
		Initiator:  "iqn.1993-08.org.debian:01:init-a",
		Permission: domain.PermissionDeny,
		Priority:   100,
	}})
	if err != nil {
		t.Fatalf("second replace failed: %v", err)
	}

	decision, err := svcs.access.Authorize(ctx, pub.PublicationID, "iqn.1993-08.org.debian:01:init-a", "ops")
	if err != nil {
		t.Fatalf("authorize before rollback failed: %v", err)
	}
	if decision.Decision != domain.PermissionDeny {
		t.Fatalf("expected deny before rollback, got %s", decision.Decision)
	}

	snapshot, noop, err := svcs.access.RollbackRules(ctx, pub.PublicationID, "ops")
	if err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	if noop {
		t.Fatal("expected non-noop rollback")
	}
	if snapshot.Version != 3 {
		t.Fatalf("expected rollback snapshot version 3, got %d", snapshot.Version)
	}

	decision, err = svcs.access.Authorize(ctx, pub.PublicationID, "iqn.1993-08.org.debian:01:init-a", "ops")
	if err != nil {
		t.Fatalf("authorize after rollback failed: %v", err)
	}
	if decision.Decision != domain.PermissionAllow {
		t.Fatalf("expected allow after rollback, got %s", decision.Decision)
	}
}

func TestTargetAccessAuthorizeDisabledPublication(t *testing.T) {
	svcs := seededAccessServices(t)
	ctx := context.Background()
	pub := publishForAccess(t, svcs.runtime, "acl-disabled")

	_, err := svcs.runtime.Unpublish(ctx, pub.PublicationID, "ops")
	if err != nil {
		t.Fatalf("unpublish failed: %v", err)
	}

	decision, err := svcs.access.Authorize(ctx, pub.PublicationID, "iqn.1993-08.org.debian:01:init-a", "ops")
	if err != nil {
		t.Fatalf("authorize disabled publication failed: %v", err)
	}
	if decision.Decision != domain.PermissionDeny || decision.Reason != "publication_not_ready" {
		t.Fatalf("unexpected decision for disabled publication: decision=%s reason=%s", decision.Decision, decision.Reason)
	}
}
