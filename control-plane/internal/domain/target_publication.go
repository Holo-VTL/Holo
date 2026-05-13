package domain

import "time"

type PublicationState string

const (
	PublicationCreating PublicationState = "creating"
	PublicationReady    PublicationState = "ready"
	PublicationFailed   PublicationState = "failed"
	PublicationDisabled PublicationState = "disabled"
)

type TargetPublication struct {
	Timestamped
	PublicationID      string           `json:"publicationId"`
	PoolID             string           `json:"poolId"`
	LibraryID          string           `json:"libraryId"`
	DriveID            string           `json:"driveId"`
	CartridgeID        string           `json:"cartridgeId"`
	TargetIQN          string           `json:"targetIqn"`
	DeviceRole         string           `json:"deviceRole"`
	DeviceProfile      string           `json:"deviceProfile,omitempty"`
	DriveProfile       string           `json:"driveProfile,omitempty"`
	Portal             string           `json:"portal"`
	State              PublicationState `json:"state"`
	LastError          string           `json:"lastError,omitempty"`
	CompressionEnabled bool             `json:"compressionEnabled"`
	DedupEnabled       bool             `json:"dedupEnabled"`
	ConnectedHosts     *ConnectedHosts  `json:"connectedHosts,omitempty"`
}

type ConnectedHosts struct {
	Available    bool     `json:"available"`
	HostCount    int      `json:"hostCount"`
	SessionCount int      `json:"sessionCount"`
	Initiators   []string `json:"initiators"`
	LastError    string   `json:"lastError,omitempty"`
}

func NewTargetPublication(id, poolID, libraryID, driveID, cartridgeID, targetIQN string) (*TargetPublication, error) {
	if ValidateManagementID(id) != nil ||
		ValidateManagementID(poolID) != nil ||
		ValidateManagementID(libraryID) != nil ||
		ValidateManagementID(driveID) != nil ||
		ValidateManagementID(cartridgeID) != nil ||
		ValidateTargetIQN(targetIQN) != nil {
		return nil, ErrInvalidInput
	}
	now := time.Now().UTC()
	return &TargetPublication{
		Timestamped:   Timestamped{CreatedAt: now, UpdatedAt: now},
		PublicationID: id,
		PoolID:        poolID,
		LibraryID:     libraryID,
		DriveID:       driveID,
		CartridgeID:   cartridgeID,
		TargetIQN:     targetIQN,
		DeviceRole:    "drive",
		State:         PublicationCreating,
	}, nil
}

func (p *TargetPublication) SetDeviceIdentity(role, profile string) error {
	if role == "" {
		role = "drive"
	}
	if role != "drive" && role != "changer" {
		return ErrInvalidInput
	}
	if ValidateProfileToken(profile) != nil {
		return ErrInvalidInput
	}
	p.DeviceRole = role
	p.DeviceProfile = profile
	p.UpdatedAt = time.Now().UTC()
	return nil
}

func (p *TargetPublication) SetDriveProfile(profile string) {
	if ValidateProfileToken(profile) != nil {
		return
	}
	p.DriveProfile = profile
	p.UpdatedAt = time.Now().UTC()
}

func (p *TargetPublication) MarkReady(portal string) error {
	if p.State != PublicationCreating {
		return ErrInvalidState
	}
	if portal == "" {
		return ErrInvalidInput
	}
	p.Portal = portal
	p.State = PublicationReady
	p.LastError = ""
	p.UpdatedAt = time.Now().UTC()
	return nil
}

func (p *TargetPublication) MarkFailed(msg string) error {
	if p.State != PublicationCreating {
		return ErrInvalidState
	}
	if msg == "" {
		msg = "publish failed"
	}
	p.State = PublicationFailed
	p.LastError = msg
	p.UpdatedAt = time.Now().UTC()
	return nil
}

func (p *TargetPublication) MarkRuntimeFailed(msg string) {
	if msg == "" {
		msg = "runtime restore failed"
	}
	p.State = PublicationFailed
	p.LastError = msg
	p.UpdatedAt = time.Now().UTC()
}

func (p *TargetPublication) Disable() error {
	if p.State != PublicationReady && p.State != PublicationFailed {
		return ErrInvalidState
	}
	p.State = PublicationDisabled
	p.UpdatedAt = time.Now().UTC()
	return nil
}

func (p *TargetPublication) Reopen() error {
	if p.State != PublicationDisabled {
		return ErrInvalidState
	}
	p.State = PublicationCreating
	p.LastError = ""
	p.UpdatedAt = time.Now().UTC()
	return nil
}
