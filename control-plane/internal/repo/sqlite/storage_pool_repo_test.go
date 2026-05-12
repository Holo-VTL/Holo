package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

func openStorageRepo(t *testing.T, path string) *StoragePoolRepo {
	t.Helper()
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStoragePoolRepo(db)
}

func TestStoragePoolRepoPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	repo := openStorageRepo(t, dbPath)
	pool, err := domain.NewStoragePoolRuntime("pool-a", "Pool A", 80)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	if err := repo.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if _, err := repo.AttachDisk(ctx, "pool-a", domain.StoragePoolDisk{DevicePath: "/dev/sdb", SizeBytes: 100}); err != nil {
		t.Fatalf("attach disk: %v", err)
	}
	if _, _, err := repo.ReserveWrite(ctx, "pool-a", 25); err != nil {
		t.Fatalf("reserve write: %v", err)
	}

	reopened := openStorageRepo(t, dbPath)
	reloaded, err := reopened.FindPool(ctx, "pool-a")
	if err != nil {
		t.Fatalf("find reopened pool: %v", err)
	}
	if reloaded.Name != "Pool A" || reloaded.WarningThresholdPct != 80 {
		t.Fatalf("unexpected reopened pool: %+v", reloaded)
	}
	if len(reloaded.Disks) != 1 || reloaded.Disks[0].DevicePath != "/dev/sdb" {
		t.Fatalf("unexpected reopened disks: %+v", reloaded.Disks)
	}
	if reloaded.Capacity.TotalBytes != 100 || reloaded.Capacity.UsedBytes != 25 || reloaded.Capacity.FreeBytes != 75 {
		t.Fatalf("unexpected reopened capacity: %+v", reloaded.Capacity)
	}
	if owner, ok := reopened.DiskOwner(ctx, "/dev/sdb"); !ok || owner != "pool-a" {
		t.Fatalf("expected disk owner pool-a, got owner=%q ok=%v", owner, ok)
	}
}

func TestStoragePoolRepoAttachConflictAndRollback(t *testing.T) {
	ctx := context.Background()
	repo := openStorageRepo(t, filepath.Join(t.TempDir(), "metadata.db"))
	poolA, _ := domain.NewStoragePoolRuntime("pool-a", "Pool A", 80)
	poolB, _ := domain.NewStoragePoolRuntime("pool-b", "Pool B", 80)
	if err := repo.CreatePool(ctx, poolA); err != nil {
		t.Fatalf("create pool a: %v", err)
	}
	if err := repo.CreatePool(ctx, poolB); err != nil {
		t.Fatalf("create pool b: %v", err)
	}
	if _, err := repo.AttachDisk(ctx, "pool-a", domain.StoragePoolDisk{DevicePath: "/dev/sdb", SizeBytes: 100}); err != nil {
		t.Fatalf("attach disk pool a: %v", err)
	}
	if _, err := repo.AttachDisk(ctx, "pool-b", domain.StoragePoolDisk{DevicePath: "/dev/sdb", SizeBytes: 100}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected cross-pool disk conflict, got %v", err)
	}
	updated, warning, err := repo.ReserveWrite(ctx, "pool-a", 90)
	if err != nil {
		t.Fatalf("reserve write: %v", err)
	}
	if !warning || updated.Capacity.UsedBytes != 90 {
		t.Fatalf("unexpected reserve result warning=%v capacity=%+v", warning, updated.Capacity)
	}
	if _, _, err := repo.ReserveWrite(ctx, "pool-a", 11); !errors.Is(err, domain.ErrCapacityExceeded) {
		t.Fatalf("expected capacity exceeded, got %v", err)
	}
	rolledBack, err := repo.RollbackReservedWrite(ctx, "pool-a", 90)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rolledBack.Capacity.UsedBytes != 0 || rolledBack.Capacity.FreeBytes != 100 {
		t.Fatalf("unexpected rollback capacity: %+v", rolledBack.Capacity)
	}
}

func TestStoragePoolRepoSetUsedBytesPersists(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	repo := openStorageRepo(t, dbPath)
	pool, err := domain.NewStoragePoolRuntime("pool-a", "Pool A", 80)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	if err := repo.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if _, err := repo.AttachDisk(ctx, "pool-a", domain.StoragePoolDisk{DevicePath: "/dev/sdb", SizeBytes: 100}); err != nil {
		t.Fatalf("attach disk: %v", err)
	}
	if _, err := repo.SetUsedBytes(ctx, "pool-a", 25); err != nil {
		t.Fatalf("set used bytes: %v", err)
	}

	reopened := openStorageRepo(t, dbPath)
	reloaded, err := reopened.FindPool(ctx, "pool-a")
	if err != nil {
		t.Fatalf("find reopened pool: %v", err)
	}
	if reloaded.Capacity.UsedBytes != 25 || reloaded.Capacity.FreeBytes != 75 || reloaded.Capacity.UsedPercent != 25 {
		t.Fatalf("unexpected reopened capacity: %+v", reloaded.Capacity)
	}
}

func TestStoragePoolRepoPreservesPersistedPoolStatus(t *testing.T) {
	ctx := context.Background()
	repo := openStorageRepo(t, filepath.Join(t.TempDir(), "metadata.db"))
	pool, err := domain.NewStoragePoolRuntime("pool-disabled", "Pool Disabled", 80)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	pool.Status = domain.PoolDisabled
	if err := repo.SavePool(ctx, pool); err != nil {
		t.Fatalf("save pool: %v", err)
	}

	reloaded, err := repo.FindPool(ctx, "pool-disabled")
	if err != nil {
		t.Fatalf("find pool: %v", err)
	}
	if reloaded.Status != domain.PoolDisabled {
		t.Fatalf("expected persisted status %q, got %q", domain.PoolDisabled, reloaded.Status)
	}
	if reloaded.Capacity.TotalBytes != 0 || reloaded.Capacity.FreeBytes != 0 {
		t.Fatalf("expected recomputed empty capacity, got %+v", reloaded.Capacity)
	}
}

func TestStoragePoolRepoDiscoveryHistoryIsEphemeral(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	repo := openStorageRepo(t, dbPath)
	for i := 0; i < maxDiskDiscoveryHistories+10; i++ {
		repo.RecordDiscovery(ctx, []domain.StorageManagedDisk{{DevicePath: fmt.Sprintf("/dev/sd%c", 'a'+(i%26)), SizeBytes: 1}})
	}
	if _, ok := repo.LatestDiscovery(ctx); !ok {
		t.Fatalf("expected in-process discovery history")
	}

	reopened := openStorageRepo(t, dbPath)
	if _, ok := reopened.LatestDiscovery(ctx); ok {
		t.Fatalf("expected discovery history to remain ephemeral after reopen")
	}
}
