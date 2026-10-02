package domain

import (
	"regexp"
	"time"
)

type LocalDeviceKind string

const (
	LocalDeviceKindChanger LocalDeviceKind = "changer"
	LocalDeviceKindDrive   LocalDeviceKind = "drive"
)

type LocalMappingState string

const (
	LocalMappingStateActive         LocalMappingState = "active"
	LocalMappingStateCleanupPending LocalMappingState = "cleanup_pending"
	LocalMappingStateInactive       LocalMappingState = "inactive"
)

type LocalMountState string

const (
	LocalMountStateDisabled      LocalMountState = "disabled"
	LocalMountStateConnecting    LocalMountState = "connecting"
	LocalMountStateConnected     LocalMountState = "connected"
	LocalMountStatePartial       LocalMountState = "partial"
	LocalMountStateFailed        LocalMountState = "failed"
	LocalMountStateDisconnecting LocalMountState = "disconnecting"
)

type LocalMountDeviceState string

const (
	LocalMountDeviceStatePending   LocalMountDeviceState = "pending"
	LocalMountDeviceStateConnected LocalMountDeviceState = "connected"
	LocalMountDeviceStateNotReady  LocalMountDeviceState = "not_ready"
	LocalMountDeviceStateFailed    LocalMountDeviceState = "failed"
	LocalMountDeviceStateRemoving  LocalMountDeviceState = "removing"
	LocalMountDeviceStateResidual  LocalMountDeviceState = "residual"
)

type LocalMountReasonCode string

const (
	LocalMountReasonLoopbackUnavailable LocalMountReasonCode = "loopback_unavailable"
	LocalMountReasonBackendNotReady     LocalMountReasonCode = "backend_not_ready"
	LocalMountReasonPoolUnavailable     LocalMountReasonCode = "pool_unavailable"
	LocalMountReasonIdentityConflict    LocalMountReasonCode = "identity_conflict"
	LocalMountReasonMappingConflict     LocalMountReasonCode = "mapping_conflict"
	LocalMountReasonLegacyPathConflict  LocalMountReasonCode = "legacy_local_path_conflict"
	LocalMountReasonDeviceNotEnumerated LocalMountReasonCode = "device_not_enumerated"
	LocalMountReasonOperationTimeout    LocalMountReasonCode = "operation_timeout"
	LocalMountReasonCleanupFailed       LocalMountReasonCode = "cleanup_failed"
	LocalMountReasonDeviceBusy          LocalMountReasonCode = "device_busy"
)

type LocalLoopbackLibraryMapping struct {
	LibraryID string `json:"libraryId"`
	TargetNAA string `json:"targetNaa"`
	NexusNAA  string `json:"nexusNaa"`
	TPGTag    int    `json:"tpgTag"`
}

var localLoopbackNAAPattern = regexp.MustCompile(`^naa\.5[0-9a-f]{15}$`)

func (m LocalLoopbackLibraryMapping) Validate() error {
	if ValidateManagementID(m.LibraryID) != nil ||
		!localLoopbackNAAPattern.MatchString(m.TargetNAA) ||
		!localLoopbackNAAPattern.MatchString(m.NexusNAA) ||
		m.TargetNAA == m.NexusNAA || m.TPGTag != 1 {
		return ErrInvalidInput
	}
	return nil
}

type LocalLoopbackDeviceMapping struct {
	DeviceKey   string            `json:"deviceKey"`
	LibraryID   string            `json:"libraryId"`
	Kind        LocalDeviceKind   `json:"kind"`
	DriveID     string            `json:"driveId,omitempty"`
	LUNIndex    int               `json:"lunIndex"`
	IdentityRef string            `json:"identityRef"`
	BackendRef  string            `json:"backendRef"`
	State       LocalMappingState `json:"state"`
}

func (m LocalLoopbackDeviceMapping) Validate() error {
	if ValidateManagementID(m.LibraryID) != nil ||
		ValidateManagementID(m.IdentityRef) != nil ||
		ValidateManagementID(m.BackendRef) != nil {
		return ErrInvalidInput
	}
	if m.State != LocalMappingStateActive && m.State != LocalMappingStateCleanupPending && m.State != LocalMappingStateInactive {
		return ErrInvalidInput
	}
	switch m.Kind {
	case LocalDeviceKindChanger:
		if ValidateManagementID(m.LibraryID) != nil || m.DeviceKey != ChangerDeviceKey(m.LibraryID) || m.DriveID != "" || m.LUNIndex != 0 {
			return ErrInvalidInput
		}
	case LocalDeviceKindDrive:
		if ValidateManagementID(m.DriveID) != nil || m.DeviceKey != DriveDeviceKey(m.DriveID) || m.LUNIndex < 1 || m.LUNIndex > 65535 {
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	return nil
}

func ChangerDeviceKey(libraryID string) string { return "changer:" + libraryID }

func DriveDeviceKey(driveID string) string { return "drive:" + driveID }

type VTLDeviceDescriptor struct {
	DeviceKey          string               `json:"deviceKey"`
	Kind               LocalDeviceKind      `json:"kind"`
	LibraryID          string               `json:"libraryId"`
	DisplayName        string               `json:"displayName"`
	DriveID            string               `json:"driveId,omitempty"`
	DriveIDs           []string             `json:"driveIds,omitempty"`
	Profile            string               `json:"profile"`
	DriveProfile       string               `json:"driveProfile,omitempty"`
	CompressionEnabled bool                 `json:"compressionEnabled"`
	DedupEnabled       bool                 `json:"dedupEnabled"`
	IdentityRef        string               `json:"identityRef"`
	BackendRef         string               `json:"backendRef"`
	BackendReady       bool                 `json:"backendReady"`
	ReadinessReason    LocalMountReasonCode `json:"readinessReason,omitempty"`
	LoadedCartridgeID  string               `json:"loadedCartridgeId,omitempty"`
	PoolID             string               `json:"poolId,omitempty"`
}

type LocalMountDeviceStatus struct {
	DeviceKey     string                `json:"deviceKey"`
	Kind          LocalDeviceKind       `json:"kind"`
	LibraryID     string                `json:"libraryId"`
	DriveID       string                `json:"driveId,omitempty"`
	DisplayName   string                `json:"displayName"`
	State         LocalMountDeviceState `json:"state"`
	ObservedPaths []string              `json:"observedPaths"`
	ReasonCode    LocalMountReasonCode  `json:"reasonCode,omitempty"`
	Message       string                `json:"message,omitempty"`
}

type LocalMountStatus struct {
	Enabled              bool                     `json:"enabled"`
	State                LocalMountState          `json:"state"`
	DesiredDeviceCount   int                      `json:"desiredDeviceCount"`
	ConnectedDeviceCount int                      `json:"connectedDeviceCount"`
	ResidualDeviceCount  int                      `json:"residualDeviceCount"`
	Devices              []LocalMountDeviceStatus `json:"devices"`
	LastSyncAt           *time.Time               `json:"lastSyncAt"`
	LastError            string                   `json:"lastError,omitempty"`
	DesiredIQNs          []string                 `json:"desiredIqns"`
	MountedIQNs          []string                 `json:"mountedIqns"`
	SkippedTargets       []string                 `json:"skippedTargets"`
}
