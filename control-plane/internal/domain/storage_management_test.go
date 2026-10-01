package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStoragePoolConstructionAndUsageValidation(t *testing.T) {
	if _, err := NewStoragePool("", "pool", 1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty pool id should be rejected, got %v", err)
	}
	if _, err := NewStoragePool("pool", "pool", 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("non-positive capacity should be rejected, got %v", err)
	}
	legacy, err := NewStoragePool("pool", "Pool", 100)
	if err != nil {
		t.Fatalf("create legacy pool: %v", err)
	}
	if err := legacy.ValidateUsage(); err != nil {
		t.Fatalf("zero usage should be valid: %v", err)
	}
	legacy.UsedByte = 101
	if err := legacy.ValidateUsage(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("usage above capacity should be rejected, got %v", err)
	}
	legacy.UsedByte = -1
	if err := legacy.ValidateUsage(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("negative usage should be rejected, got %v", err)
	}
}

func TestStoragePoolRuntimeTracksCapacityAndDiskLifecycle(t *testing.T) {
	if _, err := NewStoragePoolRuntime("", "Pool", 90); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty pool id should be rejected, got %v", err)
	}
	if _, err := NewStoragePoolRuntime("pool", " ", 90); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty pool name should be rejected, got %v", err)
	}
	for _, threshold := range []int{49, 100} {
		if _, err := NewStoragePoolRuntime("pool", "Pool", threshold); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("threshold %d should be rejected, got %v", threshold, err)
		}
	}

	pool, err := NewStoragePoolRuntime(" pool-a ", " Primary ", 0)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if pool.PoolID != "pool-a" || pool.Name != "Primary" || pool.WarningThresholdPct != 90 || pool.Status != PoolDegraded {
		t.Fatalf("unexpected initial pool state: %+v", pool)
	}
	if err := pool.AttachDisk(StoragePoolDisk{DevicePath: " ", SizeBytes: 100}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty disk path should be rejected, got %v", err)
	}
	if err := pool.AttachDisk(StoragePoolDisk{DevicePath: "/dev/sda", SizeBytes: 0}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("non-positive disk capacity should be rejected, got %v", err)
	}
	if err := pool.AttachDisk(StoragePoolDisk{DevicePath: " /dev/sda ", SizeBytes: 100}); err != nil {
		t.Fatalf("attach disk: %v", err)
	}
	if pool.Disks[0].DevicePath != "/dev/sda" || pool.Disks[0].AttachedAt.IsZero() || pool.Status != PoolActive {
		t.Fatalf("disk attach should normalize and timestamp the disk: %+v", pool)
	}
	if err := pool.AttachDisk(StoragePoolDisk{DevicePath: "/dev/sda", SizeBytes: 100}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate disk should conflict, got %v", err)
	}

	if _, err := pool.ReserveWrite(0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("non-positive reservation should be rejected, got %v", err)
	}
	if warning, err := pool.ReserveWrite(89); err != nil || warning {
		t.Fatalf("reservation below warning threshold should succeed without warning: warning=%v err=%v", warning, err)
	}
	if warning, err := pool.ReserveWrite(1); err != nil || !warning {
		t.Fatalf("reservation reaching warning threshold should report it: warning=%v err=%v", warning, err)
	}
	if pool.Capacity.TotalBytes != 100 || pool.Capacity.UsedBytes != 90 || pool.Capacity.FreeBytes != 10 || !pool.Capacity.Warning || pool.Capacity.Exhausted {
		t.Fatalf("unexpected capacity snapshot at warning threshold: %+v", pool.Capacity)
	}
	if _, err := pool.ReserveWrite(11); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("reservation beyond free capacity should fail, got %v", err)
	}
	if err := pool.SetUsedBytes(-1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("negative usage should be rejected, got %v", err)
	}
	if err := pool.SetUsedBytes(101); err != nil {
		t.Fatalf("set usage: %v", err)
	}
	if pool.Capacity.UsedBytes != 100 || !pool.Capacity.Exhausted {
		t.Fatalf("usage should clamp to capacity and mark exhaustion: %+v", pool.Capacity)
	}
	if err := pool.RollbackReservedWrite(0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("non-positive rollback should be rejected, got %v", err)
	}
	if err := pool.RollbackReservedWrite(150); err != nil {
		t.Fatalf("rollback reservation: %v", err)
	}
	if pool.Capacity.UsedBytes != 0 || pool.Capacity.FreeBytes != 100 || pool.Capacity.Warning || pool.Capacity.Exhausted {
		t.Fatalf("rollback should clamp usage at zero and clear warnings: %+v", pool.Capacity)
	}

	if err := pool.DetachDisk(" "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty disk path should be rejected, got %v", err)
	}
	if err := pool.DetachDisk("/dev/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown disk should be not found, got %v", err)
	}
	if err := pool.DetachDisk("/dev/sda"); err != nil {
		t.Fatalf("detach disk: %v", err)
	}
	if pool.Status != PoolDegraded || pool.Capacity.TotalBytes != 0 || pool.Capacity.FreeBytes != 0 {
		t.Fatalf("pool without disks should be degraded with zero capacity: %+v", pool)
	}
	if _, err := pool.ReserveWrite(1); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("writes without attached capacity should fail, got %v", err)
	}
}

func TestStoragePoolRuntimeMultipleDisksAndCapacityClamping(t *testing.T) {
	pool, err := NewStoragePoolRuntime("pool-multi", "Multiple Disks", 90)
	if err != nil {
		t.Fatal(err)
	}
	for _, devicePath := range []string{"/dev/sda", "/dev/sdb"} {
		if err := pool.AttachDisk(StoragePoolDisk{DevicePath: devicePath, SizeBytes: 100, AttachedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("attach %s: %v", devicePath, err)
		}
	}
	if pool.Capacity.TotalBytes != 200 {
		t.Fatalf("capacity should sum attached disks, got %+v", pool.Capacity)
	}
	if err := pool.SetUsedBytes(150); err != nil {
		t.Fatal(err)
	}
	if err := pool.DetachDisk("/dev/sda"); err != nil {
		t.Fatal(err)
	}
	if pool.Capacity.TotalBytes != 100 || pool.Capacity.UsedBytes != 100 || pool.Capacity.FreeBytes != 0 || !pool.Capacity.Exhausted {
		t.Fatalf("capacity should clamp usage after disk removal: %+v", pool.Capacity)
	}
}

func TestValidateDevicePath(t *testing.T) {
	for _, value := range []string{"/dev/sda", "/dev/disk/by-id/scsi-test", " /dev/mapper/pool "} {
		if err := ValidateDevicePath(value); err != nil {
			t.Errorf("valid device path %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"", "sda", "/dev/../sda", "/dev/sda\n", "/dev/" + strings.Repeat("a", 251)} {
		if err := ValidateDevicePath(value); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("invalid device path %q accepted: %v", value, err)
		}
	}
}
