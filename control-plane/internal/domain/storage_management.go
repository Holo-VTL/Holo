package domain

import (
	"strings"
	"time"
)

type DiskAvailability string

const (
	DiskAvailable   DiskAvailability = "available"
	DiskUnavailable DiskAvailability = "unavailable"
)

type StorageManagedDisk struct {
	DevicePath        string           `json:"devicePath"`
	SizeBytes         int64            `json:"sizeBytes"`
	Vendor            string           `json:"vendor,omitempty"`
	Model             string           `json:"model,omitempty"`
	Serial            string           `json:"serial,omitempty"`
	Availability      DiskAvailability `json:"availability"`
	UnavailableReason string           `json:"unavailableReason,omitempty"`
	PoolID            string           `json:"poolId,omitempty"`
}

type StoragePoolDisk struct {
	DevicePath string    `json:"devicePath"`
	SizeBytes  int64     `json:"sizeBytes"`
	AttachedAt time.Time `json:"attachedAt"`
}

type StoragePoolCapacitySnapshot struct {
	TotalBytes          int64 `json:"totalBytes"`
	UsedBytes           int64 `json:"usedBytes"`
	FreeBytes           int64 `json:"freeBytes"`
	UsedPercent         int   `json:"usedPercent"`
	Warning             bool  `json:"warning"`
	Exhausted           bool  `json:"exhausted"`
	WarningThresholdPct int   `json:"warningThresholdPct"`
}

type StoragePoolRuntime struct {
	Timestamped
	PoolID              string                      `json:"poolId"`
	Name                string                      `json:"name"`
	Status              PoolStatus                  `json:"status"`
	WarningThresholdPct int                         `json:"warningThresholdPct"`
	Disks               []StoragePoolDisk           `json:"disks"`
	Capacity            StoragePoolCapacitySnapshot `json:"capacity"`
}

type DiskDiscoveryRecord struct {
	RecordedAt time.Time            `json:"recordedAt"`
	Disks      []StorageManagedDisk `json:"disks"`
}

func NewStoragePoolRuntime(id, name string, warningThresholdPct int) (*StoragePoolRuntime, error) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" || name == "" {
		return nil, ErrInvalidInput
	}
	if warningThresholdPct == 0 {
		warningThresholdPct = 90
	}
	if warningThresholdPct < 50 || warningThresholdPct > 99 {
		return nil, ErrInvalidInput
	}
	now := time.Now().UTC()
	pool := &StoragePoolRuntime{
		Timestamped:         Timestamped{CreatedAt: now, UpdatedAt: now},
		PoolID:              id,
		Name:                name,
		Status:              PoolActive,
		WarningThresholdPct: warningThresholdPct,
		Disks:               make([]StoragePoolDisk, 0, 4),
	}
	pool.refreshCapacitySnapshot()
	return pool, nil
}

func (p *StoragePoolRuntime) refreshCapacitySnapshot() {
	var total int64
	for _, d := range p.Disks {
		if d.SizeBytes > 0 {
			total += d.SizeBytes
		}
	}
	if p.Capacity.UsedBytes < 0 {
		p.Capacity.UsedBytes = 0
	}
	if p.Capacity.UsedBytes > total {
		p.Capacity.UsedBytes = total
	}
	free := total - p.Capacity.UsedBytes
	usedPercent := 0
	if total > 0 {
		usedPercent = int((p.Capacity.UsedBytes * 100) / total)
	}
	p.Capacity.TotalBytes = total
	p.Capacity.FreeBytes = free
	p.Capacity.UsedPercent = usedPercent
	p.Capacity.WarningThresholdPct = p.WarningThresholdPct
	p.Capacity.Warning = total > 0 && usedPercent >= p.WarningThresholdPct
	p.Capacity.Exhausted = total > 0 && free == 0
	if total == 0 {
		p.Status = PoolDegraded
	} else {
		p.Status = PoolActive
	}
}

func (p *StoragePoolRuntime) AttachDisk(disk StoragePoolDisk) error {
	disk.DevicePath = strings.TrimSpace(disk.DevicePath)
	if disk.DevicePath == "" || disk.SizeBytes <= 0 {
		return ErrInvalidInput
	}
	for _, existing := range p.Disks {
		if existing.DevicePath == disk.DevicePath {
			return ErrConflict
		}
	}
	if disk.AttachedAt.IsZero() {
		disk.AttachedAt = time.Now().UTC()
	}
	p.Disks = append(p.Disks, disk)
	p.UpdatedAt = time.Now().UTC()
	p.refreshCapacitySnapshot()
	return nil
}

func (p *StoragePoolRuntime) DetachDisk(devicePath string) error {
	devicePath = strings.TrimSpace(devicePath)
	if devicePath == "" {
		return ErrInvalidInput
	}
	idx := -1
	for i := range p.Disks {
		if p.Disks[i].DevicePath == devicePath {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrNotFound
	}
	p.Disks = append(p.Disks[:idx], p.Disks[idx+1:]...)
	p.UpdatedAt = time.Now().UTC()
	p.refreshCapacitySnapshot()
	return nil
}

func (p *StoragePoolRuntime) ReserveWrite(bytes int64) (bool, error) {
	if bytes <= 0 {
		return false, ErrInvalidInput
	}
	if p.Capacity.TotalBytes <= 0 || p.Capacity.FreeBytes < bytes {
		return false, ErrCapacityExceeded
	}
	prevWarning := p.Capacity.Warning
	p.Capacity.UsedBytes += bytes
	p.UpdatedAt = time.Now().UTC()
	p.refreshCapacitySnapshot()
	return !prevWarning && p.Capacity.Warning, nil
}

func (p *StoragePoolRuntime) SetUsedBytes(bytes int64) error {
	if bytes < 0 {
		return ErrInvalidInput
	}
	p.Capacity.UsedBytes = bytes
	p.UpdatedAt = time.Now().UTC()
	p.refreshCapacitySnapshot()
	return nil
}

func (p *StoragePoolRuntime) RollbackReservedWrite(bytes int64) error {
	if bytes <= 0 {
		return ErrInvalidInput
	}
	p.Capacity.UsedBytes -= bytes
	if p.Capacity.UsedBytes < 0 {
		p.Capacity.UsedBytes = 0
	}
	p.UpdatedAt = time.Now().UTC()
	p.refreshCapacitySnapshot()
	return nil
}
