package memory

import (
	"context"
	"fmt"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func TestStoragePoolRepo_CreateAndList(t *testing.T) {
	repo := NewStoragePoolRepo()
	ctx := context.Background()

	pool, err := domain.NewStoragePoolRuntime("pool-a", "Pool A", 90)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}
	if err := repo.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}

	list := repo.ListPools(ctx)
	if len(list) != 1 || list[0].PoolID != "pool-a" {
		t.Fatalf("unexpected list result: %+v", list)
	}

	list[0].Name = "mutated"
	reloaded, err := repo.FindPool(ctx, "pool-a")
	if err != nil {
		t.Fatalf("find pool failed: %v", err)
	}
	if reloaded.Name != "Pool A" {
		t.Fatalf("expected cloned pool value, got %q", reloaded.Name)
	}
}

func TestStoragePoolRepo_AttachConflictAndOwner(t *testing.T) {
	repo := NewStoragePoolRepo()
	ctx := context.Background()

	poolA, _ := domain.NewStoragePoolRuntime("pool-a", "Pool A", 90)
	poolB, _ := domain.NewStoragePoolRuntime("pool-b", "Pool B", 90)
	if err := repo.CreatePool(ctx, poolA); err != nil {
		t.Fatalf("create pool a failed: %v", err)
	}
	if err := repo.CreatePool(ctx, poolB); err != nil {
		t.Fatalf("create pool b failed: %v", err)
	}

	if _, err := repo.AttachDisk(ctx, "pool-a", domain.StoragePoolDisk{DevicePath: "/dev/sdb", SizeBytes: 1024}); err != nil {
		t.Fatalf("attach disk to pool a failed: %v", err)
	}
	if _, err := repo.AttachDisk(ctx, "pool-b", domain.StoragePoolDisk{DevicePath: "/dev/sdb", SizeBytes: 1024}); err != domain.ErrConflict {
		t.Fatalf("expected conflict on cross-pool attach, got %v", err)
	}

	owner, ok := repo.DiskOwner(ctx, "/dev/sdb")
	if !ok || owner != "pool-a" {
		t.Fatalf("expected disk owner pool-a, got owner=%q ok=%v", owner, ok)
	}
}

func TestStoragePoolRepo_ReserveAndRollback(t *testing.T) {
	repo := NewStoragePoolRepo()
	ctx := context.Background()

	pool, _ := domain.NewStoragePoolRuntime("pool-a", "Pool A", 80)
	if err := repo.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	if _, err := repo.AttachDisk(ctx, "pool-a", domain.StoragePoolDisk{DevicePath: "/dev/sdb", SizeBytes: 100}); err != nil {
		t.Fatalf("attach disk failed: %v", err)
	}

	updated, warning, err := repo.ReserveWrite(ctx, "pool-a", 90)
	if err != nil {
		t.Fatalf("reserve write failed: %v", err)
	}
	if !warning {
		t.Fatalf("expected warning trigger at 90%% with threshold 80%%")
	}
	if updated.Capacity.UsedBytes != 90 || updated.Capacity.FreeBytes != 10 {
		t.Fatalf("unexpected capacity after reserve: %+v", updated.Capacity)
	}

	if _, _, err := repo.ReserveWrite(ctx, "pool-a", 11); err != domain.ErrCapacityExceeded {
		t.Fatalf("expected capacity exceeded, got %v", err)
	}

	rolledBack, err := repo.RollbackReservedWrite(ctx, "pool-a", 90)
	if err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	if rolledBack.Capacity.UsedBytes != 0 || rolledBack.Capacity.FreeBytes != 100 {
		t.Fatalf("unexpected capacity after rollback: %+v", rolledBack.Capacity)
	}
}

func TestStoragePoolRepo_SetUsedBytesReconcilesSnapshot(t *testing.T) {
	repo := NewStoragePoolRepo()
	ctx := context.Background()

	pool, _ := domain.NewStoragePoolRuntime("pool-a", "Pool A", 80)
	if err := repo.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool failed: %v", err)
	}
	if _, err := repo.AttachDisk(ctx, "pool-a", domain.StoragePoolDisk{DevicePath: "/dev/sdb", SizeBytes: 100}); err != nil {
		t.Fatalf("attach disk failed: %v", err)
	}

	updated, err := repo.SetUsedBytes(ctx, "pool-a", 25)
	if err != nil {
		t.Fatalf("set used bytes failed: %v", err)
	}
	if updated.Capacity.UsedBytes != 25 || updated.Capacity.FreeBytes != 75 || updated.Capacity.UsedPercent != 25 {
		t.Fatalf("unexpected reconciled capacity: %+v", updated.Capacity)
	}
}

func TestStoragePoolRepo_DiscoveryHistoryCap(t *testing.T) {
	repo := NewStoragePoolRepo()
	ctx := context.Background()

	for i := 0; i < maxDiskDiscoveryHistories+10; i++ {
		repo.RecordDiscovery(ctx, []domain.StorageManagedDisk{{DevicePath: fmt.Sprintf("/dev/sd%c", 'a'+(i%26)), SizeBytes: 1}})
	}
	if len(repo.discoveryHistory) != maxDiskDiscoveryHistories {
		t.Fatalf("expected discovery history cap %d, got %d", maxDiskDiscoveryHistories, len(repo.discoveryHistory))
	}
}
