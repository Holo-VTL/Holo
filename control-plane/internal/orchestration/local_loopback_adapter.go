package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Holo-VTL/Holo/control-plane/internal/domain"
)

const (
	defaultLocalLoopbackHelperPath = "/opt/holo/bin/holo-local-loopback-helper"
	maxLocalLoopbackHelperPayload  = 64 * 1024
	localLoopbackHelperTimeout     = 10 * time.Second
)

var localLoopbackObservedPathPattern = regexp.MustCompile(`^/dev/(sg|st)[0-9]+$`)

var ErrLocalLoopbackHelperUnavailable = errors.New("local loopback helper unavailable")

type LocalLoopbackHelperError struct {
	ReasonCode string
}

func (e LocalLoopbackHelperError) Error() string {
	return "local loopback operation failed: " + e.ReasonCode
}

type LocalLoopbackDeviceObservation struct {
	DeviceKey     string                       `json:"deviceKey"`
	State         domain.LocalMountDeviceState `json:"state"`
	ObservedPaths []string                     `json:"observedPaths"`
	ReasonCode    domain.LocalMountReasonCode  `json:"reasonCode,omitempty"`
}

type LocalLoopbackOwner struct {
	LibraryID string                     `json:"libraryId"`
	TargetNAA string                     `json:"targetNaa"`
	NexusNAA  string                     `json:"nexusNaa"`
	TPGTag    int                        `json:"tpgTag"`
	Devices   []LocalLoopbackOwnedDevice `json:"devices"`
	Present   bool                       `json:"present"`
}

type LocalLoopbackOwnedDevice struct {
	DeviceKey   string                   `json:"deviceKey"`
	Kind        domain.LocalDeviceKind   `json:"kind"`
	DriveID     string                   `json:"driveId,omitempty"`
	LUN         int                      `json:"lun"`
	IdentityRef string                   `json:"identityRef"`
	BackendRef  string                   `json:"backendRef"`
	State       domain.LocalMappingState `json:"state"`
}

type localLoopbackHelperEnvelope struct {
	Version int             `json:"version"`
	OK      bool            `json:"ok"`
	Reason  string          `json:"reason,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

type localLoopbackRequestMapping struct {
	LibraryID string                       `json:"libraryId"`
	TargetNAA string                       `json:"targetNaa"`
	NexusNAA  string                       `json:"nexusNaa"`
	TPGTag    int                          `json:"tpgTag"`
	Devices   []localLoopbackRequestDevice `json:"devices"`
}

type localLoopbackRequestDevice struct {
	DeviceKey   string                   `json:"deviceKey"`
	Kind        domain.LocalDeviceKind   `json:"kind"`
	DriveID     string                   `json:"driveId,omitempty"`
	LUN         int                      `json:"lun"`
	IdentityRef string                   `json:"identityRef"`
	BackendRef  string                   `json:"backendRef"`
	State       domain.LocalMappingState `json:"state"`
}

type LocalLoopbackAdapter struct {
	binaryPath string
	useSudo    bool
	runner     iscsiSecurityHelperRunner
}

func NewLocalLoopbackAdapter(binaryPath string, useSudo bool) *LocalLoopbackAdapter {
	path := strings.TrimSpace(binaryPath)
	if configured := strings.TrimSpace(os.Getenv("HOLO_LOCAL_LOOPBACK_HELPER")); configured != "" {
		path = configured
	}
	if path == "" {
		path = defaultLocalLoopbackHelperPath
	}
	return &LocalLoopbackAdapter{
		binaryPath: path,
		useSudo:    useSudo,
		runner:     execISCSISecurityHelperRunner{useSudo: useSudo},
	}
}

func (a *LocalLoopbackAdapter) Probe(ctx context.Context) (bool, domain.LocalMountReasonCode, error) {
	result, err := a.call(ctx, "probe", nil)
	if err != nil {
		return false, "", err
	}
	var value struct {
		Available  bool                        `json:"available"`
		ReasonCode domain.LocalMountReasonCode `json:"reasonCode,omitempty"`
	}
	if err := decodeLocalLoopbackResult(result, &value); err != nil {
		return false, "", ErrLocalLoopbackHelperUnavailable
	}
	if !value.Available && value.ReasonCode != domain.LocalMountReasonLoopbackUnavailable {
		return false, "", ErrLocalLoopbackHelperUnavailable
	}
	return value.Available, value.ReasonCode, nil
}

func (a *LocalLoopbackAdapter) ListOwned(ctx context.Context) ([]LocalLoopbackOwner, error) {
	result, err := a.call(ctx, "list", nil)
	if err != nil {
		return nil, err
	}
	var owners []LocalLoopbackOwner
	if err := decodeLocalLoopbackResult(result, &owners); err != nil || owners == nil {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	return owners, nil
}

func (a *LocalLoopbackAdapter) Ensure(ctx context.Context, library domain.LocalLoopbackLibraryMapping, devices []domain.LocalLoopbackDeviceMapping) ([]LocalLoopbackDeviceObservation, error) {
	mapping, err := buildLocalLoopbackRequestMapping(library, devices, true)
	if err != nil {
		return nil, err
	}
	result, err := a.call(ctx, "ensure", &mapping)
	if err != nil {
		return nil, err
	}
	observed, err := decodeLocalLoopbackObservations(result)
	if err != nil || validateLocalLoopbackObservationSet(observed, devices) != nil {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	return observed, nil
}

func (a *LocalLoopbackAdapter) Remove(ctx context.Context, library domain.LocalLoopbackLibraryMapping, devices []domain.LocalLoopbackDeviceMapping) ([]LocalLoopbackDeviceObservation, error) {
	mapping, err := buildLocalLoopbackRequestMapping(library, devices, false)
	if err != nil {
		return nil, err
	}
	result, err := a.call(ctx, "remove", &mapping)
	if err != nil {
		return nil, err
	}
	observed, err := decodeLocalLoopbackObservations(result)
	if err != nil || validateLocalLoopbackObservationSet(observed, devices) != nil {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	return observed, nil
}

func (a *LocalLoopbackAdapter) call(ctx context.Context, operation string, mapping *localLoopbackRequestMapping) (json.RawMessage, error) {
	if a == nil || a.runner == nil || !validLocalLoopbackHelperPath(a.binaryPath) {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	if operation != "probe" && operation != "list" && operation != "ensure" && operation != "remove" {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	request := map[string]any{"version": 1, "operation": operation}
	if mapping != nil {
		request["mapping"] = mapping
	}
	payload, err := json.Marshal(request)
	if err != nil || len(payload) == 0 || len(payload) > maxLocalLoopbackHelperPayload {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, localLoopbackHelperTimeout)
	defer cancel()
	output, err := a.runner.Run(callCtx, a.binaryPath, payload)
	if err != nil || len(output) == 0 || len(output) > maxLocalLoopbackHelperPayload {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	var response localLoopbackHelperEnvelope
	if err := decoder.Decode(&response); err != nil || response.Version != 1 {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	if !response.OK {
		if !knownLocalLoopbackHelperReason(response.Reason) {
			return nil, ErrLocalLoopbackHelperUnavailable
		}
		return nil, LocalLoopbackHelperError{ReasonCode: response.Reason}
	}
	if len(response.Result) == 0 || !json.Valid(response.Result) {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	return response.Result, nil
}

func validLocalLoopbackHelperPath(value string) bool {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.Contains(value, "..") {
		return false
	}
	return true
}

func knownLocalLoopbackHelperReason(value string) bool {
	switch value {
	case "invalid_size", "invalid_json", "invalid_envelope", "invalid_schema", "unsupported_operation",
		"invalid_mapping", "mapping_conflict", "backend_not_ready", "device_busy", "operation_timeout",
		"cleanup_failed", "operation_failed", "loopback_unavailable":
		return true
	default:
		return false
	}
}

func buildLocalLoopbackRequestMapping(library domain.LocalLoopbackLibraryMapping, devices []domain.LocalLoopbackDeviceMapping, ensure bool) (localLoopbackRequestMapping, error) {
	if library.Validate() != nil {
		return localLoopbackRequestMapping{}, domain.ErrInvalidInput
	}
	if len(devices) > 5 {
		return localLoopbackRequestMapping{}, domain.ErrInvalidInput
	}
	seenLUNs := make(map[int]bool, len(devices))
	seenKeys := make(map[string]bool, len(devices))
	seenIdentity := make(map[string]bool, len(devices))
	seenBackend := make(map[string]bool, len(devices))
	changerCount := 0
	cleanupPending := false
	for _, device := range devices {
		if device.Validate() != nil || device.LibraryID != library.LibraryID || seenLUNs[device.LUNIndex] ||
			seenKeys[device.DeviceKey] || seenIdentity[device.IdentityRef] || seenBackend[device.BackendRef] {
			return localLoopbackRequestMapping{}, domain.ErrInvalidInput
		}
		if ensure && device.State != domain.LocalMappingStateActive {
			return localLoopbackRequestMapping{}, domain.ErrInvalidInput
		}
		if !ensure && device.State == domain.LocalMappingStateCleanupPending {
			cleanupPending = true
		}
		if device.Kind == domain.LocalDeviceKindChanger {
			changerCount++
		}
		seenLUNs[device.LUNIndex] = true
		seenKeys[device.DeviceKey] = true
		seenIdentity[device.IdentityRef] = true
		seenBackend[device.BackendRef] = true
	}
	if len(devices) > 0 && changerCount != 1 || !ensure && len(devices) > 0 && !cleanupPending {
		return localLoopbackRequestMapping{}, domain.ErrInvalidInput
	}
	requestDevices := make([]localLoopbackRequestDevice, 0, len(devices))
	for _, device := range devices {
		requestDevices = append(requestDevices, localLoopbackRequestDevice{
			DeviceKey:   device.DeviceKey,
			Kind:        device.Kind,
			DriveID:     device.DriveID,
			LUN:         device.LUNIndex,
			IdentityRef: device.IdentityRef,
			BackendRef:  device.BackendRef,
			State:       device.State,
		})
	}
	return localLoopbackRequestMapping{
		LibraryID: library.LibraryID,
		TargetNAA: library.TargetNAA,
		NexusNAA:  library.NexusNAA,
		TPGTag:    library.TPGTag,
		Devices:   requestDevices,
	}, nil
}

func decodeLocalLoopbackResult(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrLocalLoopbackHelperUnavailable
	}
	return nil
}

func decodeLocalLoopbackObservations(raw json.RawMessage) ([]LocalLoopbackDeviceObservation, error) {
	var observations []LocalLoopbackDeviceObservation
	if err := decodeLocalLoopbackResult(raw, &observations); err != nil || observations == nil {
		return nil, ErrLocalLoopbackHelperUnavailable
	}
	for _, observation := range observations {
		if observation.DeviceKey == "" || !knownLocalMountDeviceState(observation.State) || !knownLocalMountReason(observation.ReasonCode) {
			return nil, ErrLocalLoopbackHelperUnavailable
		}
		for _, path := range observation.ObservedPaths {
			if !localLoopbackObservedPathPattern.MatchString(path) {
				return nil, ErrLocalLoopbackHelperUnavailable
			}
		}
	}
	return observations, nil
}

func validateLocalLoopbackObservationSet(observations []LocalLoopbackDeviceObservation, devices []domain.LocalLoopbackDeviceMapping) error {
	if len(observations) != len(devices) {
		return ErrLocalLoopbackHelperUnavailable
	}
	want := make(map[string]bool, len(devices))
	for _, device := range devices {
		want[device.DeviceKey] = true
	}
	for _, observation := range observations {
		if !want[observation.DeviceKey] {
			return ErrLocalLoopbackHelperUnavailable
		}
		delete(want, observation.DeviceKey)
	}
	if len(want) != 0 {
		return ErrLocalLoopbackHelperUnavailable
	}
	return nil
}

func knownLocalMountDeviceState(state domain.LocalMountDeviceState) bool {
	switch state {
	case domain.LocalMountDeviceStatePending, domain.LocalMountDeviceStateConnected,
		domain.LocalMountDeviceStateNotReady, domain.LocalMountDeviceStateFailed,
		domain.LocalMountDeviceStateRemoving, domain.LocalMountDeviceStateResidual:
		return true
	default:
		return false
	}
}

func knownLocalMountReason(reason domain.LocalMountReasonCode) bool {
	if reason == "" {
		return true
	}
	switch reason {
	case domain.LocalMountReasonLoopbackUnavailable, domain.LocalMountReasonBackendNotReady,
		domain.LocalMountReasonPoolUnavailable, domain.LocalMountReasonIdentityConflict,
		domain.LocalMountReasonMappingConflict, domain.LocalMountReasonLegacyPathConflict,
		domain.LocalMountReasonDeviceNotEnumerated, domain.LocalMountReasonOperationTimeout,
		domain.LocalMountReasonCleanupFailed, domain.LocalMountReasonDeviceBusy:
		return true
	default:
		return false
	}
}
