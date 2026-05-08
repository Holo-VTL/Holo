package domain

import "time"

type PoolStatus string

const (
	PoolActive   PoolStatus = "active"
	PoolDegraded PoolStatus = "degraded"
	PoolDisabled PoolStatus = "disabled"
)

type StoragePool struct {
	Timestamped
	PoolID       string     `json:"poolId"`
	Name         string     `json:"name"`
	CapacityByte int64      `json:"capacityBytes"`
	UsedByte     int64      `json:"usedBytes"`
	Status       PoolStatus `json:"status"`
}

func NewStoragePool(id, name string, capBytes int64) (*StoragePool, error) {
	if id == "" || name == "" || capBytes <= 0 {
		return nil, ErrInvalidInput
	}
	now := time.Now().UTC()
	return &StoragePool{
		Timestamped:  Timestamped{CreatedAt: now, UpdatedAt: now},
		PoolID:       id,
		Name:         name,
		CapacityByte: capBytes,
		Status:       PoolActive,
	}, nil
}

func (p *StoragePool) ValidateUsage() error {
	if p.UsedByte < 0 || p.UsedByte > p.CapacityByte {
		return ErrInvalidInput
	}
	return nil
}
