package domain

import "time"

type MountState string

const (
	MountEmpty  MountState = "empty"
	MountLoaded MountState = "loaded"
	MountBusy   MountState = "busy"
	MountError  MountState = "error"
)

type VirtualDrive struct {
	Timestamped
	DriveID            string     `json:"driveId"`
	LibraryID          string     `json:"libraryId"`
	Slot               int        `json:"slot"`
	IQN                string     `json:"iqn,omitempty"`
	MountState         MountState `json:"mountState"`
	MountedCartridgeID string     `json:"mountedCartridgeId,omitempty"`
}

func NewVirtualDrive(id, libraryID string, slot int) (*VirtualDrive, error) {
	if ValidateManagementID(id) != nil || ValidateManagementID(libraryID) != nil || slot <= 0 {
		return nil, ErrInvalidInput
	}
	now := time.Now().UTC()
	return &VirtualDrive{
		Timestamped: Timestamped{CreatedAt: now, UpdatedAt: now},
		DriveID:     id,
		LibraryID:   libraryID,
		Slot:        slot,
		IQN:         defaultDriveIQN(id),
		MountState:  MountEmpty,
	}, nil
}

func defaultDriveIQN(driveID string) string {
	token := sanitizeIQNToken(driveID, "drive")
	return "iqn.2026-04.cloud.backupnext.holo:drive-" + token
}

func (d *VirtualDrive) Mount(cartridgeID string) error {
	if cartridgeID == "" || d.MountState != MountEmpty {
		return ErrInvalidState
	}
	d.MountState = MountLoaded
	d.MountedCartridgeID = cartridgeID
	d.UpdatedAt = time.Now().UTC()
	return nil
}

func (d *VirtualDrive) Unmount() error {
	if d.MountState == MountBusy {
		return ErrInvalidState
	}
	d.MountState = MountEmpty
	d.MountedCartridgeID = ""
	d.UpdatedAt = time.Now().UTC()
	return nil
}
