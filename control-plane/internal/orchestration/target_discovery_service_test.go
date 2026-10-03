package orchestration

import (
	"context"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/audit"
	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/repo/memory"
)

type discoveryServices struct {
	runtime   *TargetRuntimeService
	discovery *TargetDiscoveryService
}

func seededDiscoveryServices(t *testing.T) discoveryServices {
	t.Helper()
	coreRepo := memory.NewCoreResourcesRepo()
	runtimeRepo := memory.NewTargetRuntimeRepo()
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
	discoverySvc := NewTargetDiscoveryService(runtimeRepo, auditWriter)
	return discoveryServices{runtime: runtimeSvc, discovery: discoverySvc}
}

func TestTargetDiscoveryFiltersByStateAndPortal(t *testing.T) {
	svcs := seededDiscoveryServices(t)
	ctx := context.Background()

	pubA, err := svcs.runtime.Publish(ctx, PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:discover-a",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish A failed: %v", err)
	}
	pubB, err := svcs.runtime.Publish(ctx, PublishRequest{
		LibraryID:   "lib-1",
		DriveID:     "drive-1",
		CartridgeID: "car-1",
		TargetIQN:   "iqn.2026-04.ai.holo:discover-b",
		Actor:       "ops",
	})
	if err != nil {
		t.Fatalf("publish B failed: %v", err)
	}

	results, err := svcs.discovery.Discover(ctx, domain.TargetDiscoveryRequest{Initiator: "iqn.1993-08.org.debian:01:init-a", Actor: "ops"})
	if err != nil {
		t.Fatalf("discover failed: %v", err)
	}
	found := make(map[string]bool, len(results))
	for _, result := range results {
		found[result.PublicationID] = true
	}
	if len(results) != 2 || !found[pubA.PublicationID] || !found[pubB.PublicationID] {
		t.Fatalf("expected both unrestricted publications to be discoverable, got %+v", results)
	}

	portalFiltered, err := svcs.discovery.Discover(ctx, domain.TargetDiscoveryRequest{Initiator: "iqn.1993-08.org.debian:01:init-a", Portal: "10.10.10.10:3260", Actor: "ops"})
	if err != nil {
		t.Fatalf("discover with portal filter failed: %v", err)
	}
	if len(portalFiltered) != 0 {
		t.Fatalf("expected portal-filtered result to be empty, got %+v", portalFiltered)
	}

	_, err = svcs.runtime.Unpublish(ctx, pubA.PublicationID, "ops")
	if err != nil {
		t.Fatalf("unpublish A failed: %v", err)
	}
	resultsAfter, err := svcs.discovery.Discover(ctx, domain.TargetDiscoveryRequest{Initiator: "iqn.1993-08.org.debian:01:init-a", Actor: "ops"})
	if err != nil {
		t.Fatalf("discover after unpublish failed: %v", err)
	}
	if len(resultsAfter) != 1 || resultsAfter[0].PublicationID != pubB.PublicationID {
		t.Fatalf("expected only pubB after unpublishing pubA, got %+v", resultsAfter)
	}

	snapshot := svcs.discovery.DiscoverySnapshot()
	if snapshot.TotalQueries < 3 {
		t.Fatalf("expected discovery queries to be tracked, got %+v", snapshot)
	}
}

func TestTargetDiscoveryRejectsInvalidRequest(t *testing.T) {
	svcs := seededDiscoveryServices(t)
	_, err := svcs.discovery.Discover(context.Background(), domain.TargetDiscoveryRequest{Initiator: " "})
	if err != domain.ErrInvalidInput {
		t.Fatalf("expected invalid input error, got %v", err)
	}
}
