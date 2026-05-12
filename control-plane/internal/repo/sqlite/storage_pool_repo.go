package sqlite

import (
	"context"
	"database/sql"
	"errors"
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
	db *sql.DB

	// Disk discovery is an operational cache by design; persistent pool and
	// disk metadata live in SQLite while discovery history resets on restart.
	discoveryMu      sync.RWMutex
	discoveryHistory []domain.DiskDiscoveryRecord
}

func NewStoragePoolRepo(db *sql.DB) *StoragePoolRepo {
	return &StoragePoolRepo{
		db:               db,
		discoveryHistory: make([]domain.DiskDiscoveryRecord, 0, maxDiskDiscoveryHistories),
	}
}

func (r *StoragePoolRepo) CreatePool(ctx context.Context, pool *domain.StoragePoolRuntime) error {
	if pool == nil {
		return domain.ErrInvalidInput
	}
	var count int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_pools`).Scan(&count); err != nil {
		return err
	}
	if count >= maxStoragePools {
		return domain.ErrInvalidState
	}
	if _, err := r.FindPool(ctx, pool.PoolID); err == nil {
		return domain.ErrConflict
	} else if err != domain.ErrNotFound {
		return err
	}
	return r.SavePool(ctx, pool)
}

func (r *StoragePoolRepo) SavePool(ctx context.Context, pool *domain.StoragePoolRuntime) error {
	if pool == nil {
		return domain.ErrInvalidInput
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := savePoolTx(ctx, tx, pool); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (r *StoragePoolRepo) FindPool(ctx context.Context, poolID string) (*domain.StoragePoolRuntime, error) {
	pool, err := r.loadPool(ctx, r.db, strings.TrimSpace(poolID))
	if err != nil {
		return nil, err
	}
	disks, err := r.loadPoolDisks(ctx, r.db, pool.PoolID)
	if err != nil {
		return nil, err
	}
	pool.Disks = disks
	refreshCapacitySnapshot(pool)
	return pool, nil
}

func (r *StoragePoolRepo) ListPools(ctx context.Context) []*domain.StoragePoolRuntime {
	rows, err := r.db.QueryContext(ctx, `SELECT pool_id FROM storage_pools ORDER BY pool_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	out := make([]*domain.StoragePoolRuntime, 0, len(ids))
	for _, id := range ids {
		pool, err := r.FindPool(ctx, id)
		if err == nil {
			out = append(out, pool)
		}
	}
	return out
}

func (r *StoragePoolRepo) DeletePool(ctx context.Context, poolID string) error {
	return deleteByID(ctx, r.db, `DELETE FROM storage_pools WHERE pool_id = ?`, poolID)
}

func (r *StoragePoolRepo) DiskOwner(ctx context.Context, devicePath string) (string, bool) {
	var poolID string
	err := r.db.QueryRowContext(ctx, `SELECT pool_id FROM storage_pool_disks WHERE device_path = ?`, storageutil.NormalizeDevicePath(devicePath)).Scan(&poolID)
	if err != nil {
		return "", false
	}
	return poolID, true
}

func (r *StoragePoolRepo) AttachDisk(ctx context.Context, poolID string, disk domain.StoragePoolDisk) (*domain.StoragePoolRuntime, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	pool, err := r.loadPool(ctx, tx, strings.TrimSpace(poolID))
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	disks, err := r.loadPoolDisks(ctx, tx, pool.PoolID)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	pool.Disks = disks
	refreshCapacitySnapshot(pool)
	if len(pool.Disks) >= maxDisksPerStoragePool {
		_ = tx.Rollback()
		return nil, domain.ErrInvalidState
	}
	normalized := storageutil.NormalizeDevicePath(disk.DevicePath)
	if owner, exists, err := diskOwnerTx(ctx, tx, normalized); err != nil {
		_ = tx.Rollback()
		return nil, err
	} else if exists && owner != pool.PoolID {
		_ = tx.Rollback()
		return nil, domain.ErrConflict
	}
	disk.DevicePath = normalized
	if err := pool.AttachDisk(disk); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := savePoolTx(ctx, tx, pool); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cloneStoragePool(pool), nil
}

func (r *StoragePoolRepo) DetachDisk(ctx context.Context, poolID, devicePath string) (*domain.StoragePoolRuntime, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	pool, err := r.loadPool(ctx, tx, strings.TrimSpace(poolID))
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	disks, err := r.loadPoolDisks(ctx, tx, pool.PoolID)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	pool.Disks = disks
	refreshCapacitySnapshot(pool)
	normalized := storageutil.NormalizeDevicePath(devicePath)
	if owner, exists, err := diskOwnerTx(ctx, tx, normalized); err != nil {
		_ = tx.Rollback()
		return nil, err
	} else if !exists || owner != pool.PoolID {
		_ = tx.Rollback()
		return nil, domain.ErrNotFound
	}
	if err := pool.DetachDisk(normalized); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := savePoolTx(ctx, tx, pool); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cloneStoragePool(pool), nil
}

func (r *StoragePoolRepo) ReserveWrite(ctx context.Context, poolID string, bytes int64) (*domain.StoragePoolRuntime, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	pool, err := r.loadPoolWithDisks(ctx, tx, strings.TrimSpace(poolID))
	if err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	warning, err := pool.ReserveWrite(bytes)
	if err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	if err := savePoolTx(ctx, tx, pool); err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return cloneStoragePool(pool), warning, nil
}

func (r *StoragePoolRepo) RollbackReservedWrite(ctx context.Context, poolID string, bytes int64) (*domain.StoragePoolRuntime, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	pool, err := r.loadPoolWithDisks(ctx, tx, strings.TrimSpace(poolID))
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := pool.RollbackReservedWrite(bytes); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := savePoolTx(ctx, tx, pool); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cloneStoragePool(pool), nil
}

func (r *StoragePoolRepo) SetUsedBytes(ctx context.Context, poolID string, usedBytes int64) (*domain.StoragePoolRuntime, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	pool, err := r.loadPoolWithDisks(ctx, tx, strings.TrimSpace(poolID))
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := pool.SetUsedBytes(usedBytes); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := savePoolTx(ctx, tx, pool); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cloneStoragePool(pool), nil
}

func (r *StoragePoolRepo) RecordDiscovery(_ context.Context, disks []domain.StorageManagedDisk) {
	r.discoveryMu.Lock()
	defer r.discoveryMu.Unlock()
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
	r.discoveryMu.RLock()
	defer r.discoveryMu.RUnlock()
	if len(r.discoveryHistory) == 0 {
		return domain.DiskDiscoveryRecord{}, false
	}
	last := r.discoveryHistory[len(r.discoveryHistory)-1]
	cp := make([]domain.StorageManagedDisk, len(last.Disks))
	copy(cp, last.Disks)
	last.Disks = cp
	return last, true
}

type poolLoader interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (r *StoragePoolRepo) loadPool(ctx context.Context, q poolLoader, poolID string) (*domain.StoragePoolRuntime, error) {
	var pool domain.StoragePoolRuntime
	var status, createdAt, updatedAt string
	err := q.QueryRowContext(ctx, `
SELECT pool_id, name, status, warning_threshold_pct, used_bytes, created_at, updated_at
FROM storage_pools WHERE pool_id = ?`, poolID).Scan(
		&pool.PoolID,
		&pool.Name,
		&status,
		&pool.WarningThresholdPct,
		&pool.Capacity.UsedBytes,
		&createdAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	pool.Status = domain.PoolStatus(status)
	pool.CreatedAt = parseTime(createdAt)
	pool.UpdatedAt = parseTime(updatedAt)
	pool.Disks = make([]domain.StoragePoolDisk, 0)
	return &pool, nil
}

func (r *StoragePoolRepo) loadPoolWithDisks(ctx context.Context, q poolLoader, poolID string) (*domain.StoragePoolRuntime, error) {
	pool, err := r.loadPool(ctx, q, poolID)
	if err != nil {
		return nil, err
	}
	disks, err := r.loadPoolDisks(ctx, q, pool.PoolID)
	if err != nil {
		return nil, err
	}
	pool.Disks = disks
	refreshCapacitySnapshot(pool)
	return pool, nil
}

func (r *StoragePoolRepo) loadPoolDisks(ctx context.Context, q poolLoader, poolID string) ([]domain.StoragePoolDisk, error) {
	rows, err := q.QueryContext(ctx, `
SELECT device_path, size_bytes, attached_at
FROM storage_pool_disks WHERE pool_id = ? ORDER BY device_path`, poolID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	disks := make([]domain.StoragePoolDisk, 0)
	for rows.Next() {
		var disk domain.StoragePoolDisk
		var attachedAt string
		if err := rows.Scan(&disk.DevicePath, &disk.SizeBytes, &attachedAt); err != nil {
			return nil, err
		}
		disk.AttachedAt = parseTime(attachedAt)
		disks = append(disks, disk)
	}
	return disks, rows.Err()
}

func savePoolTx(ctx context.Context, tx *sql.Tx, pool *domain.StoragePoolRuntime) error {
	refreshCapacitySnapshot(pool)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO storage_pools (
  pool_id, name, status, warning_threshold_pct, used_bytes, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(pool_id) DO UPDATE SET
  name=excluded.name,
  status=excluded.status,
  warning_threshold_pct=excluded.warning_threshold_pct,
  used_bytes=excluded.used_bytes,
  created_at=excluded.created_at,
  updated_at=excluded.updated_at`,
		pool.PoolID,
		pool.Name,
		string(pool.Status),
		pool.WarningThresholdPct,
		pool.Capacity.UsedBytes,
		formatTime(pool.CreatedAt),
		formatTime(pool.UpdatedAt),
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM storage_pool_disks WHERE pool_id = ?`, pool.PoolID); err != nil {
		return err
	}
	for _, disk := range pool.Disks {
		_, err := tx.ExecContext(ctx, `
INSERT INTO storage_pool_disks(device_path, pool_id, size_bytes, attached_at)
VALUES (?, ?, ?, ?)`,
			storageutil.NormalizeDevicePath(disk.DevicePath),
			pool.PoolID,
			disk.SizeBytes,
			formatTime(disk.AttachedAt),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func diskOwnerTx(ctx context.Context, tx *sql.Tx, devicePath string) (string, bool, error) {
	var poolID string
	err := tx.QueryRowContext(ctx, `SELECT pool_id FROM storage_pool_disks WHERE device_path = ?`, devicePath).Scan(&poolID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return poolID, true, nil
}

func refreshCapacitySnapshot(pool *domain.StoragePoolRuntime) {
	var total int64
	for _, disk := range pool.Disks {
		if disk.SizeBytes > 0 {
			total += disk.SizeBytes
		}
	}
	if pool.Capacity.UsedBytes < 0 {
		pool.Capacity.UsedBytes = 0
	}
	if pool.Capacity.UsedBytes > total {
		pool.Capacity.UsedBytes = total
	}
	free := total - pool.Capacity.UsedBytes
	usedPercent := 0
	if total > 0 {
		usedPercent = int((pool.Capacity.UsedBytes * 100) / total)
	}
	pool.Capacity.TotalBytes = total
	pool.Capacity.FreeBytes = free
	pool.Capacity.UsedPercent = usedPercent
	pool.Capacity.WarningThresholdPct = pool.WarningThresholdPct
	pool.Capacity.Warning = total > 0 && usedPercent >= pool.WarningThresholdPct
	pool.Capacity.Exhausted = total > 0 && free == 0
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
