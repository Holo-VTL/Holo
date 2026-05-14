package domain

import "time"

type CartridgeLifecycleState string
type RetentionState string

const (
	CartridgeAvailable CartridgeLifecycleState = "available"
	CartridgeMounted   CartridgeLifecycleState = "mounted"
	CartridgeImported  CartridgeLifecycleState = "imported"
	CartridgeExported  CartridgeLifecycleState = "exported"
	CartridgeRetired   CartridgeLifecycleState = "retired"
)

const (
	RetentionNone    RetentionState = "none"
	RetentionLocked  RetentionState = "locked"
	RetentionExpired RetentionState = "expired"
)

type VirtualCartridge struct {
	Timestamped
	CartridgeID           string                  `json:"cartridgeId"`
	PoolID                string                  `json:"poolId"`
	LibraryID             string                  `json:"libraryId"`
	Barcode               string                  `json:"barcode"`
	CapacityBytes         int64                   `json:"capacityBytes"`
	UsedBytes             int64                   `json:"usedBytes"`
	LifecycleState        CartridgeLifecycleState `json:"lifecycleState"`
	RetentionState        RetentionState          `json:"retentionState"`
	CurrentElementAddress *int                    `json:"currentElementAddress,omitempty"`
	AssignedSlotAddress   *int                    `json:"assignedSlotAddress,omitempty"`
}

func NewVirtualCartridge(id, poolID, libraryID, barcode string, capacity int64) *VirtualCartridge {
	now := time.Now().UTC()
	return &VirtualCartridge{
		Timestamped:    Timestamped{CreatedAt: now, UpdatedAt: now},
		CartridgeID:    id,
		PoolID:         poolID,
		LibraryID:      libraryID,
		Barcode:        barcode,
		CapacityBytes:  capacity,
		LifecycleState: CartridgeAvailable,
		RetentionState: RetentionNone,
	}
}

func (c *VirtualCartridge) TransitionTo(next CartridgeLifecycleState) error {
	if c.LifecycleState == CartridgeRetired {
		return ErrInvalidState
	}
	valid := map[CartridgeLifecycleState]map[CartridgeLifecycleState]bool{
		CartridgeAvailable: {CartridgeMounted: true, CartridgeExported: true, CartridgeRetired: true},
		CartridgeMounted:   {CartridgeAvailable: true},
		CartridgeImported:  {CartridgeAvailable: true},
		CartridgeExported:  {CartridgeImported: true},
	}
	if !valid[c.LifecycleState][next] {
		return ErrInvalidState
	}
	c.LifecycleState = next
	c.UpdatedAt = time.Now().UTC()
	return nil
}
