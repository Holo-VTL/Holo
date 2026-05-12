package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
	"github.com/Holo-VTL/Holo/control-plane/internal/storageutil"
)

const (
	maxStoragePools           = 256
	maxDisksPerStoragePool    = 64
	maxDiskDiscoveryHistories = 128
)

type StoragePoolRepo struct {
	mu               sync.RWMutex
	pools            map[string]*domain.StoragePoolRuntime
	diskOwners       map[string]string
	discoveryHistory []domain.DiskDiscoveryRecord
}

func NewStoragePoolRepo() *StoragePoolRepo {
	return &StoragePoolRepo{
		pools:            make(map[string]*domain.StoragePoolRuntime),
		diskOwners:       make(map[string]string),
		discoveryHistory: make([]domain.DiskDiscoveryRecord, 0, maxDiskDiscoveryHistories),
	}
}

func (r *StoragePoolRepo) CreatePool(_ context.Context, pool *domain.StoragePoolRuntime) error {
	if pool == nil {
		return domain.ErrInvalidInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pools[pool.PoolID]; ok {
		return domain.ErrConflict
	}
	if len(r.pools) >= maxStoragePools {
		return domain.ErrInvalidState
	}
	r.pools[pool.PoolID] = cloneStoragePool(pool)
	return nil
}

func (r *StoragePoolRepo) SavePool(_ context.Context, pool *domain.StoragePoolRuntime) error {
	if pool == nil {
		return domain.ErrInvalidInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pools[pool.PoolID] = cloneStoragePool(pool)
	return nil
}

func (r *StoragePoolRepo) FindPool(_ context.Context, poolID string) (*domain.StoragePoolRuntime, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	poolID = strings.TrimSpace(poolID)
	pool, ok := r.pools[poolID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return cloneStoragePool(pool), nil
}

func (r *StoragePoolRepo) ListPools(_ context.Context) []*domain.StoragePoolRuntime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.StoragePoolRuntime, 0, len(r.pools))
	for _, pool := range r.pools {
		out = append(out, cloneStoragePool(pool))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PoolID < out[j].PoolID
	})
	return out
}

func (r *StoragePoolRepo) DeletePool(_ context.Context, poolID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	poolID = strings.TrimSpace(poolID)
	pool, ok := r.pools[poolID]
	if !ok {
		return domain.ErrNotFound
	}
	for _, d := range pool.Disks {
		delete(r.diskOwners, storageutil.NormalizeDevicePath(d.DevicePath))
	}
	delete(r.pools, poolID)
	return nil
}

func (r *StoragePoolRepo) DiskOwner(_ context.Context, devicePath string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	poolID, ok := r.diskOwners[storageutil.NormalizeDevicePath(devicePath)]
	return poolID, ok
}

func (r *StoragePoolRepo) AttachDisk(_ context.Context, poolID string, disk domain.StoragePoolDisk) (*domain.StoragePoolRuntime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	poolID = strings.TrimSpace(poolID)
	pool, ok := r.pools[poolID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if len(pool.Disks) >= maxDisksPerStoragePool {
		return nil, domain.ErrInvalidState
	}
	normalized := storageutil.NormalizeDevicePath(disk.DevicePath)
	if owner, exists := r.diskOwners[normalized]; exists && owner != poolID {
		return nil, domain.ErrConflict
	}
	disk.DevicePath = normalized
	if err := pool.AttachDisk(disk); err != nil {
		return nil, err
	}
	r.diskOwners[normalized] = poolID
	return cloneStoragePool(pool), nil
}

func (r *StoragePoolRepo) DetachDisk(_ context.Context, poolID, devicePath string) (*domain.StoragePoolRuntime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	poolID = strings.TrimSpace(poolID)
	pool, ok := r.pools[poolID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	normalized := storageutil.NormalizeDevicePath(devicePath)
	if owner, exists := r.diskOwners[normalized]; !exists || owner != poolID {
		return nil, domain.ErrNotFound
	}
	if err := pool.DetachDisk(normalized); err != nil {
		return nil, err
	}
	delete(r.diskOwners, normalized)
	return cloneStoragePool(pool), nil
}

func (r *StoragePoolRepo) ReserveWrite(_ context.Context, poolID string, bytes int64) (*domain.StoragePoolRuntime, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	poolID = strings.TrimSpace(poolID)
	pool, ok := r.pools[poolID]
	if !ok {
		return nil, false, domain.ErrNotFound
	}
	warningTriggered, err := pool.ReserveWrite(bytes)
	if err != nil {
		return nil, false, err
	}
	return cloneStoragePool(pool), warningTriggered, nil
}

func (r *StoragePoolRepo) RollbackReservedWrite(_ context.Context, poolID string, bytes int64) (*domain.StoragePoolRuntime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	poolID = strings.TrimSpace(poolID)
	pool, ok := r.pools[poolID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if err := pool.RollbackReservedWrite(bytes); err != nil {
		return nil, err
	}
	return cloneStoragePool(pool), nil
}

func (r *StoragePoolRepo) SetUsedBytes(_ context.Context, poolID string, usedBytes int64) (*domain.StoragePoolRuntime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	poolID = strings.TrimSpace(poolID)
	pool, ok := r.pools[poolID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if err := pool.SetUsedBytes(usedBytes); err != nil {
		return nil, err
	}
	return cloneStoragePool(pool), nil
}

func (r *StoragePoolRepo) RecordDiscovery(_ context.Context, disks []domain.StorageManagedDisk) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]domain.StorageManagedDisk, len(disks))
	copy(cp, disks)
	r.discoveryHistory = append(r.discoveryHistory, domain.DiskDiscoveryRecord{
		RecordedAt: time.Now().UTC(),
		Disks:      cp,
	})
	if len(r.discoveryHistory) > maxDiskDiscoveryHistories {
		r.discoveryHistory = r.discoveryHistory[len(r.discoveryHistory)-maxDiskDiscoveryHistories:]
	}
}

func (r *StoragePoolRepo) LatestDiscovery(_ context.Context) (domain.DiskDiscoveryRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.discoveryHistory) == 0 {
		return domain.DiskDiscoveryRecord{}, false
	}
	last := r.discoveryHistory[len(r.discoveryHistory)-1]
	cp := make([]domain.StorageManagedDisk, len(last.Disks))
	copy(cp, last.Disks)
	last.Disks = cp
	return last, true
}

func cloneStoragePool(in *domain.StoragePoolRuntime) *domain.StoragePoolRuntime {
	if in == nil {
		return nil
	}
	cp := *in
	if in.Disks != nil {
		cp.Disks = make([]domain.StoragePoolDisk, len(in.Disks))
		copy(cp.Disks, in.Disks)
	}
	return &cp
}
